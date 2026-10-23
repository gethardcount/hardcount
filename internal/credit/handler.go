package credit

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/grant", h.grant)
	r.Post("/adjust", h.adjust)
	r.Get("/balances", h.listBalances)
	r.Get("/balance/{customer_id}", h.getBalance)
	r.Get("/balance/{customer_id}/live", h.getLiveBalance)
	r.Get("/ledger/{customer_id}", h.listEntries)
	r.Get("/grants/{customer_id}", h.listGrants)
	return r
}

// GrantRequest is the POST /v1/credits/grant body. It carries only the
// documented fields. GrantInput also has invoice and source_* ids the engine
// sets on its own grants; decoding the body straight into it let a caller
// stamp those onto a ledger row unchecked, including ids from another tenant
// or the other mode.
type GrantRequest struct {
	CustomerID  string           `json:"customer_id"`
	Currency    string           `json:"currency"`
	AmountCents int64            `json:"amount_cents"`
	Description string           `json:"description"`
	ExpiresAt   *time.Time       `json:"expires_at,omitempty"`
	GrantKind   domain.GrantKind `json:"grant_kind,omitempty"`
}

func (h *Handler) grant(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var req GrantRequest
	if !respond.DecodeStrict(w, r, &req) {
		return
	}

	entry, err := h.svc.Grant(r.Context(), tenantID, GrantInput{
		CustomerID:  req.CustomerID,
		Currency:    req.Currency,
		AmountCents: req.AmountCents,
		Description: req.Description,
		ExpiresAt:   req.ExpiresAt,
		GrantKind:   req.GrantKind,
	})
	if err != nil {
		respond.FromError(w, r, err, "credit")
		return
	}

	mw.RecordCreditOperation("grant")

	respond.JSON(w, r, http.StatusCreated, entry)
}

func (h *Handler) adjust(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	var input AdjustInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	entry, err := h.svc.Adjust(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "credit")
		return
	}

	mw.RecordCreditOperation("adjustment")

	respond.JSON(w, r, http.StatusCreated, entry)
}

func (h *Handler) listBalances(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())

	balances, err := h.svc.ListBalances(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list credit balances", "error", err)
		return
	}
	if balances == nil {
		balances = []domain.CreditBalance{}
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{"data": balances})
}

func (h *Handler) getBalance(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "customer_id")

	balances, err := h.svc.GetBalance(r.Context(), tenantID, customerID)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "customer")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get credit balance", "error", err)
		return
	}
	if balances == nil {
		balances = []domain.CreditBalance{}
	}

	// One balance per currency the customer holds credit in (ADR-129).
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": balances})
}

// getLiveBalance answers "how much credit does this customer have left right
// now": the settled balance less the usage no invoice has billed yet. It runs
// a usage aggregation per meter, so integrations poll it on an interval and
// cache the answer (docs/stop-usage-when-credit-runs-out.md).
func (h *Handler) getLiveBalance(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "customer_id")

	live, err := h.svc.LiveBalance(r.Context(), tenantID, customerID)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "customer")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get live credit balance", "error", err, "customer_id", customerID)
		return
	}

	respond.JSON(w, r, http.StatusOK, live)
}

// listGrants is the commit/promo burndown: per-grant granted/drawn/
// remaining/expiry + kind subtotals — the answer to finance's "how much
// of the $50k commit is drawn, and when does it expire?" without
// reconstructing it from the raw ledger.
func (h *Handler) listGrants(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "customer_id")
	includeExhausted := r.URL.Query().Get("include_exhausted") == "true"

	resp, err := h.svc.ListGrants(r.Context(), tenantID, customerID, includeExhausted)
	if err != nil {
		respond.FromError(w, r, err, "customer")
		return
	}
	respond.JSON(w, r, http.StatusOK, resp)
}

func (h *Handler) listEntries(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	customerID := chi.URLParam(r, "customer_id")

	// limit/offset were silently unparsed pre-fix: the store's default-50
	// cap made every consumer (pagination, type filter, CSV export)
	// operate on a truncated slice presented as complete. Clamp mirrors
	// the store (1..100, default 50); offset >= 0.
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	filter := ListFilter{
		TenantID:   tenantID,
		CustomerID: customerID,
		EntryType:  r.URL.Query().Get("entry_type"),
		Sort:       r.URL.Query().Get("sort"),
		SortDir:    r.URL.Query().Get("dir"),
		Limit:      limit,
		Offset:     offset,
	}
	entries, err := h.svc.ListEntries(r.Context(), filter)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "customer")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list credit entries", "error", err)
		return
	}
	if entries == nil {
		entries = []domain.CreditLedgerEntry{}
	}
	total, err := h.svc.CountEntries(r.Context(), filter)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "count credit entries", "error", err)
		return
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{"data": entries, "total": total})
}
