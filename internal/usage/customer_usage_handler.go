package usage

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/api/timefilter"
	"github.com/gethardcount/hardcount/internal/auth"
)

// CustomerUsageHandler exposes GET /v1/customers/{id}/usage. Kept distinct
// from the existing usage.Handler (which owns /v1/usage-events ingest+list)
// because this surface composes across customer/subscription/pricing —
// the dependency set is materially different.
type CustomerUsageHandler struct {
	svc *CustomerUsageService
}

// NewCustomerUsageHandler wires a handler around a CustomerUsageService.
func NewCustomerUsageHandler(svc *CustomerUsageService) *CustomerUsageHandler {
	return &CustomerUsageHandler{svc: svc}
}

// CustomerUsageRoutes returns the sub-router. Mount at
// /v1/customers/{id}/usage with the requireRead guard (auth.PermUsageRead);
// the customer ID is read from chi.URLParam(r, "id") after the
// sibling-mount, mirroring the /customers/{id}/coupon precedent.
func (h *CustomerUsageHandler) CustomerUsageRoutes(requireRead func(http.Handler) http.Handler) chi.Router {
	r := chi.NewRouter()
	r.With(requireRead).Get("/", h.get)
	return r
}

func (h *CustomerUsageHandler) get(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "id")
	if customerID == "" {
		respond.BadRequest(w, r, "customer id is required")
		return
	}

	period, err := parseUsagePeriodQuery(r)
	if err != nil {
		respond.FromError(w, r, err, "customer_usage")
		return
	}

	result, err := h.svc.Get(r.Context(), tenantID, customerID, period)
	if err != nil {
		respond.FromError(w, r, err, "customer")
		return
	}

	respond.JSON(w, r, http.StatusOK, result)
}

// parseUsagePeriodQuery reads ?from= and ?to= from the request URL.
// Empty query → zero-valued CustomerUsagePeriod (service defaults to
// current cycle). Unparseable values surface as 400 via the handler's
// FromError path. Accepts both RFC3339 instants and bare YYYY-MM-DD
// dates via the shared timefilter helper so every operator endpoint
// reads dates the same way.
func parseUsagePeriodQuery(r *http.Request) (CustomerUsagePeriod, error) {
	from, to, err := timefilter.ParseRange(r, "from", "to")
	if err != nil {
		return CustomerUsagePeriod{}, err
	}
	return CustomerUsagePeriod{From: from, To: to}, nil
}
