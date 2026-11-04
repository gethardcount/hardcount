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

// AuditWriter is the narrow audit surface the auth handler uses. It is
// declared here so the package does not depend on internal/audit and can be
// tested with a fake. Production wires *audit.Logger with SetAuditLogger in
// internal/api/router.go.
type AuditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// Handler serves the API key routes.
type Handler struct {
	svc         *Service
	auditLogger AuditWriter
}

// NewHandler returns a Handler for the keys that svc manages.
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// SetAuditLogger wires the audit logger, so creating, revoking and rotating
// an API key writes an audit_log row. Without it, these security-critical
// key changes do not appear on the operator's Audit Log page, which blocks
// a SOC 2 audit or a forensic investigation.
func (h *Handler) SetAuditLogger(a AuditWriter) {
	h.auditLogger = a
}

// Routes returns the key routes: POST / creates a key, GET / lists keys,
// DELETE /{id} revokes one and POST /{id}/rotate rotates one.
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

	// Never log the raw key: it is a credential. The audit row records only
	// the key id, name and permission set. An auditor can then trace who
	// issued the credential, and the row is no target for credential theft.
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

	// A caller may revoke the key it calls with. Dashboard sessions are
	// bound to a user and do not reference API keys (ADR-011), so revoking
	// a key from the dashboard does not end the cookie session. A Bearer
	// caller that revokes its own key gets an immediate 401 on its next
	// request, which is what the operator meant.

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

	// A caller may rotate the key it calls with. Rotating from the
	// dashboard does not sign the operator out, because the cookie is not
	// tied to the rotated key (ADR-011). A Bearer caller that rotates its
	// own key must switch to the new raw_key in its config. That is the
	// rotation contract.
	//
	// The body is optional. POST /rotate with no body revokes the old key
	// at once, and an empty request should not get a 400.
	var input RotateKeyInput
	if !respond.DecodeStrict(w, r, &input, respond.EmptyBodyOK) {
		return
	}

	result, err := h.svc.RotateKey(r.Context(), tenantID, id, input)
	if err != nil {
		respond.FromError(w, r, err, "api_key")
		return
	}

	// As in create, the audit row holds no raw key value.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionRotate, "api_key", result.NewKey.ID, result.NewKey.Name, map[string]any{
			"key_type":   result.NewKey.KeyType,
			"old_key_id": id,
		})
	}

	respond.JSON(w, r, http.StatusOK, result)
}
