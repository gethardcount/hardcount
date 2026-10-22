package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// One crediting rule for unused prepaid time (ADR-118 amendment 2026-10-22):
// a quantity decrease, downgrade, item removal, cancel or plan swap credits
// the invoice that charged the unused time, newest charging invoice first
// (Stripe: "based on the last price billed"). An item change only credits
// the invoices that charged that item: the cycle invoices and the item's own
// proration invoices.
//
// Driven over the real server and Postgres. Fixture: plan Team, $10 a seat a
// month, in advance, on the rig's 1c/call meter; add-on B, $40 a month, in
// advance, no meter. The subscription starts Sep 1, 2026 with 5 seats, so the
// period is [Sep 1, Oct 1): 30 days. No tax.
//
// Mutation seams (each against the real store):
//   - sort the candidates oldest first → T1 and T4 fail;
//   - drop the item filter → T3 fails (B's invoice is credited);
//   - cap an in-flight invoice like an unpaid one → T6 fails.

var (
	ocP0   = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	ocUp   = time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC) // 15 of 30 days left
	ocDown = time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) // 10 of 30 days left
)

type seatRig struct {
	*lagRig
	tenantID  string
	planTeam  string
	planAddOn string
	baseID    string // the invoice that billed the period's first base fee
}

// newSeatRig creates the Team plan and the add-on, and a subscription with
// seats Team seats starting at start (calendar billing).
func newSeatRig(t *testing.T, start time.Time, seats int) *seatRig {
	t.Helper()
	r := &seatRig{lagRig: newLagRigBare(t, "in_advance", 0)}
	r.planTeam = r.newPlan("team", "in_advance", 1000)
	addOn := r.must(http.MethodPost, "/v1/plans", lagP0, map[string]any{
		"code": "addon_b", "name": "addon_b", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 4000, "base_bill_timing": "in_advance", "meter_ids": []string{},
	}, http.StatusCreated)
	r.planAddOn = addOn["id"].(string)
	sub := r.must(http.MethodPost, "/v1/subscriptions", start, map[string]any{
		"code": "seat-sub", "display_name": "Seat Sub", "customer_id": r.custID, "billing_time": "calendar",
		"items": []map[string]any{{"plan_id": r.planTeam, "quantity": seats}}, "start_now": true,
	}, http.StatusCreated)
	r.subID = sub["id"].(string)
	r.itemA = r.sub().Items[0].ID
	r.tenantID = r.tenantOf()
	r.baseID = r.firstInvoiceID()
	return r
}

func (r *seatRig) tenantOf() string {
	r.t.Helper()
	tx, err := r.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var id string
	if err := tx.QueryRowContext(context.Background(), `SELECT tenant_id FROM subscriptions WHERE id = $1`, r.subID).Scan(&id); err != nil {
		r.t.Fatalf("tenant of subscription: %v", err)
	}
	return id
}

func (r *seatRig) pay(invID string, at time.Time) {
	r.t.Helper()
	r.must(http.MethodPost, "/v1/invoices/"+invID+"/record-payment", at, map[string]any{"note": "fixture"}, http.StatusOK)
}

func (r *seatRig) setSeats(n int, at time.Time) {
	r.t.Helper()
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, at, map[string]any{"quantity": n}, http.StatusOK)
}

// prorationInvoiceAt is the subscription_update invoice the change at `at` cut.
func (r *seatRig) prorationInvoiceAt(at time.Time) string {
	r.t.Helper()
	tx, err := r.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var id string
	if err := tx.QueryRowContext(context.Background(), `SELECT id FROM invoices WHERE subscription_id = $1
		AND billing_reason = 'subscription_update' AND source_plan_changed_at = $2`, r.subID, at).Scan(&id); err != nil {
		r.logInvoices(r.invoices())
		r.t.Fatalf("proration invoice of the change at %v: %v", at, err)
	}
	return id
}

func (r *seatRig) invoice(invID string) map[string]any {
	r.t.Helper()
	return flowObj(r.must(http.MethodGet, "/v1/invoices/"+invID, r.clk.Now(context.Background()), nil, http.StatusOK), "invoice")
}

