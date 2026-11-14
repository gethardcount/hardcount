package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
)

// An invoice records whether its prices include tax, from the settings read
// that priced its lines. These tests drive every writer of an invoice whose
// lines are priced over the real server and Postgres, under both settings,
// and read invoices.tax_inclusive.

// newTaxFlagRig is the lag rig's meter, plans and customer, at 20% manual tax
// with the tenant's prices including tax or not.
func newTaxFlagRig(t *testing.T, timing string, inclusive bool) *lagRig {
	t.Helper()
	r := newLagRigBare(t, timing, 0)
	r.setTaxInclusive(inclusive)
	return r
}

func (r *lagRig) setTaxInclusive(inclusive bool) {
	r.t.Helper()
	if _, err := tenant.NewSettingsStore(r.db).Update(postgres.WithLivemode(context.Background(), false), r.tenantID, func(ts *domain.TenantSettings) error {
		ts.TaxProvider, ts.TaxRate, ts.TaxName, ts.TaxInclusive = "manual", 20, "VAT", inclusive
		return nil
	}); err != nil {
		r.t.Fatalf("set the tax: %v", err)
	}
}

// createSub creates the rig's subscription on plan A at `at`, with extra
// fields merged into the body.
func (r *lagRig) createSub(at time.Time, extra map[string]any) {
	r.t.Helper()
	body := map[string]any{
		"code": "tax-flag-sub", "display_name": "Tax Flag Sub", "customer_id": r.custID,
		"items": []map[string]any{{"plan_id": r.planA, "quantity": 1}},
	}
	for k, v := range extra {
		body[k] = v
	}
	sub := r.must(http.MethodPost, "/v1/subscriptions", at, body, http.StatusCreated)
	r.subID = sub["id"].(string)
	for _, it := range r.sub().Items {
		if it.PlanID == r.planA {
			r.itemA = it.ID
		}
	}
}

// onlyInvoice is the subscription's one invoice with the billing reason.
func (r *lagRig) onlyInvoice(reason domain.InvoiceBillingReason) string {
	r.t.Helper()
	var found []string
	invs := r.invoices()
	for _, inv := range invs {
		if inv.reason == string(reason) {
			found = append(found, inv.id)
		}
	}
	if len(found) != 1 {
		r.logInvoices(invs)
		r.t.Fatalf("%d invoices with billing reason %s, want 1", len(found), reason)
	}
	return found[0]
}

// taxInclusiveOf reads invoices.tax_inclusive.
func (r *lagRig) taxInclusiveOf(invoiceID string) bool {
	r.t.Helper()
	tx, err := r.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var got bool
	if err := tx.QueryRow(`SELECT tax_inclusive FROM invoices WHERE id = $1`, invoiceID).Scan(&got); err != nil {
		r.t.Fatalf("read tax_inclusive of %s: %v", invoiceID, err)
	}
	return got
}

// taxFlagWriters is every writer of an invoice whose lines are priced, one per
// billing reason (subscription_create has three). Each writes its invoice and
// returns its id.
var taxFlagWriters = []struct {
	name   string
	timing string
	write  func(r *lagRig) string
}{
	{"cycle close", "in_advance", func(r *lagRig) string {
		r.createSub(lagP0, nil)
		r.run(lagB)
		return r.onlyInvoice(domain.BillingReasonSubscriptionCycle)
	}},
	{"first invoice", "in_advance", func(r *lagRig) string {
		r.createSub(lagP0, nil)
		return r.onlyInvoice(domain.BillingReasonSubscriptionCreate)
	}},
	{"trial end", "in_advance", func(r *lagRig) string {
		r.createSub(lagP0, map[string]any{"trial_days": 14})
		r.expireTrials(lagP0.AddDate(0, 0, 15))
		return r.onlyInvoice(domain.BillingReasonSubscriptionCreate)
	}},
	{"swap to a yearly plan", "in_advance", func(r *lagRig) string {
		r.createSub(lagP0, nil)
		before := r.invoices()
		r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, lagP0.AddDate(0, 0, 14),
			map[string]any{"new_plan_id": r.planY, "immediate": true}, http.StatusOK)
		return r.invoiceWrittenSince(before).id
	}},
	{"cancel", "in_arrears", func(r *lagRig) string {
		r.createSub(lagP0, nil)
		r.ingest(lagP0.AddDate(0, 0, 5), 100)
		r.cancelNow(lagP0.AddDate(0, 0, 10))
		return r.onlyInvoice(domain.BillingReasonSubscriptionCancel)
	}},
	{"threshold", "in_advance", func(r *lagRig) string {
		r.createSub(lagP0, nil)
		r.ingest(time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC), 100)
		fireAt := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
		r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", fireAt.Add(-time.Hour),
			map[string]any{"amount_gte": 100}, http.StatusOK)
		if n := r.scanThresholds(fireAt); n != 1 {
			r.t.Fatalf("threshold scan fired %d invoices, want 1", n)
		}
		return r.onlyInvoice(domain.BillingReasonThreshold)
	}},
	{"item change", "in_advance", func(r *lagRig) string {
		r.createSub(lagP0, nil)
		r.payFirstInvoice(lagP0.Add(time.Hour))
		r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, lagP0.AddDate(0, 0, 14),
			map[string]any{"quantity": 2}, http.StatusOK)
		return r.onlyInvoice(domain.BillingReasonSubscriptionUpdate)
	}},
	{"manual invoice", "in_advance", func(r *lagRig) string {
		inv := r.must(http.MethodPost, "/v1/invoices", lagP0, map[string]any{
			"customer_id": r.custID,
			"line_items":  []map[string]any{{"description": "setup", "quantity": 1, "unit_amount_cents": 5000}},
		}, http.StatusCreated)
		return inv["id"].(string)
	}},
}

