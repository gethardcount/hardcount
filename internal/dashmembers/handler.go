package dashmembers

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/session"
)

// Handler wires the team-membership endpoints.
//
// Authed (mounted at /v1/members, behind the standard auth chain):
//
//	GET    /                  — members + invitations
//	POST   /invite            — create invitation + send email
//	DELETE /invitations/{id}  — revoke a pending invitation
//	DELETE /{userID}          — remove a member
//
// Public (registered on the /v1/auth block, no auth — token IS the
// credential, mirroring password-reset):
//
//	GET  /invite/{token}   — preview for the accept page
//	POST /accept-invite    — consume token; mint session for new accounts
type Handler struct {
	svc     *Service
	cookie  session.CookieConfig
	signIns SignInRecorder
}

// SignInRecorder is told when an invite accept signs a new member in. The
// sign-in throttle (internal/user) implements it: the accept proved the
// inbox, so it marks the browser as known for the email, and guesses from
// elsewhere no longer slow that browser. Wired in internal/api.
type SignInRecorder interface {
	InboxProved(w http.ResponseWriter, r *http.Request, email string) error
}

func NewHandler(svc *Service, cookie session.CookieConfig) *Handler {
	return &Handler{svc: svc, cookie: cookie}
}

// SetSignInRecorder wires the sign-in throttle. Optional: nil skips it, and
// the member's browser is then marked at their first sign-in instead.
func (h *Handler) SetSignInRecorder(s SignInRecorder) { h.signIns = s }

// Routes is the authed surface (mounted under /v1/members).
func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Post("/invite", h.invite)
	r.Delete("/invitations/{id}", h.revokeInvitation)
	r.Delete("/{userID}", h.removeMember)
	return r
}

// RegisterPublicRoutes attaches the unauthenticated accept-flow
// endpoints onto the /v1/auth block (they share its rate limiter —
// token guessing gets the same throttle as credential stuffing).
func (h *Handler) RegisterPublicRoutes(r chi.Router) {
	r.Get("/invite/{token}", h.previewInvite)
	r.Post("/accept-invite", h.acceptInvite)
}

// MemberView mirrors web-v2/src/lib/members.ts MemberView.
type MemberView struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

// InvitationView mirrors members.ts InvitationView. status is derived
// server-side; clients treat it as authoritative.
type InvitationView struct {
	ID             string  `json:"id"`
	Email          string  `json:"email"`
	Role           string  `json:"role"`
	Status         string  `json:"status"`
	InvitedByEmail string  `json:"invited_by_email,omitempty"`
	ExpiresAt      string  `json:"expires_at"`
	CreatedAt      string  `json:"created_at"`
	AcceptedAt     *string `json:"accepted_at,omitempty"`
	RevokedAt      *string `json:"revoked_at,omitempty"`
}

const rfc3339 = "2006-01-02T15:04:05Z07:00"

func fmtTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(rfc3339)
	return &s
}

func (h *Handler) invitationView(inv Invitation, now time.Time) InvitationView {
	return InvitationView{
		ID:             inv.ID,
		Email:          inv.Email,
		Role:           inv.Role,
		Status:         inv.Status(now),
		InvitedByEmail: inv.InvitedByEmail,
		ExpiresAt:      inv.ExpiresAt.Format(rfc3339),
		CreatedAt:      inv.CreatedAt.Format(rfc3339),
		AcceptedAt:     fmtTimePtr(inv.AcceptedAt),
		RevokedAt:      fmtTimePtr(inv.RevokedAt),
	}
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	members, invs, err := h.svc.List(ctx, tenantID)
	if err != nil {
		respond.FromError(w, r, err, "members")
		return
	}
	now := h.svc.clock.Now(ctx)
	mv := make([]MemberView, 0, len(members))
	for _, m := range members {
		mv = append(mv, MemberView{
			UserID: m.UserID, Email: m.Email, Role: m.Role,
			JoinedAt: m.JoinedAt.Format(rfc3339),
		})
	}
	iv := make([]InvitationView, 0, len(invs))
	for _, inv := range invs {
		iv = append(iv, h.invitationView(inv, now))
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{
		"members":     mv,
		"invitations": iv,
	})
}

type InviteReq struct {
	Email string `json:"email"`
}

func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	var req InviteReq
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	inv, err := h.svc.Invite(ctx, tenantID, auth.UserID(ctx), req.Email)
	if err != nil {
		respond.FromError(w, r, err, "invitation")
		return
	}
	respond.JSON(w, r, http.StatusCreated, h.invitationView(inv, h.svc.clock.Now(ctx)))
}

func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	id := chi.URLParam(r, "id")
	if err := h.svc.Revoke(ctx, tenantID, id); err != nil {
		respond.FromError(w, r, err, "invitation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	if !respond.NoBody(w, r) {
		return
	}
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	targetUserID := chi.URLParam(r, "userID")
	if err := h.svc.RemoveMember(ctx, tenantID, auth.UserID(ctx), targetUserID); err != nil {
		respond.FromError(w, r, err, "member")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// previewInvite renders the accept-page context. Unauthed; the token is
// the credential. Invalid/expired/revoked/consumed all collapse into one
// generic 422 — no state oracle for token guessers.
func (h *Handler) previewInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	preview, err := h.svc.PreviewInvite(ctx, chi.URLParam(r, "token"))
	if err != nil {
		respond.FromError(w, r, err, "invitation")
		return
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{
		"email":             preview.Email,
		"tenant_id":         preview.TenantID,
		"tenant_name":       preview.TenantName,
		"needs_new_account": preview.NeedsNewAccount,
		"invited_by_email":  preview.InvitedByEmail,
		"expires_at":        preview.ExpiresAt.Format(rfc3339),
	})
}

type AcceptInviteReq struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// acceptInvite consumes the token. A new account is signed in: the invitee
// just proved control of the invited inbox and set the password, the same
// trust as a completed password reset. An existing account is only attached
// and signs in with its own password: holding the email alone must not open
// an account that already exists.
func (h *Handler) acceptInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req AcceptInviteReq
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	res, err := h.svc.AcceptInvite(ctx, AcceptInput{
		Token:     req.Token,
		Password:  req.Password,
		UserAgent: r.UserAgent(),
		// The address TrustedClientIP resolved, never a forwarding header
		// the client wrote.
		IP: audit.ExtractClientIP(r),
	})
	if err != nil {
		respond.FromError(w, r, err, "invitation")
		return
	}

	out := map[string]any{
		"user_id":        res.UserID,
		"email":          res.Email,
		"tenant_id":      res.TenantID,
		"session_minted": false,
		"livemode":       false,
		"expires_at":     "",
	}
	if res.RawSessionID != "" {
		h.cookie.SetCookie(w, res.RawSessionID, res.Session.ExpiresAt)
		if h.signIns != nil {
			if err := h.signIns.InboxProved(w, r, res.Email); err != nil {
				slog.ErrorContext(ctx, "accept-invite: sign-in throttle did not mark the browser", "user_id", res.UserID, "error", err)
			}
		}
		out["session_minted"] = true
		out["livemode"] = res.Session.Livemode
		out["expires_at"] = res.Session.ExpiresAt.Format(rfc3339)
	}
	respond.JSON(w, r, http.StatusOK, out)
}
