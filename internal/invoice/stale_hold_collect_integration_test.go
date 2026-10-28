package invoice_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Collect payment and the charge hold (#976, ADR-127), on the real store
// through the real handler. Only the charge is faked.

type readyPaymentSetup struct{}

func (readyPaymentSetup) GetPaymentSetup(_ context.Context, _, customerID string) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{
		CustomerID: customerID, SetupStatus: domain.PaymentSetupReady,
		StripeCustomerID: "cus_stripe_collect", StripePaymentMethodID: "pm_collect",
	}, nil
}

// creditsCoverAll stands in for the credit drain: the customer's balance
// covers the whole invoice.
type creditsCoverAll struct {
	db *postgres.DB
}

func (c creditsCoverAll) ApplyToInvoiceAt(ctx context.Context, _, _, invoiceID string, amountCents int64, _ time.Time, _ ...string) (int64, error) {
	tx, err := c.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `UPDATE invoices SET amount_due_cents = 0, credits_applied_cents = credits_applied_cents + $2 WHERE id = $1`,
		invoiceID, amountCents); err != nil {
		return 0, err
	}
	return amountCents, tx.Commit()
}

func postCollect(t *testing.T, h *invoice.Handler, tenantID, invoiceID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/"+invoiceID+"/collect", nil)
	req = req.WithContext(context.WithValue(postgres.WithLivemode(req.Context(), false), auth.TestTenantIDKey(), tenantID))
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)
	return rr
}

func setHoldAgo(t *testing.T, db *postgres.DB, invoiceID string, ago time.Duration) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(`UPDATE invoices SET auto_charge_claimed_until = now() - make_interval(secs => $2) WHERE id = $1`,
		invoiceID, ago.Seconds()); err != nil {
		t.Fatalf("set hold: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func holdIsSet(t *testing.T, db *postgres.DB, invoiceID string) bool {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var set bool
	if err := tx.QueryRow(`SELECT auto_charge_claimed_until IS NOT NULL FROM invoices WHERE id = $1`, invoiceID).Scan(&set); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	return set
}

// 1 (Collect). A charge whose result was lost left a stale hold. Collect
// charges nothing and answers 409 with the words void and offline payment
// use, saying the charge is being confirmed with Stripe.
func TestCollect_StaleHold_RefusedWith409AndNoCharge(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Collect Stale Hold")
	store := invoice.NewPostgresStore(db)
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-COLLECT-STALE")
	setHoldAgo(t, db, inv.ID, 11*time.Minute)

	charger := &collectCharger{}
	h := invoice.NewHandler(invoice.NewService(store, nil, nil), nil, nil, invoice.HandlerDeps{Charger: charger, PaymentSetups: readyPaymentSetup{}})
	rr := postCollect(t, h, tenantID, inv.ID)

	if charger.calls != 0 {
		t.Fatalf("Collect charged %d times on an invoice whose earlier charge may have taken the money", charger.calls)
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409. body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "payment_in_flight") || !strings.Contains(body, "being confirmed with Stripe") {
		t.Errorf("body = %s, want code payment_in_flight and the 'being confirmed with Stripe' copy", body)
	}
	if !holdIsSet(t, db, inv.ID) {
		t.Error("Collect cleared a stale hold: only the stale-hold sweep may, after asking Stripe")
	}
}

// 14 (Collect). Credits cover the whole invoice, so Collect settles it with no
// charge — and releases its hold. Left set, it would wait for the stale-hold
// sweep for no reason.
func TestCollect_CreditsCoverIt_SettledAndHoldReleased(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Collect Credit Covered")
	store := invoice.NewPostgresStore(db)
	inv := seedClaimableInvoice(t, db, ctx, tenantID, "INV-COLLECT-CREDIT")

	svc := invoice.NewService(store, nil, nil)
	svc.SetCreditApplier(creditsCoverAll{db: db})
	charger := &collectCharger{}
	h := invoice.NewHandler(svc, nil, nil, invoice.HandlerDeps{Charger: charger, PaymentSetups: readyPaymentSetup{}})
	rr := postCollect(t, h, tenantID, inv.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if charger.calls != 0 {
		t.Fatalf("charged %d times an invoice credits covered", charger.calls)
	}
	got, err := store.Get(ctx, tenantID, inv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != domain.InvoicePaid {
		t.Fatalf("status %s, want paid", got.Status)
	}
	if holdIsSet(t, db, inv.ID) {
		t.Error("the charge hold is still set after a settle that sent no charge")
	}
}
