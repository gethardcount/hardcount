package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// The charge record (ADR-127, #976) on real Postgres: the CAS that writes it,
// the claims and settling actions that refuse while it exists, and the
// reconciler's release and park.

type chargeRecordCols struct {
	set              bool
	startedAt        time.Time
	seq              int64
	key              string
	stripeCustomerID string
}

func readChargeRecord(t *testing.T, db *postgres.DB, id string) chargeRecordCols {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var at sql.NullTime
	var seq sql.NullInt64
	var key, cust sql.NullString
	if err := tx.QueryRow(`SELECT charge_started_at, charge_started_seq, charge_started_key, charge_started_stripe_customer
		FROM invoices WHERE id = $1`, id).Scan(&at, &seq, &key, &cust); err != nil {
		t.Fatalf("read charge record: %v", err)
	}
	return chargeRecordCols{set: at.Valid, startedAt: at.Time, seq: seq.Int64, key: key.String, stripeCustomerID: cust.String}
}

func execBypass(t *testing.T, db *postgres.DB, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func recordKey(id string, seq int64) string { return fmt.Sprintf("velox_inv_%s_%d_pm_card", id, seq) }

// recordCharge writes a charge record the way the charge path does: the real
// claim, then BeginChargeAttempt at the invoice's current seq. The hold is
// then lapsed, so whatever refuses afterwards refuses because of the record.
func recordCharge(t *testing.T, db *postgres.DB, store *invoice.PostgresStore, ctx context.Context, tenantID, id string) chargeRecordCols {
	t.Helper()
	claimAuto(t, store, ctx, tenantID, id)
	inv, err := store.Get(ctx, tenantID, id)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := store.BeginChargeAttempt(ctx, tenantID, id, inv.ChargeAttemptSeq, recordKey(id, inv.ChargeAttemptSeq), "cus_stripe_"+id)
	if err != nil || !ok {
		t.Fatalf("BeginChargeAttempt: ok=%v err=%v", ok, err)
	}
	expireLease(t, db, id)
	return readChargeRecord(t, db, id)
}

// The record is written only by a charge that holds the invoice, at the seq
// it read, while the invoice is still chargeable, and only once.
func TestBeginChargeAttempt_RecordsOnlyWhileTheChargeHoldsTheInvoice(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record CAS")
	store := invoice.NewPostgresStore(db)

	begin := func(id string, seq int64) bool {
		t.Helper()
		ok, err := store.BeginChargeAttempt(ctx, tenantID, id, seq, recordKey(id, seq), "cus_stripe_"+id)
		if err != nil {
			t.Fatalf("BeginChargeAttempt: %v", err)
		}
		return ok
	}

	t.Run("written by the holder, once, with every field", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-OK")
		if begin(inv.ID, inv.ChargeAttemptSeq) {
			t.Fatal("recorded with no charge hold: a charger that does not hold the invoice must never call Stripe")
		}
		claimAuto(t, store, ctx, tenantID, inv.ID)
		if !begin(inv.ID, inv.ChargeAttemptSeq) {
			t.Fatal("the holder could not record its charge")
		}
		rec := readChargeRecord(t, db, inv.ID)
		if !rec.set || rec.seq != inv.ChargeAttemptSeq || rec.key != recordKey(inv.ID, inv.ChargeAttemptSeq) || rec.stripeCustomerID != "cus_stripe_"+inv.ID {
			t.Fatalf("record = %+v", rec)
		}
		if d := time.Since(rec.startedAt); d < -time.Minute || d > time.Minute {
			t.Errorf("charge_started_at %s is not the database's now", rec.startedAt)
		}
		if begin(inv.ID, inv.ChargeAttemptSeq) {
			t.Error("a second record at the same counter: two charges could both call Stripe")
		}

		got, err := store.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ChargeStartedAt == nil || !got.PaymentInFlight() {
			t.Errorf("Get: ChargeStartedAt=%v PaymentInFlight=%v, want the record on the read and the payment in flight", got.ChargeStartedAt, got.PaymentInFlight())
		}

		if ok, err := store.EndChargeAttempt(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq+1); err != nil || ok {
			t.Errorf("EndChargeAttempt at another seq: ok=%v err=%v, want false", ok, err)
		}
		if !readChargeRecord(t, db, inv.ID).set {
			t.Fatal("another attempt's End cleared this record")
		}
		if ok, err := store.EndChargeAttempt(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq); err != nil || !ok {
			t.Fatalf("EndChargeAttempt: ok=%v err=%v", ok, err)
		}
		if rec := readChargeRecord(t, db, inv.ID); rec != (chargeRecordCols{}) {
			t.Errorf("record after End = %+v, want all four columns NULL", rec)
		}
		if got, _ := store.Get(ctx, tenantID, inv.ID); got.ChargeStartedAt != nil || got.PaymentInFlight() {
			t.Error("the cleared record still reads as in flight")
		}
	})

	t.Run("the hold lapsed before the record (a stalled charger)", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-STALL")
		claimAuto(t, store, ctx, tenantID, inv.ID)
		expireLease(t, db, inv.ID)
		if begin(inv.ID, inv.ChargeAttemptSeq) {
			t.Fatal("recorded after the hold lapsed: another charger may hold the invoice now")
		}
	})

	t.Run("an outcome was saved since the charge read the invoice", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-SEQ")
		claimAuto(t, store, ctx, tenantID, inv.ID)
		stampOutcome(t, store, ctx, tenantID, inv.ID, domain.PaymentFailed, "", "card_declined")
		if begin(inv.ID, inv.ChargeAttemptSeq) {
			t.Fatal("recorded at a stale seq: the key would repeat an attempt whose outcome is saved")
		}
	})

	t.Run("the invoice is no longer chargeable", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			apply func(id string)
		}{
			{"payment unknown", func(id string) {
				stampOutcome(t, store, ctx, tenantID, id, domain.PaymentUnknown, "pi_unk", "timeout")
			}},
			{"paid", func(id string) {
				if _, _, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, id, "pi_paid", time.Now().UTC(), nil); err != nil {
					t.Fatal(err)
				}
			}},
		} {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-"+strings.ToUpper(strings.ReplaceAll(tc.name, " ", "-")))
			claimAuto(t, store, ctx, tenantID, inv.ID)
			tc.apply(inv.ID)
			cur, _ := store.Get(ctx, tenantID, inv.ID)
			if begin(inv.ID, cur.ChargeAttemptSeq) {
				t.Errorf("%s: recorded a charge on an invoice no charge may start on", tc.name)
			}
		}
	})
}

