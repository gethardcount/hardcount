package dashmembers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// InviteTTL: 7 days — long enough for a colleague on PTO, short enough
// that a stale link in an inbox isn't a standing credential. (Password
// reset is 1h because it targets an EXISTING account; an invite mints a
// new one and the pending-unique index caps exposure to one live link
// per address.)
const InviteTTL = 7 * 24 * time.Hour

// UserDirectory is the narrow slice of the user package the invite flow
// needs. The router's adapter satisfies it with user.HashPassword and
// *user.PostgresStore. dashmembers never imports the user package, keeping
// the auth dependency one-directional.
type UserDirectory interface {
	// GetByEmail returns errs.ErrNotFound when no account exists.
	GetByEmail(ctx context.Context, email string) (domain.User, error)
	// TenantsForUser lists the user's memberships.
	TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error)
	// HashPassword checks a new account's password against the password
	// rules and returns its bcrypt hash. The accept flow calls it before it
	// claims the invitation, so a refused password leaves the invite usable.
	HashPassword(plaintext string) (string, error)
	// CreateInTx creates the account and its membership with the role, on
	// the caller's transaction. The bootstrap owner is created the same way.
	CreateInTx(ctx context.Context, tx *sql.Tx, email, passwordHash, tenantID, role string) (domain.User, error)
	// AttachTenantInTx adds a membership for an existing user, on the
	// caller's transaction.
	AttachTenantInTx(ctx context.Context, tx *sql.Tx, userID, tenantID, role string) error
}

// SessionRevoker kills every session of a removed member — session rows
// pin (user, tenant), so without this an active session keeps operating
// after the membership row is gone.
type SessionRevoker interface {
	RevokeAllForUser(ctx context.Context, userID string) error
}

// InviteEmailSender queues the invite email in email_outbox on the caller's
// transaction, so the email commits with the invitation.
type InviteEmailSender interface {
	SendMemberInviteTx(ctx context.Context, tx *sql.Tx, tenantID, to, inviterEmail, tenantName, acceptURL string) error
}

// TenantNamer resolves the workspace name for the accept-page header.
type TenantNamer interface {
	GetTenantName(ctx context.Context, tenantID string) (string, error)
}

type Service struct {
	store    *PostgresStore
	users    UserDirectory
	sessions SessionRevoker
	email    InviteEmailSender
	tenants  TenantNamer
	clock    clock.Clock
	// dashboardBaseURL builds accept links; mirrors the password-reset
	// posture — never derived from request headers (host-header
	// poisoning steals tokens). Empty = invites are created but no email
	// goes out (loud in the response is wrong — loud at CREATE: see Invite).
	dashboardBaseURL string
	// emailConfigured is whether this server can send email (SMTP_HOST
	// set). The accept link exists only in the email, so an invite on a
	// server with no SMTP is a dead end; Invite refuses it up front. Zero
	// value refuses.
	emailConfigured bool
}

// SetEmailConfigured records whether this server can send email
// (email.Sender.IsConfigured at boot).
func (s *Service) SetEmailConfigured(configured bool) { s.emailConfigured = configured }

// InvitesOffNoDashboardURL is the refusal when DASHBOARD_BASE_URL is unset.
// It names no setting: the dashboard shows it to whoever clicked Send invite,
// who usually can't change server settings. The setting's name is in
// docs/ops/email-setup.md and in the Team tab's Details.
const InvitesOffNoDashboardURL = "Invites are off: this server has no dashboard address set, so it can't build the invite link. Ask whoever runs Hardcount to set it."

// CanInvite reports whether Invite can succeed on this server's settings:
// an accept link can be built (DASHBOARD_BASE_URL) and emailed (SMTP). It is
// the same two checks Invite makes first, so the Team tab can say invites are
// off before anyone types an address (served on /v1/whoami).
func (s *Service) CanInvite() bool {
	return s.dashboardBaseURL != "" && s.emailConfigured
}

func NewService(store *PostgresStore, users UserDirectory, sessions SessionRevoker, email InviteEmailSender, tenants TenantNamer, clk clock.Clock, dashboardBaseURL string) *Service {
	if clk == nil {
		clk = clock.Real()
	}
	return &Service{
		store: store, users: users, sessions: sessions, email: email, tenants: tenants,
		clock: clk, dashboardBaseURL: strings.TrimRight(strings.TrimSpace(dashboardBaseURL), "/"),
	}
}

// generateInviteToken mirrors the password-reset token discipline: 32
// random bytes, hex on the wire, only sha256(token) persisted so a DB
// snapshot can't be replayed as an accept link.
func generateInviteToken() (plaintext, hash string, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate invite token: %w", err)
	}
	plaintext = hex.EncodeToString(buf)
	return plaintext, hashInviteToken(plaintext), nil
}

func hashInviteToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// InvitationStatus derives the lifecycle state the client treats as
// authoritative.
func (inv Invitation) Status(now time.Time) string {
	switch {
	case inv.RevokedAt != nil:
		return "revoked"
	case inv.AcceptedAt != nil:
		return "accepted"
	case now.After(inv.ExpiresAt):
		return "expired"
	default:
		return "pending"
	}
}

// Invite creates a pending invitation and enqueues the accept email.
// inviterUserID must be a real dashboard user (API-key callers have no
// user identity — inviting is a human act and the audit trail needs a
// person). Role is recorded as 'member' but grants the full permission
// set in v1 (no RBAC yet — documented at the session middleware seam).
func (s *Service) Invite(ctx context.Context, tenantID, inviterUserID, email string) (Invitation, error) {
	if inviterUserID == "" {
		return Invitation{}, errs.InvalidState("inviting requires a dashboard session — API keys have no user identity to attribute the invite to")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return Invitation{}, errs.Required("email")
	}
	// The address becomes the member's sign-in email and the invite's To
	// address, so it gets the same check as a customer's email.
	email, err := domain.ValidateEmail("email", email)
	if err != nil {
		return Invitation{}, err
	}
	if s.dashboardBaseURL == "" {
		// Same fail-loud posture as password reset: without a canonical
		// dashboard origin there is no safe way to build the accept link
		// (deriving it from request headers invites host-header token
		// theft). Refuse rather than mint an invite nobody can accept.
		return Invitation{}, errs.InvalidState(InvitesOffNoDashboardURL)
	}

	// The accept link is delivered only by email; without email there is
	// nothing the invitee can act on. Refuse before the invitation row exists.
	if !s.emailConfigured {
		return Invitation{}, errs.EmailNotConfigured()
	}

	// Already a member? The unique index can't see this (memberships live
	// in user_tenants), so check explicitly for a clean 409.
	if existing, err := s.users.GetByEmail(ctx, email); err == nil {
		tenants, terr := s.users.TenantsForUser(ctx, existing.ID)
		if terr != nil {
			return Invitation{}, fmt.Errorf("check memberships: %w", terr)
		}
		for _, t := range tenants {
			if t.TenantID == tenantID {
				return Invitation{}, errs.AlreadyExists("email", "this person is already a member of the workspace")
			}
		}
	} else if !errors.Is(err, errs.ErrNotFound) {
		return Invitation{}, err
	}

	rawToken, tokenHash, err := generateInviteToken()
	if err != nil {
		return Invitation{}, err
	}
	tenantName := s.tenantName(ctx, tenantID)
	acceptURL := s.dashboardBaseURL + "/accept-invite?token=" + rawToken
	// The invitation and its email commit together. A pending invitation
	// with no email is a dead end for the invitee, and it blocks the
	// operator's retry through the pending-unique index. So a failed enqueue
	// fails the invite and writes nothing.
	return s.store.CreateInvitation(ctx, Invitation{
		TenantID:        tenantID,
		Email:           email,
		TokenHash:       tokenHash,
		InvitedByUserID: inviterUserID,
		Role:            "member",
		ExpiresAt:       s.clock.Now(ctx).Add(InviteTTL),
	}, func(tx *sql.Tx, inv Invitation) error {
		if s.email == nil {
			return nil
		}
		if err := s.email.SendMemberInviteTx(ctx, tx, tenantID, email, inv.InvitedByEmail, tenantName, acceptURL); err != nil {
			return fmt.Errorf("enqueue invite email: %w", err)
		}
		return nil
	})
}

func (s *Service) tenantName(ctx context.Context, tenantID string) string {
	if s.tenants == nil {
		return ""
	}
	name, err := s.tenants.GetTenantName(ctx, tenantID)
	if err != nil {
		slog.WarnContext(ctx, "invite: tenant name lookup failed (email header falls back to generic)", "error", err)
		return ""
	}
	return name
}

// List returns members + all invitations for the tenant.
func (s *Service) List(ctx context.Context, tenantID string) ([]Member, []Invitation, error) {
	members, err := s.store.ListMembers(ctx, tenantID)
	if err != nil {
		return nil, nil, err
	}
	invs, err := s.store.ListInvitations(ctx, tenantID)
	if err != nil {
		return nil, nil, err
	}
	return members, invs, nil
}

// Revoke cancels a pending invitation.
func (s *Service) Revoke(ctx context.Context, tenantID, invitationID string) error {
	return s.store.RevokeInvitation(ctx, tenantID, invitationID, s.clock.Now(ctx))
}

