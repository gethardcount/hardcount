package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/subscription"
)

// customerDetailHandler serves the per-customer overview on the operator
// dashboard's Customer Detail page. The page lists a customer's invoices and
// subscriptions through GET /v1/invoices?customer_id= and
// GET /v1/subscriptions?customer_id=. The handler crosses domain
// boundaries, so it lives in the API package. That keeps the domain packages
// independent.
//
// It is an operator route (PermCustomerRead) under /v1/customers/{id}/*. It
// is not the customer self-serve portal, which ADR-051 removed.
type customerDetailHandler struct {
	customers *customer.PostgresStore
	subs      *subscription.PostgresStore
	invoices  *invoice.PostgresStore
}

func newCustomerDetailHandler(customers *customer.PostgresStore, subs *subscription.PostgresStore, invoices *invoice.PostgresStore) *customerDetailHandler {
	return &customerDetailHandler{customers: customers, subs: subs, invoices: invoices}
}

// requireCustomer resolves the path's customer id to an existing row, and
// writes the 404 itself when there is none. The overview runs tenant-scoped
// sub-queries that all rightly return empty for an unknown id. Without this
// lookup, a mistyped or cross-tenant id would read as "this customer has
// nothing" (200, an empty shell) instead of "no such customer". The empty
// shell is not a leak, and it does not reveal whether an id exists: a
// cross-tenant id and a made-up one behave the same. But a 404 is an answer
// a caller can act on.
func (h *customerDetailHandler) requireCustomer(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "customer_id")
	if _, err := h.customers.Get(r.Context(), tenantID, customerID); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "customer")
		} else {
			respond.InternalError(w, r)
		}
		return "", "", false
	}
	return tenantID, customerID, true
}

// overview returns one view of a customer: active subscriptions, the 5 most
// recent invoices and the outstanding balance. It does not return a usage
// summary. requireCustomer answers 404 for an unknown id, instead of a 200
// with an empty shell.
func (h *customerDetailHandler) overview(w http.ResponseWriter, r *http.Request) {
	tenantID, customerID, ok := h.requireCustomer(w, r)
	if !ok {
		return
	}

	subs, _, _ := h.subs.List(r.Context(), subscription.ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
		Status:     "active",
	})

	invoices, _, _ := h.invoices.List(r.Context(), invoice.ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
		Limit:      5,
	})

	if subs == nil {
		subs = []domain.Subscription{}
	}
	if invoices == nil {
		invoices = []domain.Invoice{}
	}

	// The outstanding balance is what this customer owes (accounts
	// receivable, AR). Sum all their unpaid invoices, not only the 5 most
	// recent in the recent_invoices slice. Stripe, Lago, Chargebee and
	// Recurly all show it on the customer page, so operators see the total
	// AR at a glance instead of adding it up by hand. A read failure shows
	// as zero (best-effort), and the rest of the overview still renders.
	outstanding, _ := h.invoices.GetOutstandingBalance(r.Context(), tenantID, customerID)
	if outstanding.ByCurrency == nil {
		outstanding.ByCurrency = []invoice.CurrencyAmount{}
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"customer_id":          customerID,
		"active_subscriptions": subs,
		"recent_invoices":      invoices,
		"outstanding_balance":  outstanding,
	})
}
