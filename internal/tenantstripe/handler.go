package tenantstripe

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

// AuditWriter is the narrow audit surface tenantstripe uses.
type AuditWriter interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

// Handler serves the tenant-facing Stripe connection endpoints. Mount under
// /v1/settings/stripe with auth middleware that populates the tenant ctx.
// This endpoint is deliberately platform-scoped: a tenant uses their single
// set of platform API keys to administer both modes via the same surface.
type Handler struct {
	svc         *Service
	auditLogger AuditWriter
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// SetAuditLogger wires audit on Stripe connection management. Each
// mutation (connect / delete / set-webhook) is a security-relevant
// integration change — auditors look for these.
func (h *Handler) SetAuditLogger(a AuditWriter) {
	h.auditLogger = a
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/", h.connect)
	r.Get("/", h.list)
	r.Delete("/{mode}", h.delete)
	r.Patch("/{mode}/webhook", h.setWebhook)
	return r
}

func (h *Handler) connect(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}

	var in ConnectInput
	if !respond.DecodeStrict(w, r, &in) {
		return
	}

	creds, err := h.svc.Connect(r.Context(), tenantID, in)
	if err != nil {
		respond.FromError(w, r, err, "stripe_credentials")
		return
	}
	// Never log the secret key value itself — just record that a
	// connection was made for (tenant, mode). Auditor can correlate
	// with the timestamp + actor.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionCreate, "stripe_credentials", creds.ID, "", map[string]any{
			"action":   "connected",
			"livemode": creds.Livemode,
		})
	}
	respond.JSON(w, r, http.StatusOK, creds)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}

	creds, err := h.svc.List(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list stripe credentials", "error", err)
		return
	}
	if creds == nil {
		creds = []domain.StripeProviderCredentials{}
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{"data": creds})
}

// SetWebhookInput is the PATCH /v1/settings/stripe/{mode}/webhook body.
type SetWebhookInput struct {
	WebhookSecret string `json:"webhook_secret"`
}

func (h *Handler) setWebhook(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}

	mode := chi.URLParam(r, "mode")
	livemode, ok := parseMode(mode)
	if !ok {
		respond.BadRequest(w, r, `mode must be "test" or "live"`)
		return
	}

	var in SetWebhookInput
	if !respond.DecodeStrict(w, r, &in) {
		return
	}

	creds, err := h.svc.SetWebhookSecret(r.Context(), tenantID, livemode, in.WebhookSecret)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "stripe_credentials")
			return
		}
		respond.FromError(w, r, err, "stripe_credentials")
		return
	}
	// Webhook-secret rotation is an integration-security event;
	// record the rotation without the secret value.
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionRotate, "stripe_credentials", creds.ID, "", map[string]any{
			"action":   "webhook_secret_set",
			"livemode": livemode,
		})
	}
	respond.JSON(w, r, http.StatusOK, creds)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		respond.Unauthorized(w, r, "missing tenant context")
		return
	}

	mode := chi.URLParam(r, "mode")
	livemode, ok := parseMode(mode)
	if !ok {
		respond.BadRequest(w, r, `mode must be "test" or "live"`)
		return
	}

	if err := h.svc.Delete(r.Context(), tenantID, livemode); err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			respond.NotFound(w, r, "stripe_credentials")
			return
		}
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "delete stripe credentials", "error", err)
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.Log(r.Context(), tenantID, domain.AuditActionDelete, "stripe_credentials", "", "", map[string]any{
			"livemode": livemode,
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseMode(mode string) (bool, bool) {
	switch mode {
	case "live":
		return true, true
	case "test":
		return false, true
	default:
		return false, false
	}
}
