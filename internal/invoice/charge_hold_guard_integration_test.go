package invoice_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// #975. A card charge between its claim and its outcome leaves the invoice at
// payment_status 'pending' (or 'failed', on a retry) and writes only the
// 5-minute charge hold, auto_charge_claimed_until. Void, write-off and
// offline payment used to check payment_status alone, on a copy read before
// the row lock, so all three went through while Stripe could be taking the
// money: a charge then succeeded on a voided invoice, or the customer paid
// twice after an offline payment.
//
// Every hold below is taken by the REAL claim method, so the row is in the
// shape a charge in progress leaves it.

// operatorAction is one of the three guarded actions, run through the real
// service.
type operatorAction struct {
	name string
	run  func(ctx context.Context, svc *invoice.Service, tenantID, id string) (domain.Invoice, error)
	want domain.InvoiceStatus // status after a successful run
}

var heldChargeActions = []operatorAction{
	{"void", func(ctx context.Context, svc *invoice.Service, tenantID, id string) (domain.Invoice, error) {
		return svc.Void(ctx, tenantID, id)
	}, domain.InvoiceVoided},
	{"write-off", func(ctx context.Context, svc *invoice.Service, tenantID, id string) (domain.Invoice, error) {
		return svc.MarkUncollectible(ctx, tenantID, id)
	}, domain.InvoiceUncollectible},
	{"offline payment", func(ctx context.Context, svc *invoice.Service, tenantID, id string) (domain.Invoice, error) {
		return svc.RecordOfflinePayment(ctx, tenantID, id, "wire")
	}, domain.InvoicePaid},
}

// requireChargeInProgressRefusal asserts the 409 the three actions share and
// that the invoice was left exactly where the charge found it.
func requireChargeInProgressRefusal(t *testing.T, store *invoice.PostgresStore, ctx context.Context, tenantID, id string, got domain.Invoice, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("action went through while a charge was in progress (status now %s) — Stripe may be taking the money", got.Status)
	}
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("want an invalid-state refusal (HTTP 409), got %v", err)
	}
	var de *errs.DomainError
	if !errors.As(err, &de) || de.Code != "payment_in_flight" {
		t.Fatalf("want code payment_in_flight, got %v", err)
	}
	cur, gerr := store.Get(ctx, tenantID, id)
	if gerr != nil {
		t.Fatalf("re-read: %v", gerr)
	}
	if cur.Status != domain.InvoiceFinalized {
		t.Fatalf("a refused action changed the invoice: status %s", cur.Status)
	}
}

// requireHoldRefusal is requireChargeInProgressRefusal for a refusal caused by
// the charge hold, whose message must tell the operator to wait briefly.
func requireHoldRefusal(t *testing.T, store *invoice.PostgresStore, ctx context.Context, tenantID, id string, got domain.Invoice, err error) {
	t.Helper()
	requireChargeInProgressRefusal(t, store, ctx, tenantID, id, got, err)
	var de *errs.DomainError
	if errors.As(err, &de) && !strings.Contains(de.Message, "try again in a few minutes") {
		t.Fatalf("the refusal must tell the operator to try again shortly, got %q", de.Message)
	}
}

func claimAuto(t *testing.T, store *invoice.PostgresStore, ctx context.Context, tenantID, id string) {
	t.Helper()
	if ok, err := store.ClaimAutoCharge(ctx, tenantID, id); err != nil || !ok {
		t.Fatalf("ClaimAutoCharge: ok=%v err=%v", ok, err)
	}
}