// wantNotes checks the sum of the invoice's credit notes that are not voided.
func (r *seatRig) wantNotes(what, invID string, want int) {
	r.t.Helper()
	if got := r.creditNoteTotalOn(invID); got != want {
		r.t.Errorf("%s: credit notes sum to %dc, want %dc", what, got, want)
	}
}

// upgradeTo8 is the shared start: 5 seats, the base invoice paid, then 8
// seats on Sep 16, which cuts a $15.00 invoice (3 seats x $10 x 15/30).
func (r *seatRig) upgradeTo8(payUpgrade bool) string {
	r.t.Helper()
	r.pay(r.baseID, ocP0.Add(time.Hour))
	r.setSeats(8, ocUp)
	up := r.prorationInvoiceAt(ocUp)
	if got := flowInt(r.invoice(up), "total_amount_cents"); got != 1500 {
		r.t.Fatalf("upgrade invoice total = %dc, want 1500c", got)
	}
	if payUpgrade {
		r.pay(up, ocUp.Add(time.Hour))
	}
	return up
}

// T1: a seat decrease credits the newest charging invoice first. 8 → 3 seats
// on Sep 21 owes $16.67 (5 x $10 x 10/30): $15.00 on the upgrade invoice, the
// $1.67 rest on the base. A split by subtotal gave about $3.85 / $12.82.
func TestE2E_OneCreditRule_SeatDecreaseNewestFirst(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	up := r.upgradeTo8(true)

	r.setSeats(3, ocDown)
	r.wantNotes("upgrade invoice", up, 1500)
	r.wantNotes("base invoice", r.baseID, 167)
	if bal := r.creditBalance(); bal != 1667 {
		t.Errorf("balance = %dc, want 1667c", bal)
	}
}

// T2: as T1 with the upgrade invoice left unpaid. The decrease relieves it in
// full (it is the newest), so nothing is left due on it, and only the $1.67
// on the paid base reaches the balance. A split by subtotal left $11.15 due
// while $12.82 went to the balance.
func TestE2E_OneCreditRule_UnpaidNewestRelievedFirst(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	up := r.upgradeTo8(false)

	r.setSeats(3, ocDown)
	r.wantNotes("upgrade invoice", up, 1500)
	r.wantNotes("base invoice", r.baseID, 167)
	if due := flowInt(r.invoice(up), "amount_due_cents"); due != 0 {
		t.Errorf("upgrade invoice still has %dc due, want 0c", due)
	}
	if bal := r.creditBalance(); bal != 167 {
		t.Errorf("balance = %dc, want 167c", bal)
	}
}

// T3: an item change credits only the invoices that charged that item. Add-on
// B is added Sep 16 (a $20.00 invoice, paid); seat item A goes 5 → 3 on Sep
// 21, owing $6.67. It all lands on the base invoice; B's invoice, which never
// charged A, gets no note.
func TestE2E_OneCreditRule_ItemScopedCandidates(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	r.pay(r.baseID, ocP0.Add(time.Hour))
	r.must(http.MethodPost, "/v1/subscriptions/"+r.subID+"/items", ocUp, map[string]any{"plan_id": r.planAddOn, "quantity": 1}, http.StatusCreated)
	add := r.prorationInvoiceAt(ocUp)
	if got := flowInt(r.invoice(add), "total_amount_cents"); got != 2000 {
		t.Fatalf("add-on invoice total = %dc, want 2000c", got)
	}
	r.pay(add, ocUp.Add(time.Hour))

	r.setSeats(3, ocDown)
	r.wantNotes("base invoice", r.baseID, 667)
	r.wantNotes("add-on invoice", add, 0)
}

