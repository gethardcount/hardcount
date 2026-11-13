package testclock

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Handler exposes /v1/test-clocks. Auth middleware upstream enforces that the
// caller holds a test-mode key (livemode=false); the service itself adds a
// second guard so a misconfigured auth chain can't slip live writes through.
type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(h.requireTestMode)
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	r.Get("/{id}/subscriptions", h.listSubscriptions)
	r.Get("/{id}/customers", h.listAttachedCustomers)
	r.Post("/{id}/advance", h.advance)
	r.Post("/{id}/retry-advance", h.retryAdvance)
	r.Delete("/{id}", h.delete)
	return r
}

// requireTestMode short-circuits requests from live-mode keys with 403. The
// underlying table enforces livemode=false via CHECK, so a live call would
// eventually fail anyway — this makes the failure immediate and explains why.
func (h *Handler) requireTestMode(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		live, err := postgres.Livemode(r.Context())
		if err != nil {
			respond.FromError(w, r, err, "test_clock")
			return
		}
		if live {
			respond.Forbidden(w, r, "test clocks are only available in test mode — use a test-mode API key (hc_secret_test_...)")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var input CreateInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	clk, err := h.svc.Create(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "test_clock")
		return
	}
	respond.JSON(w, r, http.StatusCreated, clk)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}
	clocks, total, err := h.svc.List(r.Context(), tenantID, page.Limit, page.Offset)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list test clocks", "error", err)
		return
	}
	respond.List(w, r, clocks, total)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	clk, err := h.svc.Get(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "test_clock")
		return
	}
	respond.JSON(w, r, http.StatusOK, clk)
}

// listSubscriptions returns a page of the subscriptions pinned to the clock,
// including their customer for one-shot rendering on the clock detail
// page. The dashboard uses the embedded customer_id to compose the
// "customers in this simulation" view without a second round-trip.
func (h *Handler) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	subs, total, err := h.svc.ListSubscriptions(r.Context(), tenantID, id, page.Limit, page.Offset)
	if err != nil {
		respond.FromError(w, r, err, "test_clock")
		return
	}
	respond.List(w, r, subs, total)
}

// listAttachedCustomers returns customers attached to this clock —
// Stripe-parity surface (ADR-027 Tier 3). The clock detail page uses
// this to render the "Attached customers" section, mirroring Stripe
// Test Clock detail's UI.
func (h *Handler) listAttachedCustomers(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}

	customers, total, err := h.svc.ListAttachedCustomers(r.Context(), tenantID, id, page.Limit, page.Offset)
	if err != nil {
		respond.FromError(w, r, err, "test_clock")
		return
	}
	respond.List(w, r, customers, total)
}

func (h *Handler) advance(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	var input AdvanceInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	clk, err := h.svc.Advance(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "test_clock")
		return
	}
	respond.JSON(w, r, http.StatusOK, clk)
}

// retryAdvance resumes a clock parked at status='internal_failure'
// after a prior catchup error. The catchup loop is idempotent on
// subs whose next_billing_at <= frozen_time, so resuming from
// where the previous attempt stopped is safe by construction.
// 200 with the clock now in 'advancing' on success; 409 when the
// clock isn't in internal_failure (refuses to retry from ready
// or advancing). ADR-018.
func (h *Handler) retryAdvance(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	clk, err := h.svc.RetryAdvance(r.Context(), tenantID, id)
	if err != nil {
		respond.FromError(w, r, err, "test_clock")
		return
	}
	respond.JSON(w, r, http.StatusOK, clk)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	id := chi.URLParam(r, "id")

	if err := h.svc.Delete(r.Context(), tenantID, id); err != nil {
		respond.FromError(w, r, err, "test_clock")
		return
	}
	respond.JSON(w, r, http.StatusOK, map[string]string{"status": "deleted"})
}
