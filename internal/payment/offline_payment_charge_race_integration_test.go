package payment_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// boundaryStripe is the concurrent-resolver fake. At the external-call
// boundary where money moves (inside ConfirmPaymentIntent, after the charge
// took its hold, re-checked the invoice and stamped it unknown on the
// PaymentIntent) it runs onCreate, which commits the racing transition on the
// real database, and then answers. beforeStamp runs inside the create instead:
// before the invoice names the PaymentIntent.
type boundaryStripe struct {
	mu          sync.Mutex
	onCreate    func()
	beforeStamp func()
	confirms    int
	res         payment.PaymentIntentResult
	err         error
}

func (f *boundaryStripe) CreateUnconfirmedPaymentIntent(context.Context, payment.PaymentIntentParams) (payment.PaymentIntentResult, error) {
	f.mu.Lock()
	hook := f.beforeStamp
	id := f.res.ID
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if id == "" {
		id = "pi_boundary"
	}
	return payment.PaymentIntentResult{ID: id, Status: "requires_confirmation"}, nil
}

func (f *boundaryStripe) ConfirmPaymentIntent(_ context.Context, id string, _ payment.PaymentIntentParams) (payment.PaymentIntentResult, error) {
	f.mu.Lock()
	f.confirms++
	hook, res, err := f.onCreate, f.res, f.err
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	var pe *payment.PaymentError
	if errors.As(err, &pe) && pe.PaymentIntentID == "" {
		named := *pe
		named.PaymentIntentID = id
		return payment.PaymentIntentResult{}, &named
	}
	return res, err
}

func (f *boundaryStripe) CancelPaymentIntent(context.Context, string) error { return nil }

func (f *boundaryStripe) GetPaymentIntent(_ context.Context, piID string) (payment.PaymentIntentResult, error) {
	return payment.PaymentIntentResult{ID: piID, Status: "succeeded", AmountReceivedCents: settleDue}, nil
}