func TestChargeHold_RefusesVoidWriteOffAndOfflinePayment(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Hold Guard")
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, nil)

	for i, a := range heldChargeActions {
		num := func(tag string) string { return "INV-HOLD-" + tag + "-" + string(rune('A'+i)) }

		t.Run(a.name+"/automatic charge claimed, outcome not yet written (pending)", func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, num("PEND"))
			claimAuto(t, store, ctx, tenantID, inv.ID)
			got, err := a.run(ctx, svc, tenantID, inv.ID)
			requireHoldRefusal(t, store, ctx, tenantID, inv.ID, got, err)
		})

		t.Run(a.name+"/retry of a declined charge claimed (failed)", func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, num("FAIL"))
			stampOutcome(t, store, ctx, tenantID, inv.ID, domain.PaymentFailed, "", "card_declined")
			if ok, err := store.ClaimChargeForManualCollect(ctx, tenantID, inv.ID); err != nil || !ok {
				t.Fatalf("ClaimChargeForManualCollect: ok=%v err=%v", ok, err)
			}
			got, err := a.run(ctx, svc, tenantID, inv.ID)
			requireHoldRefusal(t, store, ctx, tenantID, inv.ID, got, err)
		})

		// Controls: the guard must not refuse what it has no reason to.
		t.Run(a.name+"/no hold: allowed", func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, num("FREE"))
			got, err := a.run(ctx, svc, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("no charge in progress, yet refused: %v", err)
			}
			if got.Status != a.want {
				t.Fatalf("status %s, want %s", got.Status, a.want)
			}
		})

		t.Run(a.name+"/expired hold: allowed", func(t *testing.T) {
			// A crashed charger's hold lapses after five minutes; past that the
			// operator must not be locked out.
			inv := seedClaimableInvoice(t, db, ctx, tenantID, num("EXP"))
			claimAuto(t, store, ctx, tenantID, inv.ID)
			expireLease(t, db, inv.ID)
			got, err := a.run(ctx, svc, tenantID, inv.ID)
			if err != nil {
				t.Fatalf("hold expired, yet refused: %v", err)
			}
			if got.Status != a.want {
				t.Fatalf("status %s, want %s", got.Status, a.want)
			}
		})
	}
}

// TestChargeHold_ParkedInvoiceStillWritableOff: an unknown outcome keeps its
// hold for the full five minutes (the charge path releases it only on a
// definite outcome). A PARKED invoice — unknown with no PaymentIntent id —
// has write-off as its only exit (ADR-107), so the hold must not refuse it.
// Void and offline payment stay refused, by the parked rule.
func TestChargeHold_ParkedInvoiceStillWritableOff(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Hold Parked")
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, nil)

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-HOLD-PARKED")
	claimAuto(t, store, ctx, tenantID, inv.ID)
	stampOutcome(t, store, ctx, tenantID, inv.ID, domain.PaymentUnknown, "", "ambiguous: no payment_intent_id")
	if _, err := svc.Void(ctx, tenantID, inv.ID); err == nil {
		t.Fatal("a parked invoice must still refuse a void")
	}
	got, err := svc.MarkUncollectible(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("parked invoice with a live hold must stay writable-off (ADR-107's only exit): %v", err)
	}
	if got.Status != domain.InvoiceUncollectible {
		t.Fatalf("status %s, want uncollectible", got.Status)
	}
}

// raceAfterGet is a concurrent-resolver store: its Get returns the invoice as
// it stood, then commits a racing write before the caller acts on that copy.
// The service's pre-read is therefore stale, and only a check on the row read
// under the lock can see the race.
type raceAfterGet struct {
	*invoice.PostgresStore
	race func()
	done bool
}

func (r *raceAfterGet) Get(ctx context.Context, tenantID, id string) (domain.Invoice, error) {
	inv, err := r.PostgresStore.Get(ctx, tenantID, id)
	if err == nil && !r.done {
		r.done = true
		r.race()
	}
	return inv, err
}

func TestChargeHold_StaleReadIsRefusedUnderTheLock(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Hold Stale")
	store := invoice.NewPostgresStore(db)

	for i, a := range heldChargeActions {
		suffix := string(rune('A' + i))

		t.Run(a.name+"/hold taken after the service read the invoice", func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-HOLD-STALE-"+suffix)
			racer := &raceAfterGet{PostgresStore: store, race: func() { claimAuto(t, store, ctx, tenantID, inv.ID) }}
			svc := invoice.NewService(racer, nil, nil)
			got, err := a.run(ctx, svc, tenantID, inv.ID)
			if !racer.done {
				t.Fatal("the race never ran")
			}
			requireHoldRefusal(t, store, ctx, tenantID, inv.ID, got, err)
		})

		// The same race on payment_status: the charge reports 'processing'
		// after the service's read. Only the locked row carries it.
		t.Run(a.name+"/charge reported processing after the service read the invoice", func(t *testing.T) {
			inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-HOLD-PROC-"+suffix)
			racer := &raceAfterGet{PostgresStore: store, race: func() {
				stampOutcome(t, store, ctx, tenantID, inv.ID, domain.PaymentProcessing, "pi_stale_"+suffix, "")
			}}
			svc := invoice.NewService(racer, nil, nil)
			got, err := a.run(ctx, svc, tenantID, inv.ID)
			if !racer.done {
				t.Fatal("the race never ran")
			}
			requireChargeInProgressRefusal(t, store, ctx, tenantID, inv.ID, got, err)
		})
	}
}