// RemoveMember detaches a user from the tenant and revokes their
// sessions (session rows pin the tenant, so an active session would
// otherwise keep full access after the membership row is gone).
// Guards: no self-removal (lockout footgun — someone else must remove
// you) and never the last member (ErrLastMember: an ownerless workspace is
// unrecoverable without psql). The store checks the last-member rule in the
// transaction that deletes, so two concurrent removals cannot both pass it.
func (s *Service) RemoveMember(ctx context.Context, tenantID, actorUserID, targetUserID string) error {
	if targetUserID == "" {
		return errs.Required("user_id")
	}
	if actorUserID != "" && actorUserID == targetUserID {
		return errs.InvalidState("you cannot remove yourself — another member must remove you")
	}
	if err := s.store.RemoveMember(ctx, tenantID, targetUserID); err != nil {
		return err
	}
	if s.sessions != nil {
		if err := s.sessions.RevokeAllForUser(ctx, targetUserID); err != nil {
			// Membership is gone (login re-derives memberships), but an
			// EXISTING session pins the tenant and would keep working
			// until expiry — loud, because this is the actual lockout.
			slog.ErrorContext(ctx, "member removed but session revocation failed — their active sessions keep access until expiry",
				"user_id", targetUserID, "error", err)
		}
	}
	return nil
}

// InvitePreview drives the accept page: whose workspace, which email,
// and whether the invitee needs to create an account.
type InvitePreview struct {
	Email           string
	TenantID        string
	TenantName      string
	NeedsNewAccount bool
	InvitedByEmail  string
	ExpiresAt       time.Time
}

// PreviewInvite validates a token for the accept page. Invalid/expired/
// consumed tokens all return the same generic error — no oracle.
func (s *Service) PreviewInvite(ctx context.Context, token string) (InvitePreview, error) {
	inv, err := s.lookupPending(ctx, token)
	if err != nil {
		return InvitePreview{}, err
	}
	needsNew := false
	if _, err := s.users.GetByEmail(ctx, inv.Email); errors.Is(err, errs.ErrNotFound) {
		needsNew = true
	} else if err != nil {
		return InvitePreview{}, err
	}
	return InvitePreview{
		Email:           inv.Email,
		TenantID:        inv.TenantID,
		TenantName:      s.tenantName(ctx, inv.TenantID),
		NeedsNewAccount: needsNew,
		InvitedByEmail:  inv.InvitedByEmail,
		ExpiresAt:       inv.ExpiresAt,
	}, nil
}

// AcceptResult reports who joined where, and whether a session should be
// minted (new accounts only — they just set their password, the same
// trust level as a completed password reset. An EXISTING account is NOT
// logged in by email possession alone: the invitee signs in with their
// own password afterwards).
type AcceptResult struct {
	UserID      string
	Email       string
	TenantID    string
	MintSession bool
	NewAccount  bool
}

// AcceptInvite consumes the token (single-use CAS) and attaches the
// membership — creating the account first when none exists (password
// required and validated in that case; ignored otherwise). The claim, the
// account and the membership commit in one transaction: when any of them
// fails, nothing is written and the invitation stays usable.
func (s *Service) AcceptInvite(ctx context.Context, token, password string) (AcceptResult, error) {
	inv, err := s.lookupPending(ctx, token)
	if err != nil {
		return AcceptResult{}, err
	}

	existing, lookupErr := s.users.GetByEmail(ctx, inv.Email)
	isNew := errors.Is(lookupErr, errs.ErrNotFound)
	if lookupErr != nil && !isNew {
		return AcceptResult{}, lookupErr
	}
	// A new account's password is checked and hashed before the claim. The
	// bcrypt work then stays outside the transaction, and a refused password
	// (too short, too long, breached) leaves the invitation pending.
	var passwordHash string
	if isNew {
		passwordHash, err = s.users.HashPassword(password)
		if err != nil {
			return AcceptResult{}, err
		}
	}

	// The CAS is the exactly-once gate: a concurrent second accept, or a
	// revoke racing this one, loses there and join does not run.
	res := AcceptResult{UserID: existing.ID, Email: inv.Email, TenantID: inv.TenantID, MintSession: isNew, NewAccount: isNew}
	err = s.store.AcceptInvitation(ctx, inv.ID, s.clock.Now(ctx), func(tx *sql.Tx) error {
		if !isNew {
			return s.users.AttachTenantInTx(ctx, tx, existing.ID, inv.TenantID, inv.Role)
		}
		u, err := s.users.CreateInTx(ctx, tx, inv.Email, passwordHash, inv.TenantID, inv.Role)
		if err != nil {
			return err
		}
		res.UserID = u.ID
		return nil
	})
	if errors.Is(err, errs.ErrNotFound) {
		return AcceptResult{}, errs.Invalid("token", "this invitation is no longer valid — ask for a new invite")
	}
	if err != nil {
		return AcceptResult{}, err
	}
	return res, nil
}

func (s *Service) lookupPending(ctx context.Context, token string) (Invitation, error) {
	if token == "" {
		return Invitation{}, errs.Invalid("token", "this invitation link is invalid, expired, revoked, or already used")
	}
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInviteToken(token))
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return Invitation{}, errs.Invalid("token", "this invitation link is invalid, expired, revoked, or already used")
		}
		return Invitation{}, err
	}
	if inv.Status(s.clock.Now(ctx)) != "pending" {
		return Invitation{}, errs.Invalid("token", "this invitation link is invalid, expired, revoked, or already used")
	}
	return inv, nil
}
