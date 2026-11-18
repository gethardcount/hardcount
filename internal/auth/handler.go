package auth

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// Handler serves the API key routes.
type Handler struct {
	svc *Service
}

// NewHandler returns a Handler for the keys that svc manages.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Routes returns the key routes. The router mounts them behind
// session.RequireCaller, which puts the caller's tenant and mode in ctx, and
// RequirePermission, whose route table asks apikey:write of every one, which
// refuses an ingest key.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Delete("/{id}", h.revoke)
	r.Post("/{id}/rotate", h.rotate)
	return r
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var input CreateKeyInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}
	result, err := h.svc.CreateKey(r.Context(), TenantID(r.Context()), input)
	if err != nil {
		respond.FromError(w, r, err, "api key")
		return
	}

	respond.JSON(w, r, http.StatusCreated, result)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}
	keys, total, err := h.svc.ListKeys(r.Context(), ListFilter{TenantID: TenantID(r.Context()), Limit: page.Limit, Offset: page.Offset})
	if err != nil {
		respond.FromError(w, r, err, "api key")
		return
	}
	respond.List(w, r, keys, total)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	key, err := h.svc.RevokeKey(r.Context(), TenantID(r.Context()), chi.URLParam(r, "id"))
	if err != nil {
		respond.FromError(w, r, err, "api key")
		return
	}
	respond.JSON(w, r, http.StatusOK, key)
}

func (h *Handler) rotate(w http.ResponseWriter, r *http.Request) {
	var input RotateKeyInput
	if !respond.DecodeStrict(w, r, &input, respond.EmptyBodyOK) {
		return
	}
	result, err := h.svc.RotateKey(r.Context(), TenantID(r.Context()), chi.URLParam(r, "id"), input)
	if err != nil {
		respond.FromError(w, r, err, "api key")
		return
	}

	respond.JSON(w, r, http.StatusOK, result)
}
