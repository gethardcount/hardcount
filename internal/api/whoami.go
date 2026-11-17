package api

import (
	"net/http"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// handleWhoami answers GET /v1/whoami. It needs no permission. A key gets
// key_id and key_type; a session gets user_id and email, which the dashboard
// needs to stay signed in.
func (s *Server) handleWhoami(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	live, err := postgres.Livemode(ctx)
	if err != nil {
		respond.FromError(w, req, err, "whoami")
		return
	}
	out := map[string]any{
		"tenant_id": auth.TenantID(ctx),
		"livemode":  live,
		// can_email_setup_links lets the invoice page offer "Email a payment-method link".
		"can_email_setup_links": s.setupLinksEmailable,
		"can_send_invites":      s.membersSvc.CanInvite(),
		// smtp_configured lets any page show one global "email is
		// off" notice instead of repeating it on every record.
		"smtp_configured": s.EmailSender.IsConfigured(),
	}
	if uid := auth.UserID(ctx); uid != "" {
		u, err := s.userSvc.GetByID(ctx, uid)
		if err != nil {
			respond.FromError(w, req, err, "whoami")
			return
		}
		out["user_id"] = uid
		out["email"] = u.Email
	}
	if kid := auth.KeyID(ctx); kid != "" {
		out["key_id"] = kid
		out["key_type"] = string(auth.CallerOf(ctx).KeyType)
	}
	respond.JSON(w, req, http.StatusOK, out)
}
