package recipe

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
)

// Handler serves the recipes HTTP routes. It is thin: it decodes path and
// body parameters, calls Service, and translates errors with
// respond.FromError. All business logic lives in Service, so the handlers
// stay safe to refactor.
type Handler struct {
	svc *Service
}

// NewHandler returns a Handler that serves svc.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// Routes returns the chi sub-router rooted at /v1/recipes (the parent
// router mounts it). Endpoints:
//
//	GET    /                           — list recipes + per-tenant install state
//	GET    /{key}                      — get one recipe (registry only)
//	POST   /{key}/preview              — render with overrides, no DB
//	POST   /{key}/instantiate          — idempotent apply under one tx
//	GET    /instances                  — list installed recipes for tenant
//
// There is no uninstall. A recipe is an instantiation event, and the
// objects it creates belong to the operator: plans carry live
// subscriptions, and the catalog is shared reference data. So there is
// nothing an uninstall could safely take back (ADR-085). To retire the
// generated plan, archive it. The badge stays as a true record of what was
// applied, and at which version.
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/instances", h.listInstances)
	r.Get("/{key}", h.get)
	r.Post("/{key}/preview", h.preview)
	r.Post("/{key}/instantiate", h.instantiate)
	return r
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}
	// The catalog is built into the binary, read whole and paged here.
	items, err := h.svc.ListRecipes(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list recipes", "error", err)
		return
	}
	respond.List(w, r, respond.Window(items, page), len(items))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	rec, err := h.svc.GetRecipe(key)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "recipe")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "get recipe", "error", err, "key", key)
		return
	}
	respond.JSON(w, r, http.StatusOK, rec)
}

// PreviewRequest is the POST /preview body. Overrides is open-ended JSON,
// and the recipe's overridable schema limits its values. The service
// validates it against the recipe before rendering.
type PreviewRequest struct {
	Overrides map[string]any `json:"overrides"`
}

func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	// Read-only: this renders the recipe with overrides and writes nothing.
	// MarkSkip declares that this 2xx changed nothing, so the audit-coverage
	// detector does not report a preview as an uncovered mutation. The
	// route is also exempt(non_mutating_preview) in the registry, so the
	// detector already skips it. The declaration is still required: it is
	// the local declaration that the registry note cites, and the two agree.
	// If the exemption is ever lifted, this call keeps the route from being
	// reported as an uncovered mutation.
	audit.MarkSkip(r.Context())

	key := chi.URLParam(r, "key")
	var req PreviewRequest
	if !respond.DecodeStrict(w, r, &req, respond.EmptyBodyOK) {
		return
	}
	rec, err := h.svc.Preview(r.Context(), key, req.Overrides)
	if err != nil {
		respond.FromError(w, r, err, "recipe")
		return
	}
	respond.JSON(w, r, http.StatusOK, rec)
}

// InstantiateRequest is the POST /instantiate body. Apply is idempotent
// (ADR-085). Posting an installed recipe again returns the existing
// instance, so there is no force or overwrite option.
type InstantiateRequest struct {
	Overrides map[string]any `json:"overrides"`
}

func (h *Handler) instantiate(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	key := chi.URLParam(r, "key")
	var req InstantiateRequest
	if !respond.DecodeStrict(w, r, &req, respond.EmptyBodyOK) {
		return
	}
	inst, err := h.svc.Instantiate(r.Context(), tenantID, key, req.Overrides, InstantiateOptions{
		CreatedBy: auth.KeyID(r.Context()),
	})
	if err != nil {
		respond.FromError(w, r, err, "recipe")
		return
	}
	// An install writes its audit row inside the service's coordinator
	// transaction (ADR-090), and that row accounts for the request. An
	// idempotent re-apply installs nothing and emits nothing, because a
	// no-op deserves no record. Service.Instantiate declares that itself,
	// with audit.MarkSkip on the branch where the badge exists.
	respond.JSON(w, r, http.StatusCreated, inst)
}

func (h *Handler) listInstances(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}
	// At most one instance per catalog recipe, read whole and paged here.
	instances, err := h.svc.ListInstances(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list recipe instances", "error", err)
		return
	}
	respond.List(w, r, respond.Window(instances, page), len(instances))
}
