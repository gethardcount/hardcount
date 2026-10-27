package pricing

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// AuditWriter is the narrow audit surface the pricing handler uses.
type AuditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

type Handler struct {
	svc         *Service
	auditLogger AuditWriter
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// SetAuditLogger wires audit on the pricing mutations the handler records
// (plan and meter create, plan PATCH, price overrides). Price writes and
// meter PATCH record theirs on their own transaction (Service.SetAuditLogger).
func (h *Handler) SetAuditLogger(a AuditWriter) {
	h.auditLogger = a
}

func (h *Handler) MeterRoutes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.createMeter)
	r.Get("/", h.listMeters)
	r.Get("/{id}", h.getMeter)
	r.Patch("/{id}", h.updateMeter)
	return r
}

func (h *Handler) PlanRoutes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.createPlan)
	r.Get("/", h.listPlans)
	r.Get("/{id}", h.getPlan)
	r.Patch("/{id}", h.updatePlan)
	r.Get("/{id}/prices", h.listPrices)
	r.Post("/{id}/prices", h.addPrice)
	r.Patch("/{id}/prices/{price_id}", h.editPrice)
	r.Delete("/{id}/prices/{price_id}", h.removePrice)
	return r
}

func (h *Handler) OverrideRoutes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.createOverride)
	r.Get("/", h.listOverrides)
	r.Delete("/{id}", h.deleteOverride)
	return r
}

// ---------------------------------------------------------------------------
// Price overrides
// ---------------------------------------------------------------------------

func (h *Handler) createOverride(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	// Strict: 0 is a valid free rate, so a dropped rate key would be a $0 price.
	var req overrideRequest
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	input, err := req.input()
	if err != nil {
		respond.FromError(w, r, err, "price_override")
		return
	}
	override, err := h.svc.CreateOverride(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "price_override")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionCreate, "price_override", override.ID, "", map[string]any{
			"customer_id": override.CustomerID,
			"price_id":    override.PriceID,
		})
	}
	respond.JSON(w, r, http.StatusCreated, override)
}

func (h *Handler) listOverrides(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	q := r.URL.Query()
	overrides, err := h.svc.ListOverrides(r.Context(), tenantID, q.Get("customer_id"), q.Get("price_id"))
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list overrides", "error", err)
		return
	}
	if overrides == nil {
		overrides = []domain.CustomerPriceOverride{}
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": overrides})
}

// deleteOverride ends a negotiated price from the customer's next billing
// period; the period in flight keeps it.
func (h *Handler) deleteOverride(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	o, err := h.svc.DeleteOverride(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "price_override")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionDelete, "price_override", id, "", map[string]any{
			"customer_id": o.CustomerID,
			"price_id":    o.PriceID,
		})
	}
	respond.JSON(w, r, http.StatusOK, o)
}

// ---------------------------------------------------------------------------
// Meters
// ---------------------------------------------------------------------------

func (h *Handler) createMeter(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var input CreateMeterInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}
	meter, err := h.svc.CreateMeter(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "meter")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionCreate, "meter", meter.ID, meter.Name, nil)
	}
	h.respondMeter(w, r, http.StatusCreated, tenantID, meter, true)
}

// meterResponse is one meter as POST, GET and PATCH /v1/meters answer it:
// the meter and whether it has usage events. Once it has, its aggregation is
// fixed (ADR-130 §2), and the dashboard says so before an edit. Held rows do
// not count. The list leaves has_usage out: proving a meter has no events
// scans the account's events (ADR-130 §18), once per meter. A create or
// update whose has_usage read fails leaves it out too (respondMeter).
type meterResponse struct {
	domain.Meter
	HasUsage *bool `json:"has_usage,omitempty"`
}

// respondMeter writes meter with has_usage.
//
// After a create or update (wrote), the write has committed, so a failed
// has_usage read must not turn it into a 500: a client that retried the
// create would get 409 "already exists" for the meter its first call made.
// The answer is the write's status with has_usage left out, not guessed.
func (h *Handler) respondMeter(w http.ResponseWriter, r *http.Request, status int, tenantID string, meter domain.Meter, wrote bool) {
	has, err := h.svc.MeterHasUsage(r.Context(), tenantID, meter)
	if err != nil {
		slog.ErrorContext(r.Context(), "read whether the meter has usage", "meter_id", meter.ID, "error", err)
		if !wrote {
			respond.InternalError(w, r)
			return
		}
		respond.JSON(w, r, status, meterResponse{Meter: meter})
		return
	}
	respond.JSON(w, r, status, meterResponse{Meter: meter, HasUsage: &has})
}

func (h *Handler) listMeters(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	meters, err := h.svc.ListMeters(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list meters", "error", err)
		return
	}
	if meters == nil {
		meters = []domain.Meter{}
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": meters})
}

func (h *Handler) getMeter(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	meter, err := h.svc.GetMeter(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "meter")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get meter", "error", err)
		return
	}
	h.respondMeter(w, r, http.StatusOK, tenantID, meter, false)
}

// updateMeter patches a meter's name, unit or aggregation.
func (h *Handler) updateMeter(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input UpdateMeterInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}
	meter, err := h.svc.UpdateMeter(r.Context(), tenantID, id, input)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "meter")
		return
	}
	if err != nil {
		respond.FromError(w, r, err, "meter")
		return
	}
	h.respondMeter(w, r, http.StatusOK, tenantID, meter, true)
}

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

