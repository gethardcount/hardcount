package credit_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// ADR-132: credit an invoice drew from grants that is handed back, by a void
// or by a credit note, goes back to the grants it came from, with their
// expiry and kind. Before it, the return was a new grant with no expiry and
// no kind: credit due to expire next week, or promotional credit, came back
// as never-expiring paid-class credit.

type returnRig struct {
	t        *testing.T
	db       *postgres.DB
	ctx      context.Context
	tenantID string
	store    *credit.PostgresStore
	svc      *credit.Service
	invoices *invoice.PostgresStore
}

func newReturnRig(t *testing.T, name string) *returnRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	store := credit.NewPostgresStore(db)
	store.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	return &returnRig{
		t: t, db: db, ctx: postgres.WithLivemode(context.Background(), false),
		tenantID: testutil.CreateTestTenant(t, db, name),
		store:    store, svc: credit.NewService(store), invoices: invoice.NewPostgresStore(db),
	}
}

func (r *returnRig) customer(name string) string {
	r.t.Helper()
	return holdCustomer(r.t, r.ctx, r.db, r.tenantID, name)
}

func (r *returnRig) grant(cust string, cents int64, expires *time.Time, kind domain.GrantKind) string {
	r.t.Helper()
	g, err := r.svc.Grant(r.ctx, r.tenantID, credit.GrantInput{
		Currency: "USD", CustomerID: cust, AmountCents: cents, Description: "seed", ExpiresAt: expires, GrantKind: kind,
	})
	if err != nil {
		r.t.Fatalf("grant: %v", err)
	}
	return g.ID
}

func (r *returnRig) invoice(cust string, cents int64) string {
	r.t.Helper()
	return createOpenInvoice(r.t, r.ctx, r.invoices, r.tenantID, cust, cents).ID
}

func (r *returnRig) apply(cust, inv string, cents int64) {
	r.t.Helper()
	got, err := r.svc.ApplyToInvoice(r.ctx, r.tenantID, cust, inv, cents)
	if err != nil || got != cents {
		r.t.Fatalf("apply %d to %s: got %d, err %v", cents, inv, got, err)
	}
}

