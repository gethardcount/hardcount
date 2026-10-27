package tenant

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
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
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Get("/{id}", h.get)
	return r
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var input CreateInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	tenant, err := h.svc.Create(r.Context(), input)
	if err != nil {
		respond.FromError(w, r, err, "tenant")
		return
	}

	respond.JSON(w, r, http.StatusCreated, tenant)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	filter := ListFilter{
		Status: r.URL.Query().Get("status"),
	}

	tenants, err := h.svc.List(r.Context(), filter)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list tenants", "error", err)
		return
	}

	if tenants == nil {
		tenants = []domain.Tenant{}
	}

	respond.JSON(w, r, http.StatusOK, map[string]any{
		"data": tenants,
	})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	tenant, err := h.svc.Get(r.Context(), id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "tenant")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get tenant", "error", err)
		return
	}

	respond.JSON(w, r, http.StatusOK, tenant)
}