// An offline payment and a card charge on the same invoice, raced at the
// Stripe call, on the real invoice store and service.
//
//   - While the confirm is at Stripe, the offline payment is refused, with or
//     without the hold: the invoice itself says unknown on the PaymentIntent,
//     stamped before the confirm (ADR-137). The card's answer is then
//     recorded. So the customer cannot pay by card and by bank transfer at
//     the Stripe call. (With one call, an offline payment could land once the
//     hold was cleared, and the card's success was a second payment, flagged
//     after the fact.)
//   - When the offline payment lands before the stamp (the hold cleared under
//     a stalled charger), the stamp does not apply and nothing is confirmed:
//     the invoice keeps no PaymentIntent and paid_out_of_band.
func TestOfflinePayment_RacedWithCardChargeAtTheStripeCall(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	invStore := invoice.NewPostgresStore(db)
	outbox := webhook.NewOutboxStore(db)
	invStore.SetOutboxEnqueuer(outbox)
	invSvc := invoice.NewService(invStore, nil, tenant.NewSettingsStore(db))
	client := &boundaryStripe{}
	s := payment.NewStripe(client, invStore, payment.NewPostgresWebhookStore(db), nil)
	s.SetCardFetcher(settleRaceCard{})
	s.SetAnomalyRecorder(invStore)
	rig := &settleRaceRig{db: db, ctx: ctx, invStore: invStore, customers: customer.NewPostgresStore(db)}

	// column reads the PaymentIntent column and the out-of-band flag as stored.
	column := func(t *testing.T, tenantID, id string) (pi *string, offline bool) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatal(err)
		}
		defer postgres.Rollback(tx)
		var raw *string
		var flag *bool
		// to_jsonb keeps this compiling and failing cleanly on a schema
		// without the column: the key is simply absent.
		if err := tx.QueryRowContext(ctx, `SELECT stripe_payment_intent_id, (to_jsonb(i) ->> 'paid_out_of_band')::boolean FROM invoices i WHERE id = $1`, id).Scan(&raw, &flag); err != nil {
			t.Fatalf("read invoice columns: %v", err)
		}
		return raw, flag != nil && *flag
	}
	charge := func(t *testing.T, name string, beforeStamp bool, onCreate func(tenantID, invoiceID string), res payment.PaymentIntentResult, cerr error) (string, domain.Invoice, domain.Invoice, error) {
		t.Helper()
		tenantID, inv := rig.seed(t, name)
		hold, err := invStore.ClaimChargeForManualCollect(ctx, tenantID, inv.ID)
		if err != nil || !hold.Held() {
			t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
		}
		client.mu.Lock()
		client.onCreate, client.beforeStamp = nil, nil
		if beforeStamp {
			client.beforeStamp = func() { onCreate(tenantID, inv.ID) }
		} else {
			client.onCreate = func() { onCreate(tenantID, inv.ID) }
		}
		client.confirms = 0
		client.res, client.err = res, cerr
		client.mu.Unlock()
		got, chargeErr := s.ChargeInvoice(ctx, tenantID, inv, hold, "cus_stripe_"+name, "pm_"+name)
		after, err := invStore.Get(ctx, tenantID, inv.ID)
		if err != nil {
			t.Fatalf("re-read: %v", err)
		}
		return tenantID, got, after, chargeErr
	}
	// recordOffline records an offline payment at the boundary, after
	// clearing the hold when clearHold; wantRefused says whether it must be
	// refused.
	recordOffline := func(t *testing.T, clearHold, wantRefused bool) func(tenantID, invoiceID string) {
		return func(tenantID, invoiceID string) {
			if clearHold {
				release(t, db, ctx, tenantID, invoiceID)
			}
			_, err := invSvc.RecordOfflinePayment(ctx, tenantID, invoiceID, "wire")
			if wantRefused && !errors.Is(err, errs.ErrInvalidState) {
				t.Errorf("offline payment while the card charge is at Stripe (hold cleared: %v): err %v, want a refusal", clearHold, err)
			}
			if !wantRefused && err != nil {
				t.Errorf("offline payment before the charge stamped the invoice: %v", err)
			}
		}
	}

	t.Run("charge holds the invoice: offline refused, card payment recorded", func(t *testing.T) {
		tenantID, _, after, err := charge(t, "oob-race-held", false, recordOffline(t, false, true),
			payment.PaymentIntentResult{ID: "pi_oob_race_held", Status: "succeeded"}, nil)
		if err != nil {
			t.Fatalf("ChargeInvoice: %v", err)
		}
		if after.Status != domain.InvoicePaid || after.StripePaymentIntentID != "pi_oob_race_held" || after.AmountPaidCents != settleDue {
			t.Errorf("invoice %s PI %q paid %d, want paid by pi_oob_race_held for %d", after.Status, after.StripePaymentIntentID, after.AmountPaidCents, settleDue)
		}
		if _, offline := column(t, tenantID, after.ID); offline {
			t.Error("paid_out_of_band set on a card payment")
		}
	})

	for _, c := range []struct {
		name       string
		res        payment.PaymentIntentResult
		err        error
		wantStatus domain.InvoicePaymentStatus
		wantPI     string
	}{
		{"processing", payment.PaymentIntentResult{ID: "pi_oob_race_proc", Status: "processing"}, nil, domain.PaymentProcessing, "pi_oob_race_proc"},
		{"declined", payment.PaymentIntentResult{ID: "pi_oob_race_decl"}, &payment.PaymentError{Message: "Your card was declined.", DeclineCode: "generic_decline", PaymentIntentID: "pi_oob_race_decl"}, domain.PaymentFailed, "pi_oob_race_decl"},
	} {
		t.Run("hold cleared during the confirm: offline still refused, then the charge answers "+c.name, func(t *testing.T) {
			tenantID, _, after, _ := charge(t, "oob-race-"+c.name, false, recordOffline(t, true, true), c.res, c.err)
			if after.Status != domain.InvoiceFinalized || after.PaymentStatus != c.wantStatus || after.StripePaymentIntentID != c.wantPI || after.AmountPaidCents != 0 {
				t.Fatalf("invoice %s/%s on %q paid %d, want finalized/%s on %s: the card's answer, and no offline payment",
					after.Status, after.PaymentStatus, after.StripePaymentIntentID, after.AmountPaidCents, c.wantStatus, c.wantPI)
			}
			if _, offline := column(t, tenantID, after.ID); offline {
				t.Error("paid_out_of_band set although the offline payment was refused")
			}
		})
	}

	t.Run("offline payment lands before the stamp: nothing is confirmed", func(t *testing.T) {
		tenantID, _, after, err := charge(t, "oob-race-early", true, recordOffline(t, true, false),
			payment.PaymentIntentResult{ID: "pi_oob_race_early", Status: "succeeded"}, nil)
		if !errors.Is(err, payment.ErrPaymentTransient) {
			t.Fatalf("ChargeInvoice: %v, want ErrPaymentTransient (the stamp did not apply)", err)
		}
		client.mu.Lock()
		confirms := client.confirms
		client.mu.Unlock()
		if confirms != 0 {
			t.Fatalf("confirms = %d on an invoice paid offline before the stamp, want 0: that would be a second payment", confirms)
		}
		if after.Status != domain.InvoicePaid || after.PaymentStatus != domain.PaymentSucceeded || after.AmountPaidCents != settleDue {
			t.Fatalf("invoice %s/%s paid %d, want the offline payment's paid/succeeded %d", after.Status, after.PaymentStatus, after.AmountPaidCents, settleDue)
		}
		pi, offline := column(t, tenantID, after.ID)
		if pi != nil || !offline {
			t.Errorf("PaymentIntent %v, paid_out_of_band %v; want NULL and true", pi, offline)
		}
		if after.HasCardPayment() {
			t.Error("HasCardPayment() = true on an invoice paid by bank transfer")
		}
	})
}

// release clears the invoice's charge hold, as the stale-hold sweep does
// when Stripe shows no payment for it (ADR-127).
func release(t *testing.T, db *postgres.DB, ctx context.Context, tenantID, id string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET auto_charge_claimed_until = NULL WHERE id = $1`, id); err != nil {
		t.Fatalf("clear hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(fmt.Errorf("commit hold clear: %w", err))
	}
}