func TestInvoiceTaxInclusive_EveryWriterRecordsTheSetting(t *testing.T) {
	for _, inclusive := range []bool{false, true} {
		for _, w := range taxFlagWriters {
			t.Run(fmt.Sprintf("%s/inclusive=%v", w.name, inclusive), func(t *testing.T) {
				r := newTaxFlagRig(t, w.timing, inclusive)
				id := w.write(r)
				if got := r.taxInclusiveOf(id); got != inclusive {
					t.Errorf("tax_inclusive = %v on the %s invoice, want %v: the tenant's setting when its lines were priced", got, w.name, inclusive)
				}
			})
		}
	}
}

// refusingTaxResolver serves the tenant's tax provider, wrapped so that each
// Calculate fails while refuse is set.
type refusingTaxResolver struct {
	inner  *tax.Resolver
	refuse bool
}

func (r *refusingTaxResolver) Resolve(ctx context.Context, ts domain.TenantSettings) (tax.Provider, error) {
	prov, err := r.inner.Resolve(ctx, ts)
	if err != nil {
		return nil, err
	}
	return refusingProvider{Provider: prov, r: r}, nil
}

type refusingProvider struct {
	tax.Provider
	r *refusingTaxResolver
}

func (p refusingProvider) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	if p.r.refuse {
		return nil, errors.New("tax provider: 503 service unavailable")
	}
	return p.Provider.Calculate(ctx, req)
}

// An invoice whose tax calculation fails is kept with its tax pending, and it
// records whether its prices include tax all the same: the settings were read
// before the provider was called.
func TestInvoiceTaxInclusive_RecordedWhenTheTaxIsLeftPending(t *testing.T) {
	r := newTaxFlagRig(t, "in_advance", true)
	r.srv.BillingEngine.SetTaxProviderResolver(&refusingTaxResolver{inner: tax.NewResolver(nil), refuse: true})
	r.createSub(lagP0, nil)
	id := r.onlyInvoice(domain.BillingReasonSubscriptionCreate)
	if got := r.invoice(id)["tax_status"]; got != string(domain.InvoiceTaxPending) {
		t.Fatalf("tax_status = %v, want pending: the provider refused", got)
	}
	if !r.taxInclusiveOf(id) {
		t.Error("tax_inclusive = false on an invoice priced with tax included and its tax left pending")
	}
}

// The flag is written once, by the invoice's insert, and nothing after it
// changes it: not a line added to a draft, not finalize, not a tax retry, even
// with the tenant's setting switched in between.
func TestInvoiceTaxInclusive_WrittenOnceAndNeverUpdated(t *testing.T) {
	t.Run("a manual invoice, a line added and finalized after the switch", func(t *testing.T) {
		r := newTaxFlagRig(t, "in_advance", true)
		inv := r.must(http.MethodPost, "/v1/invoices", lagP0, map[string]any{
			"customer_id": r.custID,
			"line_items":  []map[string]any{{"description": "setup", "quantity": 1, "unit_amount_cents": 5000}},
		}, http.StatusCreated)
		id := inv["id"].(string)
		r.setTaxInclusive(false)
		r.must(http.MethodPost, "/v1/invoices/"+id+"/line-items", lagP0,
			map[string]any{"description": "extra", "quantity": 1, "unit_amount_cents": 1000}, http.StatusCreated)
		if !r.taxInclusiveOf(id) {
			t.Fatal("adding a line changed tax_inclusive")
		}
		r.must(http.MethodPost, "/v1/invoices/"+id+"/finalize", lagP0, nil, http.StatusOK)
		if !r.taxInclusiveOf(id) {
			t.Error("finalize changed tax_inclusive")
		}
	})

	t.Run("a pending invoice, its tax retried after the switch", func(t *testing.T) {
		r := newTaxFlagRig(t, "in_advance", true)
		resolver := &refusingTaxResolver{inner: tax.NewResolver(nil), refuse: true}
		r.srv.BillingEngine.SetTaxProviderResolver(resolver)
		r.createSub(lagP0, nil)
		id := r.onlyInvoice(domain.BillingReasonSubscriptionCreate)
		r.setTaxInclusive(false)
		resolver.refuse = false
		r.must(http.MethodPost, "/v1/invoices/"+id+"/retry-tax", lagP0.Add(time.Hour), nil, http.StatusOK)
		if got := r.invoice(id)["tax_status"]; got == string(domain.InvoiceTaxPending) {
			t.Fatalf("tax_status still pending after the retry; the retry did not run")
		}
		if !r.taxInclusiveOf(id) {
			t.Error("the tax retry changed tax_inclusive")
		}
	})
}

// The invoice answers with tax_inclusive, read-only: the terms it was
// created in, under both settings.
func TestInvoiceTaxInclusive_OnTheAPI(t *testing.T) {
	for _, inclusive := range []bool{false, true} {
		t.Run(fmt.Sprintf("inclusive=%v", inclusive), func(t *testing.T) {
			r := newTaxFlagRig(t, "in_advance", inclusive)
			inv := r.must(http.MethodPost, "/v1/invoices", lagP0, map[string]any{
				"customer_id": r.custID,
				"line_items":  []map[string]any{{"description": "setup", "quantity": 1, "unit_amount_cents": 5000}},
			}, http.StatusCreated)
			if got := inv["tax_inclusive"]; got != inclusive {
				t.Errorf("POST answers tax_inclusive %v, want %v", got, inclusive)
			}
			if got := r.invoice(inv["id"].(string))["tax_inclusive"]; got != inclusive {
				t.Errorf("GET answers tax_inclusive %v, want %v", got, inclusive)
			}
		})
	}
}
