package api

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testclock/testclocktest"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// reliefFailsOnce is the real relief credit-note job, except that its first
// call fails as if the note could not be issued.
type reliefFailsOnce struct {
	inner *creditnote.Service
	calls int
}

func (r *reliefFailsOnce) RetryPendingClawbackIssueForClock(ctx context.Context, tenantID, clockID string, frozen time.Time, batch int) (int, []error) {
	r.calls++
	if r.calls == 1 {
		return 0, []error{errors.New("relief credit note could not be issued")}
	}
	return r.inner.RetryPendingClawbackIssueForClock(ctx, tenantID, clockID, frozen, batch)
}

// TestE2E_TestClock_FailedReliefStopsTheCharge: an invoice waiting to be
// charged has a relief credit note waiting to be issued against it. When the
// relief job fails, the advance stops before the card charge, so the card is
// not charged the full amount. Retry advance issues the note, then charges the
// lower amount. The relief and charge jobs are the real ones; Stripe is a stub
// that records each charge.
func TestE2E_TestClock_FailedReliefStopsTheCharge(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.Real(), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantID := testutil.CreateTestTenant(t, db, "Relief Before Charge")
	key := createAPIKeyWithMode(t, db, tenantID, false)
	call := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{bearer: key, body: body})
	}
	if _, err := tenantstripe.NewStore(db).Upsert(callerCtx(tenantID, false), tenantstripe.UpsertInput{
		TenantID: tenantID, Livemode: false, SecretKey: "sk_test_relief_charge", PublishableKey: "pk_test_relief_charge",
	}); err != nil {
		t.Fatalf("connect stripe: %v", err)
	}
	stripe := installStripeStub(t)
	stripe.extra = func(call string, form url.Values) (int, string, bool) {
		if call == "POST /v1/payment_intents" {
			return 200, fmt.Sprintf(`{"id":"pi_relief_charge","object":"payment_intent","status":"succeeded","amount":%s,"currency":"usd","latest_charge":"ch_relief_charge"}`, form.Get("amount")), true
		}
		return 0, "", false
	}

	frozen := time.Now().UTC().AddDate(0, 0, -10).Truncate(time.Second)
	clk := flowStr(call("POST", "/v1/test-clocks", map[string]any{"name": "relief before charge", "frozen_time": frozen}).want(t, 201, "create clock").body, "id")
	cust := flowStr(call("POST", "/v1/customers", map[string]any{
		"external_id": "relief-charge", "display_name": "Relief Charge", "email": "relief@charge.test", "test_clock_id": clk,
	}).want(t, 201, "create customer").body, "id")

	ctx := postgres.WithLivemode(context.Background(), false)
	inv, err := invoice.NewPostgresStore(db).CreateWithLineItems(ctx, tenantID, domain.Invoice{
		CustomerID: cust, InvoiceNumber: "INV-RELIEF-CHARGE", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, Currency: "USD",
		SubtotalCents: 5000, TotalAmountCents: 5000, AmountDueCents: 5000,
		BillingPeriodStart: frozen.AddDate(0, -1, 0), BillingPeriodEnd: frozen, IssuedAt: &frozen,
	}, nil)
	if err != nil {
		t.Fatalf("invoice: %v", err)
	}
	note, err := creditnote.NewPostgresStore(db).Create(ctx, tenantID, domain.CreditNote{
		InvoiceID: inv.ID, CustomerID: cust, CreditNoteNumber: "CN-RELIEF-CHARGE",
		Status: domain.CreditNoteDraft, Reason: "subscription_cancellation",
		SubtotalCents: 2000, TotalCents: 2000, Currency: "USD", RefundStatus: domain.RefundNone, IssuePending: true,
	})
	if err != nil {
		t.Fatalf("relief draft: %v", err)
	}
	seedBypass(t, db, `UPDATE customers SET stripe_customer_id = 'cus_relief_charge' WHERE id = $1`, cust)
	seedBypass(t, db, `INSERT INTO payment_methods (id, tenant_id, customer_id, stripe_payment_method_id, type, is_default, livemode)
		VALUES ('hc_pm_relief_charge', $1, $2, 'pm_relief_charge', 'card', true, false)`, tenantID, cust)
	seedBypass(t, db, `UPDATE invoices SET auto_charge_pending = true, is_simulated = true WHERE id = $1`, inv.ID)
	seedBypass(t, db, `UPDATE credit_notes SET is_simulated = true WHERE id = $1`, note.ID)

	clockDeps := testclocktest.Deps(srv.BillingEngine)
	clockDeps.Relief = &reliefFailsOnce{inner: srv.CreditNoteSvc}
	svc := testclock.NewService(testclock.NewPostgresStore(db), clockDeps)
	amountDue := func() int64 {
		got, err := srv.InvoiceSvc.Get(callerCtx(tenantID, false), tenantID, inv.ID)
		if err != nil {
			t.Fatalf("get invoice: %v", err)
		}
		return got.AmountDueCents
	}

	call("POST", "/v1/test-clocks/"+clk+"/advance", map[string]any{"frozen_time": frozen.Add(time.Hour)}).want(t, 200, "advance")
	testclocktest.Drain(t, svc)
	if st := flowStr(call("GET", "/v1/test-clocks/"+clk, nil).want(t, 200, "get clock").body, "status"); st != "internal_failure" {
		t.Fatalf("clock after the failed relief = %q, want internal_failure", st)
	}
	if n := stripe.count("POST /v1/payment_intents"); n != 0 {
		t.Fatalf("charges after the failed relief = %d, want 0: the card was charged the full amount", n)
	}
	if due := amountDue(); due != 5000 {
		t.Errorf("amount due after the failed relief = %d, want 5000", due)
	}

	call("POST", "/v1/test-clocks/"+clk+"/retry-advance", nil).want(t, 200, "retry advance")
	testclocktest.DrainReady(t, svc, tenantID, clk)
	if issued, err := creditnote.NewPostgresStore(db).Get(ctx, tenantID, note.ID); err != nil || issued.Status != domain.CreditNoteIssued {
		t.Errorf("relief note after the retry: status %q err %v, want issued", issued.Status, err)
	}
	if n := stripe.count("POST /v1/payment_intents"); n != 1 {
		t.Fatalf("charges after the retry = %d, want 1", n)
	}
	if amount := stripe.form("POST /v1/payment_intents").Get("amount"); amount != "3000" {
		t.Errorf("charge amount = %s, want 3000: the note lowers the invoice before it is charged", amount)
	}
}
