package auth

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
)

// Handler serves the API key routes. The Service writes each change's audit
// row.
type Handler struct {
	svc *Service
}

// NewHandler returns a Handler for the keys that svc manages.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Routes returns the key routes: POST / creates a key, GET / lists keys,
// DELETE /{id} revokes one and POST /{id}/rotate rotates one. They are
// mounted behind session.MiddlewareOrAPIKey, which sets the tenant. A caller
// may revoke or rotate the key it calls with; a dashboard session is not
// tied to a key (ADR-011).
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
	// The body is optional: with none, the old key is revoked at once.
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