// Two charges at one counter: exactly one records itself.
func TestBeginChargeAttempt_TwoAtOneCounter_OneWins(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record Race")
	store := invoice.NewPostgresStore(db)

	for round := range 10 {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, fmt.Sprintf("INV-REC-RACE-%d", round))
		claimAuto(t, store, ctx, tenantID, inv.ID)
		var wins, losses int
		var mu sync.Mutex
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ok, err := store.BeginChargeAttempt(ctx, tenantID, inv.ID, inv.ChargeAttemptSeq, fmt.Sprintf("key_%d", i), "cus_x")
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					t.Errorf("round %d: %v", round, err)
				}
				if ok {
					wins++
				} else {
					losses++
				}
			}()
		}
		close(start)
		wg.Wait()
		if wins != 1 || losses != 3 {
			t.Fatalf("round %d: %d records written, want exactly 1", round, wins)
		}
	}
}

// No charge path claims an invoice that holds a charge record, whatever the
// charge kind or the card.
func TestChargeRecord_RefusedByEveryClaimPath(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record Claims")
	store := invoice.NewPostgresStore(db)

	for _, c := range []struct {
		name  string
		claim func(id string) (bool, error)
	}{
		{"ClaimAutoCharge", func(id string) (bool, error) { return store.ClaimAutoCharge(ctx, tenantID, id) }},
		{"ClaimChargeForManualCollect", func(id string) (bool, error) { return store.ClaimChargeForManualCollect(ctx, tenantID, id) }},
		{"ClaimChargeForDunningRetry", func(id string) (bool, error) { return store.ClaimChargeForDunningRetry(ctx, tenantID, id) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-CLAIM-"+c.name)
			rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)

			ok, err := c.claim(inv.ID)
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatalf("%s claimed an invoice with a charge record and no hold: the charge sent to Stripe may have taken the money, so this would be the customer's second payment", c.name)
			}

			// Control: once the record clears, the same claim goes through.
			if ok, err := store.EndChargeAttempt(ctx, tenantID, inv.ID, rec.seq); err != nil || !ok {
				t.Fatalf("EndChargeAttempt: ok=%v err=%v", ok, err)
			}
			if ok, err := c.claim(inv.ID); err != nil || !ok {
				t.Fatalf("control: %s refused once the record cleared: ok=%v err=%v", c.name, ok, err)
			}
		})
	}
}

