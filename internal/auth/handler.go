package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

// AuditWriter is the narrow audit surface the auth handler uses.
// Declared here so the package stays decoupled from internal/audit
// and can be tested with a fake. Production wires *audit.Logger via
// SetAuditLogger in router.go.
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

// SetAuditLogger wires the audit logger so API key lifecycle changes
// (create / revoke / rotate) write audit_log rows. Without this,
// security-critical key mutations are invisible to the operator's
// Audit Log page — a SOC2 / forensics blocker.
func (h *Handler) SetAuditLogger(a AuditWriter) {
	h.auditLogger = a
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/", h.list)
	r.Delete("/{id}", h.revoke)
	r.Post("/{id}/rotate", h.rotate)
	return r
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "not authenticated")
		return
	}

	var input CreateKeyInput
	if !respond.DecodeStrict(w, r, &input) {
		return
	}

	result, err := h.svc.CreateKey(r.Context(), tenantID, input)
	if err != nil {
		respond.FromError(w, r, err, "api_key")
		return
	}

	// Never log the raw key (security-critical). The audit row records
	// only the key id, name, and permission set so an auditor can trace
	// "who issued this credential" without the row becoming a credential
	// harvesting target.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionCreate, "api_key", result.Key.ID, result.Key.Name, map[string]any{
			"key_type": result.Key.KeyType,
			"livemode": result.Key.Livemode,
		})
	}

	respond.JSON(w, r, http.StatusCreated, result)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "not authenticated")
		return
	}

	page, ok := respond.ParsePage(w, r)
	if !ok {
		return
	}
	keys, total, err := h.svc.ListKeys(r.Context(), ListFilter{TenantID: tenantID, Limit: page.Limit, Offset: page.Offset})
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list api keys", "error", err)
		return
	}
	respond.List(w, r, keys, total)
}

func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := TenantID(r.Context())
	id := chi.URLParam(r, "id")

	// Pre-ADR-011 guarded against revoking the calling Bearer key.
	// With user-bound dashboard sessions, revoking via the dashboard
	// doesn't kill the cookie session (sessions don't reference API
	// keys), so the foot-gun is gone. Bearer callers that revoke
	// their own key get an immediate 401 on the next request — that's
	// the operator's intent.

	key, err := h.svc.RevokeKey(r.Context(), tenantID, id)
	if errors.Is(err, errs.ErrNotFound) {
		respond.NotFound(w, r, "api key")
		return
	}
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "revoke api key", "error", err)
		return
	}

	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionRevoke, "api_key", key.ID, key.Name, map[string]any{
			"key_type": key.KeyType,
		})
	}

	respond.JSON(w, r, http.StatusOK, key)
}

func (h *Handler) rotate(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantID(r.Context())
	id := chi.URLParam(r, "id")

	// Pre-ADR-011 guarded against self-rotation; with user-bound
	// dashboard sessions, rotating via the dashboard doesn't drop
	// the operator's auth (cookie isn't tied to the rotated key).
	// Bearer callers that rotate their own key need to swap to the
	// new raw_key in their config — that's the rotation contract.
	//
	// Body is optional — POST /rotate with no body defaults to immediate
	// revocation of the old key. An empty request should not 400.
	var input RotateKeyInput
	if !respond.DecodeStrict(w, r, &input, respond.EmptyBodyOK) {
		return
	}

	result, err := h.svc.RotateKey(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "api_key")
		return
	}

	// Same hygiene as create — no raw key value in the audit row.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionRotate, "api_key", result.NewKey.ID, result.NewKey.Name, map[string]any{
			"key_type":   result.NewKey.KeyType,
			"old_key_id": id,
		})
	}

	respond.JSON(w, r, http.StatusOK, result)
}