// planResponse is a plan as the API answers it: the plan and its prices from
// now on. GET /v1/plans/{id} adds whether the plan was ever used, which
// decides when a price edit applies (ADR-130 §5), and the number of live
// subscriptions that keep its base fee fixed (ADR-034).
type planResponse struct {
	domain.Plan
	Prices                []domain.PlanPrice `json:"prices"`
	Used                  *bool              `json:"used,omitempty"`
	LiveSubscriptionCount *int               `json:"live_subscription_count,omitempty"`
}

func pricesOrEmpty(p []domain.PlanPrice) []domain.PlanPrice {
	if p == nil {
		return []domain.PlanPrice{}
	}
	return p
}

func (h *Handler) createPlan(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var req planRequest
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	input, err := req.input()
	if err != nil {
		respond.FromError(w, r, err, "plan")
		return
	}
	plan, prices, err := h.svc.CreatePlan(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "plan")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionCreate, "plan", plan.ID, plan.Name, map[string]any{"prices": len(prices)})
	}
	used := false
	live := 0
	respond.JSON(w, r, http.StatusCreated, planResponse{Plan: plan, Prices: pricesOrEmpty(prices), Used: &used, LiveSubscriptionCount: &live})
}

func (h *Handler) listPlans(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	plans, err := h.svc.ListPlans(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list plans", "error", err)
		return
	}
	ids := make([]string, 0, len(plans))
	for _, p := range plans {
		ids = append(ids, p.ID)
	}
	prices, err := h.svc.ListPrices(r.Context(), tenantID, ids)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list plans: prices", "error", err)
		return
	}
	out := make([]planResponse, 0, len(plans))
	for _, p := range plans {
		out = append(out, planResponse{Plan: p, Prices: pricesOrEmpty(prices[p.ID])})
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": out})
}

func (h *Handler) getPlan(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	plan, err := h.svc.GetPlan(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "plan")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get plan", "error", err)
		return
	}
	h.respondPlan(w, r, http.StatusOK, tenantID, plan)
}

// respondPlan writes plan with its prices, used and the live count.
func (h *Handler) respondPlan(w http.ResponseWriter, r *http.Request, status int, tenantID string, plan domain.Plan) {
	prices, err := h.svc.ListPrices(r.Context(), tenantID, []string{plan.ID})
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get plan: prices", "error", err)
		return
	}
	used, err := h.svc.PlanUsed(r.Context(), tenantID, plan.ID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get plan: used", "error", err)
		return
	}
	live, err := h.svc.LiveSubscriptionCount(r.Context(), tenantID, plan.ID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get plan: live subscriptions", "error", err)
		return
	}
	respond.JSON(w, r, status, planResponse{Plan: plan, Prices: pricesOrEmpty(prices[plan.ID]), Used: &used, LiveSubscriptionCount: &live})
}

func (h *Handler) updatePlan(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input UpdatePlanInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}
	plan, err := h.svc.UpdatePlan(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "plan")
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionUpdate, "plan", plan.ID, plan.Name, nil)
	}
	h.respondPlan(w, r, http.StatusOK, tenantID, plan)
}

// ---------------------------------------------------------------------------
// Plan prices
// ---------------------------------------------------------------------------

// priceWriteResponse is how a price write answers: the price, and when the
// write applies (ADR-130 §5). A removal also says it removed the price and
// which customer overrides it ended.
type priceWriteResponse struct {
	domain.PlanPrice
	Effective      Effective       `json:"effective"`
	Removed        bool            `json:"removed,omitempty"`
	EndedOverrides []EndedOverride `json:"ended_overrides,omitempty"`
}

func writeResponse(pw PriceWrite) priceWriteResponse {
	out := priceWriteResponse{PlanPrice: pw.Price, Effective: pw.Effective, Removed: pw.Removed}
	if pw.Removed {
		out.EndedOverrides = pw.EndedOverrides
		if out.EndedOverrides == nil {
			out.EndedOverrides = []EndedOverride{}
		}
	}
	return out
}

func (h *Handler) listPrices(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	prices, err := h.svc.ListPlanPrices(r.Context(), tenantID, chi.URLParam(r, "id"))
	if err != nil {
		respond.FromError(w, r, err, "plan")
		return
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": pricesOrEmpty(prices)})
}

func (h *Handler) addPrice(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	// Strict: 0 is a valid free rate, so a dropped rate key would be a $0 price.
	var req priceRequest
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	in, err := req.input("")
	if err != nil {
		respond.FromError(w, r, err, "price")
		return
	}
	pw, err := h.svc.AddPrice(r.Context(), tenantID, chi.URLParam(r, "id"), in)
	if err != nil {
		respond.FromError(w, r, err, "price")
		return
	}
	respond.JSON(w, r, http.StatusCreated, writeResponse(pw))
}

func (h *Handler) editPrice(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	var req priceEditRequest
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	shape, err := req.shape()
	if err != nil {
		respond.FromError(w, r, err, "price")
		return
	}
	pw, err := h.svc.EditPrice(r.Context(), tenantID, chi.URLParam(r, "id"), chi.URLParam(r, "price_id"), shape)
	if err != nil {
		respond.FromError(w, r, err, "price")
		return
	}
	respond.JSON(w, r, http.StatusOK, writeResponse(pw))
}

func (h *Handler) removePrice(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	pw, err := h.svc.RemovePrice(r.Context(), tenantID, chi.URLParam(r, "id"), chi.URLParam(r, "price_id"))
	if err != nil {
		respond.FromError(w, r, err, "price")
		return
	}
	respond.JSON(w, r, http.StatusOK, writeResponse(pw))
}
