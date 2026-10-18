package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/email"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// Settings → Email: a read-only view of this server's email setup and a
// "Send test email" action. Email settings live in the server environment
// (SMTP_*, the link base URLs), not in the database, so an operator could
// only learn that email was off from a per-invoice notice or the Team tab.
// This puts the whole picture on one page.

// emailLinkEnv is one environment variable an email link is built from.
type emailLinkEnv struct {
	Env string `json:"env"`
	Set bool   `json:"set"`
	// Effect says what breaks while it is unset, in the operator's words.
	Effect string `json:"effect"`
}

// emailSettingsStatus is GET /v1/settings/email.
type emailSettingsStatus struct {
	SMTPConfigured bool   `json:"smtp_configured"`
	SMTPHost       string `json:"smtp_host"`
	FromAddress    string `json:"from_address"`
	TLSMode        string `json:"tls_mode"`
	// Links lists every email-link env var, set or not, in a fixed order.
	Links []emailLinkEnv `json:"links"`
	// The same two flags whoami carries, so the page needs one request.
	CanEmailSetupLinks bool `json:"can_email_setup_links"`
	CanSendInvites     bool `json:"can_send_invites"`
}

type emailStatusSender interface {
	IsConfigured() bool
	SMTPHost() string
	FromAddress() string
	TLSMode() string
	SendTestEmail(ctx context.Context, tenantID, to string) error
}

type emailSettingsHandler struct {
	sender             emailStatusSender
	links              []emailLinkEnv
	canEmailSetupLinks bool
	canSendInvites     bool
	// userEmail resolves the signed-in member's email; the test email only
	// goes there, so this endpoint can't be used to mail arbitrary people.
	userEmail func(ctx context.Context, userID string) (string, error)
	audit     auth.AuditWriter
	timeout   time.Duration
}

// emailLinkEnvs reads the link env vars once at boot, the same values the
// boot WARNs report.
func emailLinkEnvs(getenv func(string) string) []emailLinkEnv {
	set := func(k string) bool { return strings.TrimSpace(getenv(k)) != "" }
	return []emailLinkEnv{
		{Env: "HOSTED_INVOICE_BASE_URL", Set: set("HOSTED_INVOICE_BASE_URL"),
			Effect: "Invoice, receipt and reminder emails go out without a link to view and pay the invoice."},
		{Env: "PAYMENT_UPDATE_URL", Set: set("PAYMENT_UPDATE_URL"),
			Effect: "Emails asking a customer to add a payment method can't be sent."},
		{Env: "DASHBOARD_BASE_URL", Set: set("DASHBOARD_BASE_URL"),
			Effect: "Password reset and team invite emails can't be sent."},
	}
}

func (h *emailSettingsHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.status)
	r.Post("/test", h.sendTest)
	return r
}

func (h *emailSettingsHandler) status(w http.ResponseWriter, r *http.Request) {
	respond.JSON(w, r, http.StatusOK, emailSettingsStatus{
		SMTPConfigured:     h.sender.IsConfigured(),
		SMTPHost:           h.sender.SMTPHost(),
		FromAddress:        h.sender.FromAddress(),
		TLSMode:            h.sender.TLSMode(),
		Links:              h.links,
		CanEmailSetupLinks: h.canEmailSetupLinks,
		CanSendInvites:     h.canSendInvites,
	})
}

// sendTest sends one plain email to the signed-in member, synchronously, and
// returns the SMTP server's real answer. 409 email_not_configured without
// SMTP_HOST; 502 smtp_error when the server refuses or can't be reached.
func (h *emailSettingsHandler) sendTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	userID := auth.UserID(ctx)
	if auth.GetKeyType(ctx) != auth.KeyTypeSession || userID == "" {
		// An API key has no address of its own to send to.
		respond.Forbidden(w, r, "Sign in to the dashboard to send a test email. It goes to your own address.")
		return
	}
	if !h.sender.IsConfigured() {
		respond.ConflictCoded(w, r, "", errs.CodeEmailNotConfigured,
			"This server can't send email: SMTP_HOST is not set. Set the SMTP_* variables and restart the server.")
		return
	}
	to, err := h.userEmail(ctx, userID)
	if err != nil || strings.TrimSpace(to) == "" {
		slog.Error("test email: resolve member email", "error", err)
		respond.InternalError(w, r)
		return
	}
	timeout := h.timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := h.sender.SendTestEmail(sendCtx, tenantID, to); err != nil {
		if errors.Is(err, email.ErrSMTPNotConfigured) {
			respond.ConflictCoded(w, r, "", errs.CodeEmailNotConfigured,
				"This server can't send email: SMTP_HOST is not set.")
			return
		}
		// The SMTP error is the operator's own server talking (refused
		// login, dial timeout, STARTTLS missing). It never carries the
		// password, and it is the one thing that tells them what to fix.
		respond.Error(w, r, http.StatusBadGateway, "api_error", "smtp_error",
			"The email server didn't accept the test email: "+err.Error())
		return
	}
	// The recipient is the actor's own address, already on the row as the
	// actor; it is not repeated in metadata. audit is non-nil: router.go
	// passes this handler to audit.MustWired.
	_ = h.audit.Log(ctx, tenantID, domain.AuditActionSend, "setting", "smtp", "SMTP test send",
		map[string]any{"action": "test_email_sent"})
	respond.JSON(w, r, http.StatusOK, map[string]string{"sent_to": to})
}