// Every read that reaches a "may a payment be open?" check carries the record.
func TestChargeRecord_EveryInvoiceReadCarriesIt(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record Reads")
	store := invoice.NewPostgresStore(db)

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-READS")
	recordCharge(t, db, store, ctx, tenantID, inv.ID)

	byNumber, err := store.GetByNumber(ctx, tenantID, inv.InvoiceNumber)
	if err != nil {
		t.Fatal(err)
	}
	list, _, err := store.List(ctx, invoice.ListFilter{TenantID: tenantID, CustomerID: inv.CustomerID})
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %d rows, %v", len(list), err)
	}
	for name, got := range map[string]domain.Invoice{"GetByNumber": byNumber, "List": list[0]} {
		if got.ChargeStartedAt == nil || !got.PaymentInFlight() {
			t.Errorf("%s: ChargeStartedAt=%v, want the record on the read", name, got.ChargeStartedAt)
		}
	}
}

// Void, write-off and offline payment refuse while the record exists, with the
// hold long gone, and say why in words an operator can act on.
func TestChargeRecord_RefusesVoidWriteOffAndOfflinePayment(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record Actions")
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, nil)

	for i, a := range heldChargeActions {
		t.Run(a.name, func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-ACT-"+string(rune('A'+i)))
			rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)
			got, err := a.run(ctx, svc, tenantID, inv.ID)
			requireChargeInProgressRefusal(t, store, ctx, tenantID, inv.ID, got, err)
			var de *errs.DomainError
			if errors.As(err, &de) && !strings.Contains(de.Message, "about an hour") {
				t.Errorf("refusal %q does not say an interrupted charge is confirmed within about an hour", de.Message)
			}

			// Control: allowed once the record clears.
			if ok, err := store.EndChargeAttempt(ctx, tenantID, inv.ID, rec.seq); err != nil || !ok {
				t.Fatal(err)
			}
			if got, err := a.run(ctx, svc, tenantID, inv.ID); err != nil || got.Status != a.want {
				t.Fatalf("control: %v, status %s, want %s", err, got.Status, a.want)
			}
		})
	}

	t.Run("relief void inside a cancel or plan swap", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REC-RELIEF")
		recordCharge(t, db, store, ctx, tenantID, inv.ID)
		snap, err := store.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		defer postgres.Rollback(tx)
		got, verr := svc.VoidUnchangedTx(ctx, tx, tenantID, snap)
		requireChargeInProgressRefusal(t, store, ctx, tenantID, inv.ID, got, verr)
	})
}