func (r *returnRig) void(ctx context.Context, cust, inv string) domain.CreditReturn {
	r.t.Helper()
	tx, err := r.db.BeginTx(ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	res, err := r.svc.ReverseForInvoiceTx(ctx, tx, r.tenantID, cust, inv, "")
	if err != nil {
		r.t.Fatalf("void reversal: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
	return res
}

func (r *returnRig) creditNote(cust, inv, cnID string, cents int64, paid bool) (domain.CreditReturn, error) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	res, err := r.svc.GrantForCreditNoteTx(r.ctx, tx, r.tenantID, cnID, credit.CreditNoteReturnInput{
		CustomerID: cust, Currency: "USD", AmountCents: cents, Description: "Credit note " + cnID, InvoiceID: inv, InvoicePaid: paid,
	})
	if err != nil {
		return domain.CreditReturn{}, err
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit: %v", err)
	}
	return res, nil
}

func (r *returnRig) backdate(blockID string, at time.Time) {
	r.t.Helper()
	bypassExec(r.t, r.db, r.ctx, `UPDATE customer_credit_ledger SET expires_at = $1 WHERE id = $2`, at, blockID)
}

type blockRow struct {
	Amount, Consumed int64
	ExpiresAt        *time.Time
	Kind             string
}

func (r *returnRig) block(id string) blockRow {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var b blockRow
	if err := tx.QueryRowContext(r.ctx, `SELECT amount_cents, consumed_cents, expires_at, COALESCE(grant_kind, '') FROM customer_credit_ledger WHERE id = $1`, id).
		Scan(&b.Amount, &b.Consumed, &b.ExpiresAt, &b.Kind); err != nil {
		r.t.Fatalf("read block %s: %v", id, err)
	}
	return b
}

// returnRow reads the one return row of a source.
func (r *returnRig) returnRow(where string, arg string) (amount, consumed int64, meta map[string]any) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var raw []byte
	if err := tx.QueryRowContext(r.ctx, `SELECT amount_cents, consumed_cents, metadata FROM customer_credit_ledger WHERE `+where+` = $1`, arg).
		Scan(&amount, &consumed, &raw); err != nil {
		r.t.Fatalf("read return row (%s = %s): %v", where, arg, err)
	}
	_ = json.Unmarshal(raw, &meta)
	return amount, consumed, meta
}

func (r *returnRig) balance(cust string) int64 {
	r.t.Helper()
	sum, _, _ := customerLedgerFacts(r.t, r.db, r.ctx, cust)
	return sum
}

type expiryRow struct {
	Amount    int64
	CreatedAt time.Time
}

func (r *returnRig) expiries(cust, grantID string) []expiryRow {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx, `SELECT amount_cents, created_at FROM customer_credit_ledger
		WHERE customer_id = $1 AND entry_type = 'expiry' AND description = 'Expired grant ' || $2 ORDER BY created_at, id`, cust, grantID)
	if err != nil {
		r.t.Fatalf("read expiries: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []expiryRow
	for rows.Next() {
		var e expiryRow
		if err := rows.Scan(&e.Amount, &e.CreatedAt); err != nil {
			r.t.Fatalf("scan expiry: %v", err)
		}
		out = append(out, e)
	}
	return out
}

// checkLedger asserts the two ledger invariants a return must keep, the same
// ones hardcount-doctor checks: the balance equals the open capacity of the
// customer's blocks (balance_exceeds_block_capacity, both directions), and no
// grant the sweep wrote off has headroom again (expired_grant_headroom_reopened).
func (r *returnRig) checkLedger(cust string) {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var balance, capacity int64
	if err := tx.QueryRowContext(r.ctx, `
		SELECT COALESCE(SUM(amount_cents), 0),
		       COALESCE(SUM(CASE WHEN amount_cents > 0 THEN amount_cents - consumed_cents ELSE 0 END), 0)
		FROM customer_credit_ledger WHERE customer_id = $1`, cust).Scan(&balance, &capacity); err != nil {
		r.t.Fatalf("read capacity: %v", err)
	}
	if balance != capacity {
		r.t.Errorf("balance %d != block capacity %d", balance, capacity)
	}
	var reopened int
	if err := tx.QueryRowContext(r.ctx, `
		SELECT COUNT(*) FROM customer_credit_ledger g
		WHERE g.customer_id = $1 AND g.entry_type = 'grant' AND g.consumed_cents < g.amount_cents
		  AND EXISTS (SELECT 1 FROM customer_credit_ledger e WHERE e.customer_id = g.customer_id
		              AND e.entry_type = 'expiry' AND e.description = 'Expired grant ' || g.id)`, cust).Scan(&reopened); err != nil {
		r.t.Fatalf("read reopened grants: %v", err)
	}
	if reopened != 0 {
		r.t.Errorf("%d expired grants have headroom again", reopened)
	}
}

func (r *returnRig) summaries(cust string, history bool) credit.GrantsResponse {
	r.t.Helper()
	resp, err := r.svc.ListGrants(r.ctx, r.tenantID, cust, history)
	if err != nil {
		r.t.Fatalf("list grants: %v", err)
	}
	return resp
}

func findSummary(resp credit.GrantsResponse, id string) (credit.GrantSummary, bool) {
	for _, g := range resp.Grants {
		if g.ID == id {
			return g, true
		}
	}
	return credit.GrantSummary{}, false
}

func (r *returnRig) events(cust, eventType string) int {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM webhook_outbox
		WHERE tenant_id = $1 AND event_type = $2 AND payload->'credit_balance'->>'customer_id' = $3`,
		r.tenantID, eventType, cust).Scan(&n); err != nil {
		r.t.Fatalf("count events: %v", err)
	}
	return n
}

// A void returns credit to a grant that expires next week, with that expiry,
// and to the paid grant the invoice drew last. Fails before ADR-132: the
// expiring grant stayed consumed and the credit came back as a new grant with
// no expiry.
func TestCreditReturn_VoidRestoresTheExpiringGrant(t *testing.T) {
	r := newReturnRig(t, "Return Expiring")
	cust := r.customer("ret_expiring")
	nextWeek := time.Now().UTC().Add(7 * 24 * time.Hour).Truncate(time.Microsecond)
	soon := r.grant(cust, 5000, &nextWeek, "")
	paid := r.grant(cust, 5000, nil, "")
	inv := r.invoice(cust, 6000)
	r.apply(cust, inv, 6000) // soonest-expiring first: soon 5000, then paid 1000

	res := r.void(r.ctx, cust, inv)

	if res != (domain.CreditReturn{ReturnedCents: 6000, RestoredCents: 6000}) {
		t.Errorf("return = %+v, want 6000 returned, all restored", res)
	}
	if b := r.block(soon); b.Consumed != 0 || b.ExpiresAt == nil || !b.ExpiresAt.Equal(nextWeek) {
		t.Errorf("expiring grant: consumed %d, expires %v; want 0 and %v", b.Consumed, b.ExpiresAt, nextWeek)
	}
	if b := r.block(paid); b.Consumed != 0 {
		t.Errorf("paid grant consumed %d, want 0", b.Consumed)
	}
	amount, consumed, meta := r.returnRow("source_invoice_reversal_id", inv)
	if amount != 6000 || consumed != 6000 {
		t.Errorf("reversal row: amount %d consumed %d, want 6000 6000 (no capacity of its own)", amount, consumed)
	}
	legs, _ := meta["restored_blocks"].([]any)
	if len(legs) != 2 || legs[0].(map[string]any)["block_id"] != paid || legs[1].(map[string]any)["block_id"] != soon {
		t.Errorf("restored_blocks = %v, want the paid grant first (drawn last), then the expiring one", meta["restored_blocks"])
	}
	if got := r.balance(cust); got != 10000 {
		t.Errorf("balance %d, want 10000", got)
	}
	r.checkLedger(cust)

	// The grant summary shows both grants whole again, and the reversal row
	// is not a block: everything it returned is on the grants.
	for _, history := range []bool{false, true} {
		resp := r.summaries(cust, history)
		if _, ok := findSummary(resp, soon); !ok {
			t.Errorf("history=%v: expiring grant missing", history)
		}
		if len(resp.Grants) != 2 {
			t.Errorf("history=%v: %d grant rows, want the 2 grants only", history, len(resp.Grants))
		}
		for _, g := range resp.Grants {
			if g.RemainingCents != 5000 || g.ConsumedCents != 0 {
				t.Errorf("history=%v: grant %s remaining %d consumed %d, want 5000 0", history, g.ID, g.RemainingCents, g.ConsumedCents)
			}
		}
	}
}

// A void returns promotional credit as promotional: it drains first again,
// and the kind subtotal shows it.
func TestCreditReturn_VoidKeepsThePromotionalKind(t *testing.T) {
	r := newReturnRig(t, "Return Promotional")
	cust := r.customer("ret_promo")
	promo := r.grant(cust, 3000, nil, domain.GrantKindPromotional)
	r.grant(cust, 5000, nil, "")
	inv := r.invoice(cust, 4000)
	r.apply(cust, inv, 4000) // promotional first: 3000, then 1000 paid

	r.void(r.ctx, cust, inv)

	if b := r.block(promo); b.Consumed != 0 || b.Kind != string(domain.GrantKindPromotional) {
		t.Errorf("promotional grant: consumed %d kind %q, want 0 promotional", b.Consumed, b.Kind)
	}
	resp := r.summaries(cust, false)
	if len(resp.RemainingByCurrency) != 1 {
		t.Fatalf("remaining by currency = %+v", resp.RemainingByCurrency)
	}
	if got := resp.RemainingByCurrency[0]; got.PromotionalRemainingCents != 3000 || got.OtherRemainingCents != 5000 {
		t.Errorf("remaining = %+v, want promotional 3000, other 5000 (before ADR-132 the 4000 returned was all other)", got)
	}
	r.checkLedger(cust)

	// It drains first again.
	next := r.invoice(cust, 1000)
	r.apply(cust, next, 1000)
	if b := r.block(promo); b.Consumed != 1000 {
		t.Errorf("next draw took %d from the promotional grant, want 1000", b.Consumed)
	}
}

// A share whose grant is already past its expiry expires at once, by the
// sweep's rule (Stripe: past expiry, the reinstated balance "expires
// immediately"), and the net balance change fires no crossing webhook.
//
// Mutation-verify (CI, PR #1498): skip the at-once expiry → the three
// expiring cases keep live credit on an expired grant (balance, expiry
// entries and checkLedger red); emit the return row's crossings on its own
// insert → "fully drawn" enqueues a balance_recovered (events red); drop the
// unswept-remainder write-off → "unswept remainder" has no -2000 entry at
// expires_at (red).
func TestCreditReturn_PastExpiryExpiresAtOnce(t *testing.T) {
	r := newReturnRig(t, "Return Past Expiry")
	later := time.Now().UTC().Add(time.Hour)

	t.Run("fully drawn, nothing left to sweep", func(t *testing.T) {
		r.t = t
		cust := r.customer("ret_exp_full")
		g := r.grant(cust, 5000, &later, "")
		inv := r.invoice(cust, 5000)
		r.apply(cust, inv, 5000)
		expired := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		r.backdate(g, expired)
		crossings := func() int {
			return r.events(cust, domain.EventCreditBalanceRecovered) + r.events(cust, domain.EventCreditBalanceDepleted)
		}
		crossingsBefore := crossings()

		start := time.Now().UTC()
		res := r.void(r.ctx, cust, inv)

		if res.ReturnedCents != 5000 || res.RestoredCents != 5000 || res.ExpiredOnReturnCents != 5000 {
			t.Errorf("return = %+v, want 5000 restored and expired at once", res)
		}
		if got := r.balance(cust); got != 0 {
			t.Errorf("balance %d, want 0: the credit would have expired had the invoice never drawn it", got)
		}
		ex := r.expiries(cust, g)
		if len(ex) != 1 || ex[0].Amount != -5000 || ex[0].CreatedAt.Before(start.Add(-time.Second)) {
			t.Errorf("expiry entries = %+v, want one -5000 stamped at the return", ex)
		}
		if b := r.block(g); b.Consumed != 5000 {
			t.Errorf("grant consumed %d, want 5000", b.Consumed)
		}
		if n := crossings() - crossingsBefore; n != 0 {
			t.Errorf("the return enqueued %d balance crossing events, want 0: the balance went 0 -> 0", n)
		}
		resp := r.summaries(cust, true)
		if s, ok := findSummary(resp, g); !ok || s.ExpiredCents != 5000 || s.RemainingCents != 0 {
			t.Errorf("grant summary = %+v (found %v), want expired 5000, remaining 0", s, ok)
		}
		r.checkLedger(cust)
	})

	t.Run("swept: retirement wins", func(t *testing.T) {
		r.t = t
		cust := r.customer("ret_exp_swept")
		g := r.grant(cust, 5000, &later, "")
		inv := r.invoice(cust, 3000)
		r.apply(cust, inv, 3000)
		r.backdate(g, time.Now().UTC().Add(-time.Hour))
		if n, err := r.store.ExpireGrantAtomic(r.ctx, r.tenantID, cust, g); err != nil || n != 2000 {
			t.Fatalf("sweep: %d, %v; want 2000", n, err)
		}

		res := r.void(r.ctx, cust, inv)

		if res.ExpiredOnReturnCents != 3000 {
			t.Errorf("expired on return %d, want 3000", res.ExpiredOnReturnCents)
		}
		if got := r.balance(cust); got != 0 {
			t.Errorf("balance %d, want 0", got)
		}
		if ex := r.expiries(cust, g); len(ex) != 2 || ex[0].Amount != -2000 || ex[1].Amount != -3000 {
			t.Errorf("expiry entries = %+v, want the sweep's -2000 then the return's -3000", ex)
		}
		resp := r.summaries(cust, true)
		if s, _ := findSummary(resp, g); s.ExpiredCents != 5000 || s.ConsumedCents != 5000 {
			t.Errorf("grant summary = %+v, want expired 5000 of 5000", s)
		}
		r.checkLedger(cust)
	})

	t.Run("unswept remainder is written off as the sweep would", func(t *testing.T) {
		r.t = t
		cust := r.customer("ret_exp_unswept")
		g := r.grant(cust, 5000, &later, "")
		inv := r.invoice(cust, 3000)
		r.apply(cust, inv, 3000)
		expiredAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		r.backdate(g, expiredAt)

		res := r.void(r.ctx, cust, inv)

		if res.ExpiredOnReturnCents != 3000 {
			t.Errorf("expired on return %d, want 3000 (the remainder's 2000 is the sweep's, not the return's)", res.ExpiredOnReturnCents)
		}
		ex := r.expiries(cust, g)
		if len(ex) != 2 || ex[0].Amount != -2000 || !ex[0].CreatedAt.Equal(expiredAt) || ex[1].Amount != -3000 {
			t.Errorf("expiry entries = %+v, want -2000 stamped at expires_at %v, then -3000", ex, expiredAt)
		}
		if got := r.balance(cust); got != 0 {
			t.Errorf("balance %d, want 0", got)
		}
		r.checkLedger(cust)
	})

	t.Run("held by an owed period invoice: stays live", func(t *testing.T) {
		r.t = t
		cust := r.customer("ret_exp_held")
		g := r.grant(cust, 5000, &later, "")
		inv := r.invoice(cust, 3000)
		r.apply(cust, inv, 3000)
		expiredAt := time.Now().UTC().Add(-time.Hour)
		r.backdate(g, expiredAt)
		// A period invoice issued before the grant expired, still a draft:
		// its collection may draw on the grant, so the sweep holds it.
		seedPeriodInvoice(t, r.ctx, r.invoices, r.tenantID, cust, "", domain.BillingReasonSubscriptionCycle,
			domain.InvoiceDraft, domain.InvoiceTaxOK, expiredAt.Add(-time.Hour))

		res := r.void(r.ctx, cust, inv)

		if res.ExpiredOnReturnCents != 0 || res.RestoredCents != 3000 {
			t.Errorf("return = %+v, want 3000 restored, none expired", res)
		}
		if b := r.block(g); b.Consumed != 0 {
			t.Errorf("grant consumed %d, want 0: the owed draw may still use it", b.Consumed)
		}
		if ex := r.expiries(cust, g); len(ex) != 0 {
			t.Errorf("expiry entries = %+v, want none while the draw is owed", ex)
		}
		r.checkLedger(cust)
	})
}

// A partial credit note on an unpaid invoice restores the last-drawn block
// first; the void then returns the rest to the block drawn first.
func TestCreditReturn_PartialCreditNoteRestoresLastDrawnFirst(t *testing.T) {
	r := newReturnRig(t, "Return Partial CN")
	cust := r.customer("ret_partial")
	nextWeek := time.Now().UTC().Add(7 * 24 * time.Hour)
	first := r.grant(cust, 3000, &nextWeek, "")
	second := r.grant(cust, 3000, nil, "")
	inv := r.invoice(cust, 5000)
	r.apply(cust, inv, 5000) // first 3000, second 2000

	res, err := r.creditNote(cust, inv, "hc_cn_partial", 2500, false)
	if err != nil {
		t.Fatalf("credit note: %v", err)
	}
	if res.RestoredCents != 2500 {
		t.Errorf("credit note restored %d, want 2500", res.RestoredCents)
	}
	if a, b := r.block(first).Consumed, r.block(second).Consumed; a != 2500 || b != 0 {
		t.Errorf("after the note: first consumed %d, second %d; want 2500 0 (second drawn last, restored first)", a, b)
	}
	r.checkLedger(cust)

	vres := r.void(r.ctx, cust, inv)
	if vres.ReturnedCents != 2500 || vres.RestoredCents != 2500 {
		t.Errorf("void = %+v, want the remaining 2500 restored", vres)
	}
	if a, b := r.block(first).Consumed, r.block(second).Consumed; a != 0 || b != 0 {
		t.Errorf("after the void: first consumed %d, second %d; want 0 0", a, b)
	}
	if got := r.balance(cust); got != 6000 {
		t.Errorf("balance %d, want 6000", got)
	}
	r.checkLedger(cust)
}

// A credit note on a paid invoice restores only the part the invoice drew
// from grants; the cash-paid part is new credit with no expiry, as before.
func TestCreditReturn_PaidInvoiceRestoresOnlyTheDrawnPart(t *testing.T) {
	r := newReturnRig(t, "Return Paid CN")
	cust := r.customer("ret_paid")
	nextWeek := time.Now().UTC().Add(7 * 24 * time.Hour)
	promo := r.grant(cust, 3000, &nextWeek, domain.GrantKindPromotional)
	inv := r.invoice(cust, 10000)
	r.apply(cust, inv, 3000) // credit covers 3000; the card paid the other 7000

	res, err := r.creditNote(cust, inv, "hc_cn_paid_1", 5000, true)
	if err != nil {
		t.Fatalf("credit note: %v", err)
	}
	if res != (domain.CreditReturn{ReturnedCents: 5000, RestoredCents: 3000, UnattributedCents: 2000}) {
		t.Errorf("return = %+v, want 3000 back to the grant and 2000 new credit", res)
	}
	if b := r.block(promo); b.Consumed != 0 || b.Kind != string(domain.GrantKindPromotional) || b.ExpiresAt == nil {
		t.Errorf("promotional grant = %+v, want whole again with its kind and expiry", b)
	}
	amount, consumed, _ := r.returnRow("source_credit_note_id", "hc_cn_paid_1")
	if amount != 5000 || consumed != 3000 {
		t.Errorf("credit note row: amount %d consumed %d, want 5000 3000", amount, consumed)
	}
	resp := r.summaries(cust, false)
	if got := resp.RemainingByCurrency[0]; got.PromotionalRemainingCents != 3000 || got.OtherRemainingCents != 2000 {
		t.Errorf("remaining = %+v, want promotional 3000, other 2000", got)
	}
	if len(resp.Grants) != 2 {
		t.Errorf("%d grant rows, want the promotional grant and the note's 2000", len(resp.Grants))
	}
	for _, g := range resp.Grants {
		if g.ID != promo && (g.AmountCents != 2000 || g.ConsumedCents != 0 || g.RemainingCents != 2000) {
			t.Errorf("note row summary = %+v, want amount 2000, consumed 0, remaining 2000", g)
		}
	}
	r.checkLedger(cust)

	// Nothing drawn is left on the invoice: a second note is all new credit.
	res, err = r.creditNote(cust, inv, "hc_cn_paid_2", 4000, true)
	if err != nil {
		t.Fatalf("second credit note: %v", err)
	}
	if res != (domain.CreditReturn{ReturnedCents: 4000, UnattributedCents: 4000}) {
		t.Errorf("second return = %+v, want all 4000 new credit", res)
	}
	if got := r.balance(cust); got != 9000 {
		t.Errorf("balance %d, want 9000 (3000 granted, 3000 drawn, 5000 + 4000 back)", got)
	}
	r.checkLedger(cust)
}

// A usage row written before drains recorded their blocks (2026-07-05) has no
// attribution: its part goes back as new credit with no expiry, as before,
// and the rest goes back to its blocks. What is handed back as new credit is
// (still applied) - (attributed outstanding).
func TestCreditReturn_LegacyRowsTakeTheLegacyPath(t *testing.T) {
	r := newReturnRig(t, "Return Legacy")
	legacyUsage := func(cust, g, inv string, cents int64) {
		bypassExec(r.t, r.db, r.ctx, `INSERT INTO customer_credit_ledger
				(id, tenant_id, customer_id, entry_type, currency, amount_cents, balance_after, description, invoice_id, metadata)
			VALUES ($1, $2, $3, 'usage', 'USD', $4, 0, 'legacy draw', $5, '{}')`,
			postgres.NewID("hc_ccl"), r.tenantID, cust, -cents, inv)
		bypassExec(r.t, r.db, r.ctx, `UPDATE customer_credit_ledger SET consumed_cents = consumed_cents + $1 WHERE id = $2`, cents, g)
	}

	t.Run("void", func(t *testing.T) {
		r.t = t
		cust := r.customer("ret_legacy_void")
		g := r.grant(cust, 10000, nil, "")
		inv := r.invoice(cust, 3000)
		legacyUsage(cust, g, inv, 4000)
		r.apply(cust, inv, 3000)

		res := r.void(r.ctx, cust, inv)

		if res != (domain.CreditReturn{ReturnedCents: 7000, RestoredCents: 3000, UnattributedCents: 4000}) {
			t.Errorf("return = %+v, want 3000 back to the grant and 4000 new credit", res)
		}
		if b := r.block(g); b.Consumed != 4000 {
			t.Errorf("grant consumed %d, want 4000 (the legacy draw stays on it)", b.Consumed)
		}
		if amount, consumed, _ := r.returnRow("source_invoice_reversal_id", inv); amount != 7000 || consumed != 3000 {
			t.Errorf("reversal row: amount %d consumed %d, want 7000 3000", amount, consumed)
		}
		if got := r.balance(cust); got != 10000 {
			t.Errorf("balance %d, want 10000", got)
		}
		r.checkLedger(cust)
	})

	t.Run("void after a credit note from before ADR-132", func(t *testing.T) {
		r.t = t
		cust := r.customer("ret_legacy_cn")
		g := r.grant(cust, 10000, nil, "")
		inv := r.invoice(cust, 3000)
		legacyUsage(cust, g, inv, 4000)
		r.apply(cust, inv, 3000)
		// The old credit note returned 1000 as a new grant, with no record.
		bypassExec(t, r.db, r.ctx, `INSERT INTO customer_credit_ledger
				(id, tenant_id, customer_id, entry_type, currency, amount_cents, balance_after, description, invoice_id, source_credit_note_id, metadata)
			VALUES ($1, $2, $3, 'grant', 'USD', 1000, 0, 'old credit note', $4, $5, '{}')`,
			postgres.NewID("hc_ccl"), r.tenantID, cust, inv, postgres.NewID("hc_cn"))

		res := r.void(r.ctx, cust, inv)

		// Still applied 7000 - 1000 = 6000; attributed outstanding 3000.
		if res != (domain.CreditReturn{ReturnedCents: 6000, RestoredCents: 3000, UnattributedCents: 3000}) {
			t.Errorf("return = %+v, want 3000 restored and 3000 new credit", res)
		}
		r.checkLedger(cust)
	})

	t.Run("more than was drawn, with no legacy rows: refused", func(t *testing.T) {
		r.t = t
		cust := r.customer("ret_legacy_refused")
		r.grant(cust, 5000, nil, "")
		inv := r.invoice(cust, 3000)
		r.apply(cust, inv, 3000)
		before := r.balance(cust)

		if _, err := r.creditNote(cust, inv, "hc_cn_too_much", 4000, false); err == nil {
			t.Fatal("a credit note returning 4000 of 3000 drawn: accepted, want a refusal")
		}
		if got := r.balance(cust); got != before {
			t.Errorf("balance %d after the refusal, want %d", got, before)
		}
	})
}

// A test-clock customer's return judges expiry on the customer's clock and
// stamps there. The grant is live on the wall clock but past expiry on the
// customer's clock, so its share expires at once, at the clock's instant.
func TestCreditReturn_TestClockCustomerUsesItsClock(t *testing.T) {
	r := newReturnRig(t, "Return Test Clock")
	now := time.Now().UTC().Truncate(time.Microsecond)
	frozen := now.Add(60 * 24 * time.Hour)
	clockID := postgres.NewID("hc_tclk")
	bypassExec(t, r.db, r.ctx, `INSERT INTO test_clocks (id, tenant_id, name, frozen_time, status, livemode)
		VALUES ($1, $2, 'return', $3, 'ready', false)`, clockID, r.tenantID, frozen)
	expires := now.Add(30 * 24 * time.Hour)

	pinned := r.customer("ret_clock")
	bypassExec(t, r.db, r.ctx, `UPDATE customers SET test_clock_id = $1 WHERE id = $2`, clockID, pinned)
	g := r.grant(pinned, 5000, &expires, "")
	inv := r.invoice(pinned, 5000)
	r.apply(pinned, inv, 5000)

	simCtx := clock.WithSim(r.ctx, clock.Sim{At: frozen, TestClockID: clockID})
	res := r.void(simCtx, pinned, inv)

	if res.ExpiredOnReturnCents != 5000 {
		t.Errorf("return = %+v, want the 5000 expired at once on the customer's clock", res)
	}
	if ex := r.expiries(pinned, g); len(ex) != 1 || !ex[0].CreatedAt.Equal(frozen) {
		t.Errorf("expiry entries = %+v, want one stamped at the clock's %v", ex, frozen)
	}
	r.checkLedger(pinned)

	// The same grant on the wall clock is live: restored, nothing expires.
	wall := r.customer("ret_clock_wall")
	gw := r.grant(wall, 5000, &expires, "")
	invw := r.invoice(wall, 5000)
	r.apply(wall, invw, 5000)
	if res := r.void(r.ctx, wall, invw); res.ExpiredOnReturnCents != 0 || r.block(gw).Consumed != 0 {
		t.Errorf("wall-clock return = %+v, consumed %d; want restored and live", res, r.block(gw).Consumed)
	}
	r.checkLedger(wall)
}

// Concurrent-resolver: while the return holds the customer's ledger locks,
// between reading the blocks and writing the first restore, a drain on the
// same customer and an operator grant both start. Neither may commit until
// the return does: the drain then draws on the restored capacity.
//
// Mutation-verify (CI, PR #1498): drop the return's advisory lock and its
// FOR UPDATE → the drain and the grant commit inside the window ("finished
// while the return held its locks", red). The grant takes only the advisory
// lock, so dropping that lock alone would let it through (not run); the drain takes the
// advisory lock first, so dropping only the FOR UPDATE changes nothing here.
func TestCreditReturn_ConcurrentWritersWaitForTheReturn(t *testing.T) {
	r := newReturnRig(t, "Return Concurrent")
	admin := testutil.AdminPool(t)
	cust := r.customer("ret_concurrent")
	g := r.grant(cust, 10000, nil, "")
	inv := r.invoice(cust, 6000)
	r.apply(cust, inv, 6000)
	other := r.invoice(cust, 3000)

	var drainFinished, grantFinished atomic.Bool
	drainDone := make(chan error, 1)
	grantDone := make(chan error, 1)
	var finishedEarly []string
	credit.SetBeforeRestoreHook(r.store, func(ctx context.Context, tx *sql.Tx) {
		go func() {
			n, err := r.store.ApplyToInvoiceAtomic(r.ctx, r.tenantID, cust, other, "concurrent drain", 3000, time.Time{})
			if err == nil && n != 3000 {
				err = errs.InvalidState("drained a different amount")
			}
			drainFinished.Store(true)
			drainDone <- err
		}()
		go func() {
			_, err := r.svc.Grant(r.ctx, r.tenantID, credit.GrantInput{Currency: "USD", CustomerID: cust, AmountCents: 1000, Description: "concurrent grant"})
			grantFinished.Store(true)
			grantDone <- err
		}()
		deadline := time.Now().Add(15 * time.Second)
		for {
			if drainFinished.Load() {
				finishedEarly = append(finishedEarly, "drain")
			}
			if grantFinished.Load() {
				finishedEarly = append(finishedEarly, "grant")
			}
			if len(finishedEarly) > 0 {
				return
			}
			var waiting int
			if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%pg_advisory_xact_lock%'`).Scan(&waiting); err != nil {
				t.Errorf("read pg_stat_activity: %v", err)
				return
			}
			if waiting >= 2 {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("only %d of 2 writers reached the customer lock", waiting)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	res, err := r.svc.ReverseForInvoiceTx(r.ctx, tx, r.tenantID, cust, inv, "")
	if err == nil {
		err = tx.Commit()
	}
	postgres.Rollback(tx)
	credit.SetBeforeRestoreHook(r.store, nil)
	if len(finishedEarly) > 0 {
		t.Errorf("%v finished while the return held its locks", finishedEarly)
	}
	if err != nil {
		t.Fatalf("void reversal: %v", err)
	}
	if res.RestoredCents != 6000 {
		t.Errorf("restored %d, want 6000", res.RestoredCents)
	}
	for name, ch := range map[string]chan error{"drain": drainDone, "grant": grantDone} {
		select {
		case err := <-ch:
			if err != nil {
				t.Errorf("%s: %v", name, err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not finish after the return committed", name)
		}
	}
	if b := r.block(g); b.Consumed != 3000 {
		t.Errorf("grant consumed %d, want 3000: the drain ran after the restore", b.Consumed)
	}
	if got := r.balance(cust); got != 8000 {
		t.Errorf("balance %d, want 8000 (10000 - 3000 + 1000)", got)
	}
	r.checkLedger(cust)
}

// The restore is a CAS on the consumed_cents read under the locks. A change
// to the block between that read and the write (here made on the return's
// own transaction, standing in for a writer the locks failed to keep out)
// aborts the return; nothing is written.
//
// Mutation-verify: drop `AND consumed_cents = $4` from the restore UPDATE →
// the return succeeds on the stale read (red).
func TestCreditReturn_RestoreIsACAS(t *testing.T) {
	r := newReturnRig(t, "Return CAS")
	cust := r.customer("ret_cas")
	g := r.grant(cust, 10000, nil, "")
	inv := r.invoice(cust, 6000)
	r.apply(cust, inv, 6000)
	credit.SetBeforeRestoreHook(r.store, func(ctx context.Context, tx *sql.Tx) {
		if _, err := tx.ExecContext(ctx, `UPDATE customer_credit_ledger SET consumed_cents = consumed_cents + 1 WHERE id = $1`, g); err != nil {
			t.Errorf("bump consumed: %v", err)
		}
	})
	defer credit.SetBeforeRestoreHook(r.store, nil)

	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, err = r.svc.ReverseForInvoiceTx(r.ctx, tx, r.tenantID, cust, inv, "")
	postgres.Rollback(tx)
	if errs.Code(err) != "credit_block_changed" {
		t.Fatalf("err = %v (code %q), want credit_block_changed", err, errs.Code(err))
	}
	if b := r.block(g); b.Consumed != 6000 {
		t.Errorf("grant consumed %d, want 6000 (rolled back)", b.Consumed)
	}
}
