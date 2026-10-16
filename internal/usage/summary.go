package usage

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/api/timefilter"
	"github.com/sagarsuperuser/velox/internal/auth"
)

// UsageSummary represents aggregated usage for a customer in a period.
type UsageSummary struct {
	CustomerID  string                     `json:"customer_id"`
	PeriodFrom  time.Time                  `json:"period_from"`
	PeriodTo    time.Time                  `json:"period_to"`
	Meters      map[string]decimal.Decimal `json:"meters"` // meter_id -> total quantity
	TotalEvents int                        `json:"total_events"`
}

// GetSummary aggregates usage for a customer over [from, to). The HTTP
// handler defaults the window to the UTC calendar month of the customer's
// effective now, which is not the billing period: an anniversary-billed
// subscription's period starts mid-month, and the tenant zone is ignored.
//
// Aggregation is done server-side via store.Aggregate (COUNT(*) +
// GROUP BY meter_id SUM) over the full filtered set. The previous
// List(Limit:10000)+Go-reduce undercounted: List clamps the limit to
// 1000, so any customer with >1000 events in the window reported a
// truncated total — wrong on both the per-meter quantities and the
// event count.
func (s *Service) GetSummary(ctx context.Context, tenantID, customerID string, from, to time.Time) (UsageSummary, error) {
	agg, err := s.store.Aggregate(ctx, ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
		From:       &from,
		To:         &to,
	})
	if err != nil {
		return UsageSummary{}, err
	}

	meters := make(map[string]decimal.Decimal, len(agg.ByMeter))
	for _, m := range agg.ByMeter {
		meters[m.MeterID] = m.Total
	}

	return UsageSummary{
		CustomerID:  customerID,
		PeriodFrom:  from,
		PeriodTo:    to,
		Meters:      meters,
		TotalEvents: agg.TotalEvents,
	}, nil
}

// SummaryHandler handles the usage summary HTTP endpoint.
func (h *Handler) SummaryRoutes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{customer_id}", h.getSummary)
	return r
}

func (h *Handler) getSummary(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "customer_id")

	// Default to the customer's current month — frozen_time's month when
	// the customer is pinned to a test clock (wall-clock's otherwise), so
	// the summary follows the simulated timeline after an Advance instead
	// of reporting the wall-clock month the clock has left behind.
	now := h.svc.effectiveNow(r.Context(), tenantID, customerID)
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)

	// Allow custom period via query params. Pre-2026-05-28 this path
	// silently swallowed parse errors and fell back to current-month —
	// so a typo in ?from= gave you the wrong window with no signal.
	// Now consistent with every other operator endpoint: bad input →
	// 400, valid date-only or RFC3339 input is honored.
	parsedFrom, parsedTo, err := timefilter.ParseRange(r, "from", "to")
	if err != nil {
		respond.FromError(w, r, err, "usage_summary")
		return
	}
	if !parsedFrom.IsZero() {
		from = parsedFrom
	}
	if !parsedTo.IsZero() {
		to = parsedTo
	}

	summary, err := h.svc.GetSummary(r.Context(), tenantID, customerID, from, to)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal_error"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(summary)
}