// The release: Stripe has nothing for the record. It clears the record and
// moves the counter if it still equals the record's, so the next charge sends
// a fresh key; a CAS on the record it read.
func TestReleaseInterruptedCharge(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record Release")
	store := invoice.NewPostgresStore(db)

	t.Run("counter unchanged: released and moved", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REL-1")
		rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)
		released, moved, err := store.ReleaseInterruptedCharge(ctx, tenantID, inv.ID, rec.seq, rec.startedAt)
		if err != nil || !released || !moved {
			t.Fatalf("released=%v moved=%v err=%v, want both", released, moved, err)
		}
		got, _ := store.Get(ctx, tenantID, inv.ID)
		if got.ChargeAttemptSeq != rec.seq+1 || got.ChargeStartedAt != nil || got.PaymentStatus != domain.PaymentPending {
			t.Fatalf("after release: seq %d record %v status %s, want seq %d, no record, pending", got.ChargeAttemptSeq, got.ChargeStartedAt, got.PaymentStatus, rec.seq+1)
		}
	})

	t.Run("an outcome was saved meanwhile: released, the counter left alone", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REL-2")
		rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)
		stampOutcome(t, store, ctx, tenantID, inv.ID, domain.PaymentFailed, "", "card_declined")
		released, moved, err := store.ReleaseInterruptedCharge(ctx, tenantID, inv.ID, rec.seq, rec.startedAt)
		if err != nil || !released || moved {
			t.Fatalf("released=%v moved=%v err=%v, want released, not moved", released, moved, err)
		}
		if got, _ := store.Get(ctx, tenantID, inv.ID); got.ChargeAttemptSeq != rec.seq+1 {
			t.Fatalf("seq %d, want %d (the outcome's bump only)", got.ChargeAttemptSeq, rec.seq+1)
		}
	})

	t.Run("the record changed since it was read: nothing", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-REL-3")
		rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)
		for _, tc := range []struct {
			seq int64
			at  time.Time
		}{{rec.seq, rec.startedAt.Add(-time.Second)}, {rec.seq + 1, rec.startedAt}} {
			if released, _, err := store.ReleaseInterruptedCharge(ctx, tenantID, inv.ID, tc.seq, tc.at); err != nil || released {
				t.Fatalf("released=%v err=%v for another record", released, err)
			}
		}
		if !readChargeRecord(t, db, inv.ID).set {
			t.Fatal("another record's release cleared this one")
		}
	})
}

// The give-up: the invoice parks (ADR-107) and the record clears in one
// statement, or, if the invoice moved on, only the record clears.
func TestParkInterruptedCharge(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record Park")
	store := invoice.NewPostgresStore(db)

	t.Run("still waiting at the record's counter: parked", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-PARK-1")
		rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)
		parked, cleared, err := store.ParkInterruptedCharge(ctx, tenantID, inv.ID, rec.seq, rec.startedAt, "Stripe could not be read for 24 hours")
		if err != nil || !parked || !cleared {
			t.Fatalf("parked=%v cleared=%v err=%v", parked, cleared, err)
		}
		got, _ := store.Get(ctx, tenantID, inv.ID)
		if got.PaymentStatus != domain.PaymentUnknown || got.StripePaymentIntentID != "" || got.ChargeAttemptSeq != rec.seq+1 || got.ChargeStartedAt != nil {
			t.Fatalf("after park: %s PI %q seq %d record %v, want unknown, none, %d, no record",
				got.PaymentStatus, got.StripePaymentIntentID, got.ChargeAttemptSeq, got.ChargeStartedAt, rec.seq+1)
		}
		if got.LastPaymentError == "" {
			t.Error("the park records no reason")
		}
	})

	t.Run("paid meanwhile: only the record clears", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-PARK-2")
		rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)
		if _, _, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, inv.ID, "pi_paid", time.Now().UTC(), nil); err != nil {
			t.Fatal(err)
		}
		parked, cleared, err := store.ParkInterruptedCharge(ctx, tenantID, inv.ID, rec.seq, rec.startedAt, "x")
		if err != nil || parked || !cleared {
			t.Fatalf("parked=%v cleared=%v err=%v, want only cleared", parked, cleared, err)
		}
		if got, _ := store.Get(ctx, tenantID, inv.ID); got.Status != domain.InvoicePaid || got.ChargeStartedAt != nil {
			t.Fatalf("after: %s record %v, want paid with no record", got.Status, got.ChargeStartedAt)
		}
	})

	t.Run("another record: nothing", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-PARK-3")
		rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)
		parked, cleared, err := store.ParkInterruptedCharge(ctx, tenantID, inv.ID, rec.seq, rec.startedAt.Add(time.Second), "x")
		if err != nil || parked || cleared {
			t.Fatalf("parked=%v cleared=%v err=%v for another record", parked, cleared, err)
		}
	})
}

