package dashmembers

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/session"
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
	svc      *Service
	sessions *session.Service
	cookie   session.CookieConfig
	audit    AuditRecorder
}

// AuditRecorder is the narrow audit surface (same shape as the auth
// handler's) so dashmembers doesn't import *audit.Logger's full type.
type AuditRecorder interface {
	Log(ctx context.Context, tenantID, action, resourceType, resourceID, resourceLabel string, metadata map[string]any) error
}

func NewHandler(svc *Service, sessions *session.Service, cookie session.CookieConfig) *Handler {
	return &Handler{svc: svc, sessions: sessions, cookie: cookie}
}

// SetAuditLogger wires audit rows for membership changes (invite,
// revoke, remove, accept). Optional: nil skips audit writes.
func (h *Handler) SetAuditLogger(a AuditRecorder) { h.audit = a }

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

// memberView mirrors web-v2/src/lib/members.ts MemberView.
type memberView struct {
	UserID   string `json:"user_id"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

// invitationView mirrors members.ts InvitationView. status is derived
// server-side; clients treat it as authoritative.
type invitationView struct {
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

func (h *Handler) invitationView(inv Invitation, now time.Time) invitationView {
	return invitationView{
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
	mv := make([]memberView, 0, len(members))
	for _, m := range members {
		mv = append(mv, memberView{
			UserID: m.UserID, Email: m.Email, Role: m.Role,
			JoinedAt: m.JoinedAt.Format(rfc3339),
		})
	}
	iv := make([]invitationView, 0, len(invs))
	for _, inv := range invs {
		iv = append(iv, h.invitationView(inv, now))
	}
	respond.JSON(w, r, http.StatusOK, map[string]any{
		"members":     mv,
		"invitations": iv,
	})
}

type inviteReq struct {
	Email string `json:"email"`
}

func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	var req inviteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	inv, err := h.svc.Invite(ctx, tenantID, auth.UserID(ctx), req.Email)
	if err != nil {
		respond.FromError(w, r, err, "invitation")
		return
	}
	// No email in the label: resource_id is the invitation id and the reader resolves
	// the address from member_invitations, which is erasable. audit_log is not.
	h.auditEvent(ctx, tenantID, "member.invited", inv.ID, "", map[string]any{
		"expires_at": inv.ExpiresAt.Format(rfc3339),
	})
	respond.JSON(w, r, http.StatusCreated, h.invitationView(inv, h.svc.clock.Now(ctx)))
}

func (h *Handler) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	id := chi.URLParam(r, "id")
	if err := h.svc.Revoke(ctx, tenantID, id); err != nil {
		respond.FromError(w, r, err, "invitation")
		return
	}
	h.auditEvent(ctx, tenantID, "member.invite_revoked", id, "", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	targetUserID := chi.URLParam(r, "userID")
	if err := h.svc.RemoveMember(ctx, tenantID, auth.UserID(ctx), targetUserID); err != nil {
		respond.FromError(w, r, err, "member")
		return
	}
	h.auditEvent(ctx, tenantID, "member.removed", targetUserID, "", nil)
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

type acceptInviteReq struct {
	Token    string `json:"token"`
	Password string `json:"password"`
}

// acceptInvite consumes the token. New account → create + mint a session
// cookie (they just proved control of the invited inbox AND set their
// password — same trust as a completed password reset). Existing account
// → attach only; they sign in with their own password (email possession
// alone must not open an ALREADY-privileged account).
func (h *Handler) acceptInvite(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req acceptInviteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	res, err := h.svc.AcceptInvite(ctx, req.Token, req.Password)
	if err != nil {
		respond.FromError(w, r, err, "invitation")
		return
	}

	// Accept happens pre-auth, so stamp the accepting user as the actor
	// and livemode=false (the mode the fresh session starts in — without
	// it the audit writer's TxTenant refuses to open; same pattern as
	// the auth handler's audit).
	if h.audit != nil {
		actx := auth.WithUserID(ctx, res.UserID)
		actx = audit.WithClientIP(actx, audit.ExtractClientIP(r))
		actx = postgres.WithLivemode(actx, false)
		// resource_id IS the new member's user id; the reader resolves the address from
		// the (erasable) users row. Storing it would make it permanent.
		if err := h.audit.Log(actx, res.TenantID, "member.joined", "user", res.UserID, "",
			map[string]any{"new_account": res.NewAccount}); err != nil {
			slog.ErrorContext(ctx, "audit: member.joined write failed", "error", err)
		}
	}

	out := map[string]any{
		"user_id":        res.UserID,
		"email":          res.Email,
		"tenant_id":      res.TenantID,
		"session_minted": false,
		"livemode":       false,
		"expires_at":     "",
	}
	if res.MintSession && h.sessions != nil {
		rawID, sess, err := h.sessions.Issue(ctx, session.IssueInput{
			UserID:    res.UserID,
			TenantID:  res.TenantID,
			Livemode:  false, // dashboard sessions start in test mode, same as login
			UserAgent: r.UserAgent(),
			IP:        session.ClientIP(r),
		})
		if err != nil {
			// The membership is committed — the account works; only the
			// convenience auto-login failed. Return success and let the
			// login page take it from here.
			slog.ErrorContext(ctx, "accept-invite: session issue failed — user must log in manually",
				"user_id", res.UserID, "error", err)
		} else {
			h.cookie.SetCookie(w, r, rawID, sess.ExpiresAt)
			out["session_minted"] = true
			out["livemode"] = sess.Livemode
			out["expires_at"] = sess.ExpiresAt.Format(rfc3339)
		}
	}
	respond.JSON(w, r, http.StatusOK, out)
}

// auditEvent writes one membership audit row (actor comes from the
// session ctx the auth middleware populated). Best-effort.
func (h *Handler) auditEvent(ctx context.Context, tenantID, action, resourceID, label string, meta map[string]any) {
	if h.audit == nil {
		return
	}
	if err := h.audit.Log(ctx, tenantID, action, "user", resourceID, label, meta); err != nil {
		slog.ErrorContext(ctx, "audit: membership event write failed", "action", action, "error", err)
	}
}