// TestChargeHold_ChargeStillSettlesItself is the negative control for the
// card path: the charge holding the hold must still be able to record its own
// success. Applying the operator refusal to the card settlement would strand
// every successful charge.
func TestChargeHold_ChargeStillSettlesItself(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Hold Self Settle")
	store := invoice.NewPostgresStore(db)

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-HOLD-SELF")
	claimAuto(t, store, ctx, tenantID, inv.ID)
	paid, transitioned, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, inv.ID, "pi_self_settle", time.Now().UTC(), nil)
	if err != nil {
		t.Fatalf("the charge holding the hold could not settle itself: %v", err)
	}
	if !transitioned || paid.Status != domain.InvoicePaid {
		t.Fatalf("transitioned=%v status=%s, want a paid transition", transitioned, paid.Status)
	}
}

// TestChargeHold_ReliefVoidRefused: the engine's relief void (VoidUnchangedTx,
// run inside a cancel or plan swap) voids a prepaid invoice on its period's
// first day — when the first automatic charge fires. It must refuse while a
// charge holds the invoice, or the charge succeeds on a voided invoice.
func TestChargeHold_ReliefVoidRefused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Charge Hold Relief Void")
	store := invoice.NewPostgresStore(db)
	svc := invoice.NewService(store, nil, nil)

	reliefVoid := func(t *testing.T, id string) (domain.Invoice, error) {
		t.Helper()
		snap, err := store.Get(ctx, tenantID, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		got, verr := svc.VoidUnchangedTx(ctx, tx, tenantID, snap)
		if verr != nil {
			return got, verr
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return got, nil
	}

	t.Run("live hold: refused", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-HOLD-RELIEF-LIVE")
		claimAuto(t, store, ctx, tenantID, inv.ID)
		got, err := reliefVoid(t, inv.ID)
		requireHoldRefusal(t, store, ctx, tenantID, inv.ID, got, err)
	})

	t.Run("expired hold: voided", func(t *testing.T) {
		inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-HOLD-RELIEF-EXP")
		claimAuto(t, store, ctx, tenantID, inv.ID)
		expireLease(t, db, inv.ID)
		got, err := reliefVoid(t, inv.ID)
		if err != nil {
			t.Fatalf("hold expired, yet refused: %v", err)
		}
		if got.Status != domain.InvoiceVoided {
			t.Fatalf("status %s, want voided", got.Status)
		}
	})
}

// TestRecordOfflinePayment_CardSettledAfterReadIsRefused: a card charge that
// settles between the service's read and the row lock leaves the row paid. The
// paid transition is then a no-op, so without a refusal under the lock the
// service would still write the payment_recorded audit row and send
// invoice.payment_recorded for a payment that never happened.
func TestRecordOfflinePayment_CardSettledAfterReadIsRefused(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Offline After Card Settle")
	store := invoice.NewPostgresStore(db)

	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-OFFLINE-AFTER-CARD")
	racer := &raceAfterGet{PostgresStore: store, race: func() {
		if _, ok, err := store.MarkPaidCardSettlementTransition(ctx, tenantID, inv.ID, "pi_card_won", time.Now().UTC(), nil); err != nil || !ok {
			t.Fatalf("card settle: ok=%v err=%v", ok, err)
		}
	}}
	svc := invoice.NewService(racer, nil, nil)

	_, err := svc.RecordOfflinePayment(ctx, tenantID, inv.ID, "wire")
	if !racer.done {
		t.Fatal("the race never ran")
	}
	if !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("an offline payment on an invoice the card already paid must be refused, got %v", err)
	}
	cur, gerr := store.Get(ctx, tenantID, inv.ID)
	if gerr != nil {
		t.Fatalf("re-read: %v", gerr)
	}
	if cur.StripePaymentIntentID != "pi_card_won" {
		t.Fatalf("payment intent %q, want the card's pi_card_won", cur.StripePaymentIntentID)
	}
}