// The check's input: this mode's records past the minimum age, not checked
// within it, never-checked first; PastGiveUp on the database's clock.
func TestListInterruptedCharges(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	liveCtx := postgres.WithLivemode(context.Background(), true)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record List")
	store := invoice.NewPostgresStore(db)

	seedRecord := func(c context.Context, num, age, synced string) domain.Invoice {
		t.Helper()
		inv := seedClaimableInvoice(t, db, c, tenantID, num)
		recordCharge(t, db, store, c, tenantID, inv.ID)
		execBypass(t, db, `UPDATE invoices SET charge_started_at = now() - $2::interval WHERE id = $1`, inv.ID, age)
		if synced != "" {
			execBypass(t, db, `UPDATE invoices SET provider_synced_at = now() - $2::interval WHERE id = $1`, inv.ID, synced)
		}
		return inv
	}
	young := seedRecord(ctx, "INV-LIST-YOUNG", "1 minute", "")
	due := seedRecord(ctx, "INV-LIST-DUE", "11 minutes", "")
	old := seedRecord(ctx, "INV-LIST-OLD", "25 hours", "")
	checkedRecently := seedRecord(ctx, "INV-LIST-RECENT", "2 hours", "1 minute")
	checkedLongAgo := seedRecord(ctx, "INV-LIST-LONGAGO", "2 hours", "11 minutes")
	live := seedRecord(liveCtx, "INV-LIST-LIVE", "2 hours", "")

	got, err := store.ListInterruptedCharges(ctx, 10*time.Minute, 24*time.Hour, 50)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]domain.InterruptedCharge{}
	for _, c := range got {
		ids = append(ids, c.InvoiceID)
		byID[c.InvoiceID] = c
	}
	want := []string{old.ID, due.ID, checkedLongAgo.ID}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("listed %v, want %v (never-checked first, oldest first; not %s young, %s checked a minute ago, %s live mode)",
			ids, want, young.ID, checkedRecently.ID, live.ID)
	}
	if !byID[old.ID].PastGiveUp || byID[due.ID].PastGiveUp {
		t.Errorf("PastGiveUp: old=%v due=%v, want true/false", byID[old.ID].PastGiveUp, byID[due.ID].PastGiveUp)
	}
	c := byID[due.ID]
	if c.TenantID != tenantID || c.Seq != due.ChargeAttemptSeq || c.Key != recordKey(due.ID, due.ChargeAttemptSeq) || c.StripeCustomerID != "cus_stripe_"+due.ID || c.StartedAt.IsZero() {
		t.Errorf("listed record = %+v", c)
	}
}

// Pair test (playbook §5.6): the release and a card settlement of the same
// invoice, at once. No deadlock, no error, and a consistent end: paid, no
// record, the counter moved once or twice in the right order.
func TestReleaseInterruptedCharge_RacesCardSettlement(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Record Pair")
	store := invoice.NewPostgresStore(db)

	for round := range 10 {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, fmt.Sprintf("INV-PAIR-%d", round))
		rec := recordCharge(t, db, store, ctx, tenantID, inv.ID)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var released, moved bool
		var relErr, payErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			released, moved, relErr = store.ReleaseInterruptedCharge(ctx, tenantID, inv.ID, rec.seq, rec.startedAt)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, _, payErr = store.MarkPaidCardSettlementTransition(ctx, tenantID, inv.ID, "pi_pair", time.Now().UTC(), nil)
		}()
		close(start)
		wg.Wait()
		if relErr != nil || payErr != nil {
			t.Fatalf("round %d: release err %v, settle err %v", round, relErr, payErr)
		}
		if !released {
			t.Fatalf("round %d: the release found no record (the settlement must not clear it)", round)
		}
		got, _ := store.Get(ctx, tenantID, inv.ID)
		wantSeq := rec.seq + 1
		if moved {
			wantSeq = rec.seq + 2
		}
		if got.Status != domain.InvoicePaid || got.ChargeStartedAt != nil || got.ChargeAttemptSeq != wantSeq {
			t.Fatalf("round %d: %s record %v seq %d (moved=%v), want paid, no record, seq %d", round, got.Status, got.ChargeStartedAt, got.ChargeAttemptSeq, moved, wantSeq)
		}
	}
}
