package audit

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/api/timefilter"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

type Handler struct {
	logger *Logger
}

func NewHandler(logger *Logger) *Handler {
	return &Handler{logger: logger}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/filters", h.filters)
	return r
}

// filters returns the distinct action and resource_type values currently
// recorded for the tenant. The UI populates its filter dropdowns from this
// so new audit action types show up without a frontend release.
func (h *Handler) filters(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	actions, resourceTypes, err := h.logger.FilterOptions(r.Context(), tenantID)
	if err != nil {
		slog.ErrorContext(r.Context(), "audit filters", "error", err)
		respond.InternalError(w, r)
		return
	}
	// Clocks the LOG has seen — including ones ADR-086 teardown has already
	// deleted, which are the ones a forensic query is usually about.
	clocks, err := h.logger.SimClocks(r.Context(), tenantID)
	if err != nil {
		slog.ErrorContext(r.Context(), "audit filters: sim clocks", "error", err)
		respond.InternalError(w, r)
		return
	}
	if actions == nil {
		actions = []string{}
	}
	if resourceTypes == nil {
		resourceTypes = []string{}
	}
	if clocks == nil {
		clocks = []SimClock{}
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{
		"actions":        actions,
		"resource_types": resourceTypes,
		"test_clocks":    clocks,
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	dateFrom, dateTo, err := timefilter.ParseRange(r, "date_from", "date_to")
	if err != nil {
		respond.FromError(w, r, err, "audit_log")
		return
	}
	// Sim-time window. Same parser as the wall-clock range (RFC3339 or bare
	// YYYY-MM-DD) — an operator windowing a simulation types dates, and the
	// dates they mean are the SIMULATED ones.
	simFrom, simTo, err := timefilter.ParseRange(r, "sim_from", "sim_to")
	if err != nil {
		respond.FromError(w, r, err, "audit_log")
		return
	}

	// There is ONE sort axis: created_at. `?order=` is still validated rather
	// than ignored, because a client that asks for an ordering it does not get,
	// and then pages through the result, reads rows in an order it is not
	// expecting. See QueryFilter for why ordering by simulated time does not
	// exist (short version: within a clock it is the same order; across clocks
	// it is a timeline that never happened).
	switch order := r.URL.Query().Get("order"); order {
	case "", "created_at":
	default:
		respond.BadRequest(w, r, "invalid `order` — audit rows are ordered by created_at")
		return
	}

	filter := QueryFilter{
		ResourceType: r.URL.Query().Get("resource_type"),
		ResourceID:   r.URL.Query().Get("resource_id"),
		Action:       r.URL.Query().Get("action"),
		ActorID:      r.URL.Query().Get("actor_id"),
		DateFrom:     dateFrom,
		DateTo:       dateTo,
		TestClockID:  r.URL.Query().Get("test_clock_id"),
		SimFrom:      simFrom,
		SimTo:        simTo,
		Limit:        page.Limit,
		Offset:       page.Offset,
	}
	entries, total, err := h.logger.Query(r.Context(), tenantID, filter)
	if err != nil {
		slog.ErrorContext(r.Context(), "list audit log", "error", err)
		respond.InternalError(w, r)
		return
	}
	if entries == nil {
		entries = []domain.AuditEntry{}
	}

	respond.List(w, r, entries, total)
}
