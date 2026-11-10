package billing_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
)

// reversalDouble stands in for Stripe's tax-transaction reversal over HTTP,
// and is the Stripe Tax provider's client resolver. connected=false is a
// tenant with no Stripe credentials.
type reversalDouble struct {
	client    *stripe.Client
	connected bool

	mu    sync.Mutex
	calls int
}

func newReversalDouble(t *testing.T) *reversalDouble {
	t.Helper()
	d := &reversalDouble{connected: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tax/transactions/create_reversal" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		d.mu.Lock()
		d.calls++
		n := d.calls
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"tax_rev_%d","object":"tax.transaction","type":"reversal"}`, n)
	}))
	t.Cleanup(srv.Close)
	be := stripe.GetBackendWithConfig(stripe.APIBackend, &stripe.BackendConfig{URL: stripe.String(srv.URL), MaxNetworkRetries: stripe.Int64(0)})
	d.client = stripe.NewClient("sk_test_fake", stripe.WithBackends(&stripe.Backends{API: be, Connect: be, Uploads: be, MeterEvents: be}))
	return d
}

func (d *reversalDouble) ResolveCtx(context.Context) (*stripe.Client, error) {
	if !d.connected {
		return nil, nil
	}
	return d.client, nil
}

func (d *reversalDouble) reversals() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// switchedTenant is a tenant that taxed an invoice with Stripe Tax, recorded
// its tax transaction, and has since switched its tax provider to now. The
// engine is the real one, behind the real resolver and Stripe Tax provider.
func switchedTenant(t *testing.T, name, now string) (f *thresholdFixture, ctx context.Context, svc *invoice.Service, stripeTax *reversalDouble, invoiceID string) {
	t.Helper()
	f = newThresholdFixture(t, name)
	ctx = postgres.WithLivemode(context.Background(), false)
	stripeTax = newReversalDouble(t)
	f.engine.SetTaxProviderResolver(tax.NewResolver(stripeTax))

	ts, err := f.settings.Get(ctx, f.tenantID)
	if err != nil {
		t.Fatalf("get settings: %v", err)
	}
	ts.TaxProvider = now
	if _, err := f.settings.Upsert(ctx, ts); err != nil {
		t.Fatalf("switch tax provider to %s: %v", now, err)
	}

	issued := time.Now().UTC()
	inv, err := f.invStore.CreateWithLineItems(ctx, f.tenantID, domain.Invoice{
		CustomerID: f.customerID, InvoiceNumber: "INV-SWITCHED-1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending, Currency: "USD",
		SubtotalCents:    10000,
		TaxFacts:         domain.TaxFacts{TaxAmountCents: 1000, TaxProvider: "stripe_tax", TaxStatus: domain.InvoiceTaxOK},
		TotalAmountCents: 11000, AmountDueCents: 11000,
		BillingPeriodStart: issued.Add(-30 * 24 * time.Hour), BillingPeriodEnd: issued,
		IssuedAt: &issued,
	}, nil)
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if err := f.invStore.SetTaxTransaction(ctx, f.tenantID, inv.ID, "tax_upstream_switched"); err != nil {
		t.Fatalf("record the tax transaction: %v", err)
	}
	svc = invoice.NewService(f.invStore, clock.Real(), f.settings)
	svc.SetTaxReverser(f.engine)
	return f, ctx, svc, stripeTax, inv.ID
}

func pendingTaxReversal(t *testing.T, f *thresholdFixture, ctx context.Context, invoiceID string) bool {
	t.Helper()
	pending, err := f.invStore.ListPendingTaxReversal(ctx, 500, false)
	if err != nil {
		t.Fatalf("ListPendingTaxReversal: %v", err)
	}
	for _, inv := range pending {
		if inv.ID == invoiceID {
			return true
		}
	}
	return false
}

// A tenant switches from Stripe Tax to another provider, then voids an
// invoice Stripe Tax taxed. The reversal must be made at Stripe: one
// create_reversal call, and only then is the invoice marked reversed. Sent to
// the tenant's current provider, the reversal did nothing, the invoice was
// marked reversed, and the sale stayed on the Stripe Tax report.
func TestVoid_AfterSwitchingTaxProvider_ReversesAtStripe(t *testing.T) {
	for _, now := range []string{"manual", "none"} {
		t.Run("switched to "+now, func(t *testing.T) {
			f, ctx, svc, stripeTax, invoiceID := switchedTenant(t, "Tax Switched "+now, now)

			if _, err := svc.Void(ctx, f.tenantID, invoiceID); err != nil {
				t.Fatalf("void: %v", err)
			}
			if n := stripeTax.reversals(); n != 1 {
				t.Fatalf("create_reversal calls at Stripe = %d, want 1: the tax Stripe recorded must be reversed there", n)
			}
			got, err := f.invStore.Get(ctx, f.tenantID, invoiceID)
			if err != nil {
				t.Fatalf("get invoice: %v", err)
			}
			if got.Status != domain.InvoiceVoided || got.TaxReversedAt == nil {
				t.Fatalf("after the void: status=%s tax_reversed_at=%v, want voided and stamped", got.Status, got.TaxReversedAt)
			}
			if pendingTaxReversal(t, f, ctx, invoiceID) {
				t.Fatal("a reversed invoice must leave the pending-reversal list")
			}
		})
	}
}

// The same tenant has also disconnected Stripe. The void goes through, but
// the reversal cannot be made: the invoice must not be marked reversed, and
// it stays listed so the reversal is made once Stripe is connected again.
func TestVoid_AfterSwitchingTaxProvider_StripeDisconnected_StaysOwed(t *testing.T) {
	f, ctx, svc, stripeTax, invoiceID := switchedTenant(t, "Tax Switched Disconnected", "manual")
	stripeTax.connected = false

	if _, err := svc.Void(ctx, f.tenantID, invoiceID); err != nil {
		t.Fatalf("void: %v (a failed tax reversal must not fail the void)", err)
	}
	got, err := f.invStore.Get(ctx, f.tenantID, invoiceID)
	if err != nil {
		t.Fatalf("get invoice: %v", err)
	}
	if got.Status != domain.InvoiceVoided {
		t.Fatalf("status = %s, want voided", got.Status)
	}
	if got.TaxReversedAt != nil {
		t.Fatal("the invoice is marked reversed although Stripe was never asked")
	}
	if !pendingTaxReversal(t, f, ctx, invoiceID) {
		t.Fatal("the unreversed invoice must stay in the pending-reversal list")
	}
	if n := stripeTax.reversals(); n != 0 {
		t.Fatalf("create_reversal calls = %d with Stripe disconnected, want 0", n)
	}
}