// T4: a cancel uses the same rule. From T1's state, a cancel on Sep 21 owes
// $26.67 (8 seats x $10 x 10/30): $15.00 on the upgrade invoice, $11.67 on the
// base. The unused-share weights gave $10.00 / $16.67.
//
// The variant bills the base on a renewal invoice that also carries $500 of
// August usage. A weight by subtotal sent nearly all of the credit to it; the
// split stays $15.00 / $11.67.
func TestE2E_OneCreditRule_CancelNewestFirst(t *testing.T) {
	skipShort(t)

	t.Run("base on the first invoice", func(t *testing.T) {
		r := newSeatRig(t, ocP0, 5)
		up := r.upgradeTo8(true)
		r.cancelNow(ocDown)
		r.wantNotes("upgrade invoice", up, 1500)
		r.wantNotes("base invoice", r.baseID, 1167)
	})

	t.Run("base on a renewal invoice with August usage", func(t *testing.T) {
		aug := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
		r := newSeatRig(t, aug, 5)
		r.pay(r.baseID, aug.Add(time.Hour))
		r.ingest(aug.AddDate(0, 0, 9), 50000)
		r.run(ocP0.Add(time.Minute))
		renewal := r.cycleInvoice(r.invoices(), ocP0)
		if renewal.subtotal != 55000 {
			r.logInvoices(r.invoices())
			t.Fatalf("renewal subtotal = %dc, want 55000c (Sep base 5000c + Aug usage 50000c)", renewal.subtotal)
		}
		r.baseID = renewal.id
		up := r.upgradeTo8(true)
		r.cancelNow(ocDown)
		r.wantNotes("upgrade invoice", up, 1500)
		r.wantNotes("renewal invoice", renewal.id, 1167)
	})
}

// chargeInFlight records a processing charge on the invoice through the
// writer the charge path and the webhook use.
func (r *seatRig) chargeInFlight(invID, pi string, at time.Time) {
	r.t.Helper()
	store := invoice.NewPostgresStore(r.db)
	ctx := uwCtx(at)
	inv, err := store.Get(ctx, r.tenantID, invID)
	if err != nil {
		r.t.Fatalf("read invoice: %v", err)
	}
	if _, applied, err := store.StampChargeOutcome(ctx, r.tenantID, invID, inv.ChargeAttemptSeq, domain.PaymentProcessing, pi, ""); err != nil || !applied {
		r.t.Fatalf("stamp processing: applied=%v err=%v", applied, err)
	}
}

// chargeFails records the PaymentIntent's failure, as the
// payment_intent.payment_failed webhook does.
func (r *seatRig) chargeFails(invID, pi string, at time.Time) {
	r.t.Helper()
	store := invoice.NewPostgresStore(r.db)
	_, res, err := store.MarkPaymentFailedReportingTransition(uwCtx(at), r.tenantID, invID,
		domain.PaymentFailureReport{PaymentIntentID: pi, Message: "Your account has insufficient funds."}, nil)
	if err != nil || !res.Recorded {
		r.t.Fatalf("record the failure: %+v err=%v", res, err)
	}
}

// T5: the base charge is a bank debit, still processing on Sep 16, so the
// upgrade goes through and is paid. The debit then fails. The Sep 21 decrease
// credits the upgrade invoice first ($15.00 to the balance) and relieves the
// $1.67 rest on the now-unpaid base. Before, it relieved only the base.
func TestE2E_OneCreditRule_AsyncBaseThenFailedCharge(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	r.chargeInFlight(r.baseID, "pi_t5_base", ocP0.Add(time.Hour))
	r.setSeats(8, ocUp)
	up := r.prorationInvoiceAt(ocUp)
	r.pay(up, ocUp.Add(time.Hour))
	r.chargeFails(r.baseID, "pi_t5_base", ocUp.Add(48*time.Hour))
	if ps := flowStr(r.invoice(r.baseID), "payment_status"); ps != string(domain.PaymentFailed) {
		t.Fatalf("base payment_status = %s, want failed", ps)
	}

	r.setSeats(3, ocDown)
	r.wantNotes("upgrade invoice", up, 1500)
	r.wantNotes("base invoice", r.baseID, 167)
	if due := flowInt(r.invoice(r.baseID), "amount_due_cents"); due != 5000-167 {
		t.Errorf("base invoice has %dc due, want %dc", due, 5000-167)
	}
}

// T6: a cancel while the base charge is in flight drafts a note sized off what
// the invoice can still be credited (total − credited), and Issue holds it
// until the charge settles. Cancel on Sep 16: $25.00 of unused time.
func TestE2E_OneCreditRule_InFlightCancelDrafts(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	r.chargeInFlight(r.baseID, "pi_t6_base", ocP0.Add(time.Hour))

	r.cancelNow(ocUp)
	r.wantNotes("base invoice", r.baseID, 2500)
	if n := flowCount(r.t, r.db, `SELECT count(*) FROM credit_notes WHERE invoice_id = $1 AND status = 'draft' AND issue_pending`, r.baseID); n != 1 {
		t.Errorf("%d held drafts on the in-flight base, want 1", n)
	}
	if due := flowInt(r.invoice(r.baseID), "amount_due_cents"); due != 5000 {
		t.Errorf("base invoice has %dc due, want 5000c until the charge settles", due)
	}
}

// T7: what does not fit is recorded on the change, not only logged. The base
// ($50.00, paid) already has a $40.00 operator note, so a cancel on Sep 16,
// which owes $25.00, credits the $10.00 left. The change succeeds, and its
// audit row says $15.00 was not credited.
func TestE2E_OneCreditRule_RemainderRecorded(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	r.pay(r.baseID, ocP0.Add(time.Hour))
	r.creditNote(r.baseID, 4000, ocP0.Add(2*time.Hour))

	r.cancelNow(ocUp)
	r.wantNotes("base invoice", r.baseID, 5000)
	if n := r.creditNotesOn(r.baseID); n != 2 {
		t.Errorf("%d notes on the base, want 2 (the operator's and a 1000c relief)", n)
	}
	if got := r.auditInt("cancel", "uncredited_cents"); got != 1500 {
		t.Errorf("cancel audit uncredited_cents = %d, want 1500", got)
	}
}

// auditInt reads one integer field of the subscription's audit row for action.
func (r *seatRig) auditInt(action, key string) int64 {
	r.t.Helper()
	tx, err := r.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var raw []byte
	if err := tx.QueryRowContext(context.Background(), `SELECT metadata FROM audit_log
		WHERE resource_type = 'subscription' AND resource_id = $1 AND action = $2
		ORDER BY created_at DESC LIMIT 1`, r.subID, action).Scan(&raw); err != nil {
		r.t.Fatalf("audit row %s: %v", action, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		r.t.Fatalf("audit metadata: %v", err)
	}
	f, _ := meta[key].(float64)
	return int64(f)
}

// T8: two seat decreases at once. Each is applied in full or refused, and no
// invoice is credited past its total. Applied in either order, 8 → 4 and the
// other to 2 credit 6 seats x $10 x 10/30 in all, within a cent per change.
func TestE2E_OneCreditRule_ConcurrentDecreases(t *testing.T) {
	skipShort(t)
	r := newSeatRig(t, ocP0, 5)
	up := r.upgradeTo8(true)

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i, n := range []int{4, 2} {
		wg.Add(1)
		go func(i, n int) {
			defer wg.Done()
			code, raw := r.do(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, ocDown, map[string]any{"quantity": n})
			codes[i] = code
			if code != http.StatusOK {
				t.Logf("decrease to %d: %d %s", n, code, raw)
			}
		}(i, n)
	}
	wg.Wait()

	for _, id := range []string{up, r.baseID} {
		total := int(flowInt(r.invoice(id), "total_amount_cents"))
		if got := r.creditNoteTotalOn(id); got > total {
			t.Errorf("invoice %s credited %dc, past its total %dc", id, got, total)
		}
	}
	seats := r.sub().Items[0].Quantity
	credited := r.creditNoteTotalOn(up) + r.creditNoteTotalOn(r.baseID)
	want := int(8-seats) * 1000 * 10 / 30
	if d := credited - want; d < -2 || d > 2 {
		t.Errorf("credited %dc for 8 → %d seats, want %dc (codes %v)", credited, seats, want, codes)
	}
	for i, c := range codes {
		if c != http.StatusOK && c != http.StatusConflict {
			t.Errorf("request %d answered %d, want 200 or a clean 409", i, c)
		}
	}
}
