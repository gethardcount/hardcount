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

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
)

// InviteTTL is how long an invitation can be accepted: long enough for a
// colleague on leave, short enough that a forgotten link is not a standing
// credential.
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

// SessionWriter starts and ends sessions on the caller's transaction: an
// accept starts a new account's session, and a removal ends the member's
// sessions in the workspace (a session row pins (user, tenant)).
// *session.Service satisfies it.
type SessionWriter interface {
	IssueTx(ctx context.Context, tx *sql.Tx, in session.IssueInput) (rawID string, sess session.Session, err error)
	EndForMemberTx(ctx context.Context, tx *sql.Tx, userID, tenantID string) error
}

// AuditTxWriter writes an audit row on the caller's transaction.
// *audit.Logger satisfies it.
type AuditTxWriter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// InviteEmailSender queues the invite email in email_outbox on the caller's
// transaction, so the email commits with the invitation. hasAccount is
// whether the invitee already has a sign-in account: the email then tells
// them to sign in, not to set a password.
type InviteEmailSender interface {
	SendMemberInviteTx(ctx context.Context, tx *sql.Tx, tenantID, to, inviterEmail, tenantName, acceptURL string, hasAccount bool) error
}

// TenantNamer resolves the workspace name for the accept-page header.
type TenantNamer interface {
	GetTenantName(ctx context.Context, tenantID string) (string, error)
}

type Service struct {
	store    *PostgresStore
	users    UserDirectory
	sessions SessionWriter
	audit    AuditTxWriter
	email    InviteEmailSender
	tenants  TenantNamer
	clock    clock.Clock
	// dashboardBaseURL builds the accept link. It comes from
	// DASHBOARD_BASE_URL, never from request headers (a forged Host would
	// send the token to another site). Empty makes Invite refuse.
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

// NewService returns a Service.
func NewService(store *PostgresStore, users UserDirectory, sessions SessionWriter, auditor AuditTxWriter, email InviteEmailSender, tenants TenantNamer, clk clock.Clock, dashboardBaseURL string) *Service {
	if clk == nil {
		clk = clock.Real()
	}
	return &Service{
		store: store, users: users, sessions: sessions, audit: auditor, email: email, tenants: tenants,
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

// Status is the invitation's state at now: revoked, accepted, expired from
// its expires_at on (the instant the accept starts to refuse), else pending.
func (inv Invitation) Status(now time.Time) string {
	switch {
	case inv.RevokedAt != nil:
		return "revoked"
	case inv.AcceptedAt != nil:
		return "accepted"
	case !now.Before(inv.ExpiresAt):
		return "expired"
	default:
		return "pending"
	}
}

// Invite creates a pending invitation and queues its accept email.
// inviterUserID must be a dashboard user who is still a member of the
// workspace (an API key has no user to name in the audit row). The role is
// recorded as 'member' and grants the full permission set until roles exist
// (ADR-081).
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
		return Invitation{}, errs.InvalidState(InvitesOffNoDashboardURL)
	}
	if !s.emailConfigured {
		return Invitation{}, errs.EmailNotConfigured()
	}

	existing, err := s.users.GetByEmail(ctx, email)
	if err != nil && !errors.Is(err, errs.ErrNotFound) {
		return Invitation{}, err
	}
	hasAccount := err == nil
	// A member is refused here: the invitations' unique index cannot see
	// memberships, which are in user_tenants.
	if hasAccount {
		tenants, err := s.users.TenantsForUser(ctx, existing.ID)
		if err != nil {
			return Invitation{}, fmt.Errorf("check memberships: %w", err)
		}
		for _, t := range tenants {
			if t.TenantID == tenantID {
				return Invitation{}, errs.AlreadyExists("email", "this person is already a member of the workspace")
			}
		}
	}

	rawToken, tokenHash, err := generateInviteToken()
	if err != nil {
		return Invitation{}, err
	}
	tenantName := s.tenantName(ctx, tenantID)
	acceptURL := s.dashboardBaseURL + "/accept-invite?token=" + rawToken
	now := s.clock.Now(ctx)
	// A pending invitation with no email is a dead end for the invitee and
	// blocks the retry, so a failed enqueue or audit row fails the invite and
	// writes nothing.
	return s.store.CreateInvitation(ctx, Invitation{
		TenantID:        tenantID,
		Email:           email,
		TokenHash:       tokenHash,
		InvitedByUserID: inviterUserID,
		Role:            "member",
		ExpiresAt:       now.Add(InviteTTL),
	}, now, func(tx *sql.Tx, inv Invitation, replacedID string) error {
		if err := s.email.SendMemberInviteTx(ctx, tx, tenantID, email, inv.InvitedByEmail, tenantName, acceptURL, hasAccount); err != nil {
			return fmt.Errorf("enqueue invite email: %w", err)
		}
		meta := map[string]any{"expires_at": inv.ExpiresAt.Format(rfc3339)}
		if replacedID != "" {
			meta["replaced_invitation_id"] = replacedID
		}
		// No email in the label: resource_id is the invitation id and the
		// reader resolves the address from member_invitations, which is
		// erasable. audit_log is not.
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:       "member.invited",
			ResourceType: "user",
			ResourceID:   inv.ID,
			Metadata:     meta,
		}); err != nil {
			return fmt.Errorf("write the audit row: %w", err)
		}
		return nil
	})
}

func (s *Service) tenantName(ctx context.Context, tenantID string) string {
	name, err := s.tenants.GetTenantName(ctx, tenantID)
	if err != nil {
		slog.WarnContext(ctx, "invite: tenant name lookup failed (email header falls back to generic)", "error", err)
		return ""
	}
	return name
}

// List returns the tenant's members and its invitations that are neither
// accepted nor revoked.
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

// Revoke revokes an invitation that is not yet accepted or revoked, and
// writes the member.invite_revoked audit row on the same transaction. If the
// row cannot be written, the invitation stays as it was.
func (s *Service) Revoke(ctx context.Context, tenantID, invitationID string) error {
	return s.store.RevokeInvitation(ctx, tenantID, invitationID, s.clock.Now(ctx), func(tx *sql.Tx) error {
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:       "member.invite_revoked",
			ResourceType: "user",
			ResourceID:   invitationID,
		}); err != nil {
			return fmt.Errorf("write the audit row: %w", err)
		}
		return nil
	})
}

// RemoveMember removes a user from the workspace. In one transaction it
// deletes the membership, revokes the invitations the user sent here that
// are not yet accepted, ends the user's sessions in this workspace (both
// modes), and writes the member.removed audit row. The row lists the revoked
// invitations' ids under revoked_invitation_ids, and has no such key when
// there are none. If any step fails, nothing changes. The user's sessions in
// other workspaces are not touched.
//
// Guards: no self-removal (someone else must remove you, so nobody locks
// themselves out by mistake), and never the last member (ErrLastMember: a
// workspace with no member cannot be signed in to again). The store checks
// the last-member rule in the transaction that deletes, so two concurrent
// removals cannot both pass it.
func (s *Service) RemoveMember(ctx context.Context, tenantID, actorUserID, targetUserID string) error {
	if targetUserID == "" {
		return errs.Required("user_id")
	}
	if actorUserID != "" && actorUserID == targetUserID {
		return errs.InvalidState("you cannot remove yourself — another member must remove you")
	}
	return s.store.RemoveMember(ctx, tenantID, targetUserID, s.clock.Now(ctx), func(tx *sql.Tx, revokedInvitationIDs []string) error {
		if err := s.sessions.EndForMemberTx(ctx, tx, targetUserID, tenantID); err != nil {
			return fmt.Errorf("end sessions: %w", err)
		}
		// The actor, the client address and the request id are already on
		// ctx (the session middleware and the root middlewares put them
		// there).
		var meta map[string]any
		if len(revokedInvitationIDs) > 0 {
			meta = map[string]any{"revoked_invitation_ids": revokedInvitationIDs}
		}
		if err := s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:       "member.removed",
			ResourceType: "user",
			ResourceID:   targetUserID,
			Metadata:     meta,
		}); err != nil {
			return fmt.Errorf("write the audit row: %w", err)
		}
		return nil
	})
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

// AcceptInput is an accept request: the invite token, the new account's
// password, and the browser's user agent and client address for the session
// and the audit row.
type AcceptInput struct {
	Token     string
	Password  string
	UserAgent string
	IP        string
}

// AcceptResult is a committed accept: who joined which workspace.
// RawSessionID, the cookie value shown this once, and Session are set only
// for a new account (see Handler.acceptInvite).
type AcceptResult struct {
	UserID       string
	Email        string
	TenantID     string
	NewAccount   bool
	RawSessionID string
	Session      session.Session
}

// AcceptInvite consumes the token and attaches the membership, creating the
// account first when none exists (password required and checked in that
// case; ignored otherwise). In one transaction it claims the invitation,
// writes the account or the membership, starts a new account's session, and
// writes the member.joined audit row. When any of them fails, nothing is
// written and the invitation stays usable.
func (s *Service) AcceptInvite(ctx context.Context, in AcceptInput) (AcceptResult, error) {
	inv, err := s.lookupPending(ctx, in.Token)
	if err != nil {
		return AcceptResult{}, err
	}
	// The route has no session; account events are filed in test mode, and
	// the new session starts in it.
	ctx = postgres.WithLivemode(ctx, false)

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
		passwordHash, err = s.users.HashPassword(in.Password)
		if err != nil {
			return AcceptResult{}, err
		}
	}

	res := AcceptResult{UserID: existing.ID, Email: inv.Email, TenantID: inv.TenantID, NewAccount: isNew}
	err = s.store.AcceptInvitation(ctx, inv.ID, s.clock.Now(ctx), func(tx *sql.Tx) error {
		if isNew {
			u, err := s.users.CreateInTx(ctx, tx, inv.Email, passwordHash, inv.TenantID, inv.Role)
			if err != nil {
				return err
			}
			res.UserID = u.ID
			res.RawSessionID, res.Session, err = s.sessions.IssueTx(ctx, tx, session.IssueInput{
				UserID:    u.ID,
				TenantID:  inv.TenantID,
				Livemode:  false,
				UserAgent: in.UserAgent,
				IP:        in.IP,
			})
			if err != nil {
				return fmt.Errorf("create the session: %w", err)
			}
		} else if err := s.users.AttachTenantInTx(ctx, tx, existing.ID, inv.TenantID, inv.Role); err != nil {
			return err
		}
		// The route has no session, so the accepting user is the actor and
		// the row carries the client address. resource_id names the user, not
		// the email: audit_log cannot be erased.
		actx := audit.WithClientIP(auth.WithUserID(ctx, res.UserID), in.IP)
		if err := s.audit.LogInTx(actx, tx, audit.Entry{
			Action:       "member.joined",
			ResourceType: "user",
			ResourceID:   res.UserID,
			Metadata:     map[string]any{"new_account": isNew},
		}); err != nil {
			return fmt.Errorf("write the audit row: %w", err)
		}
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

// errLinkInvalid is the one answer for a token that is unknown, expired,
// revoked or used, so the answer tells a guesser nothing.
var errLinkInvalid = errs.Invalid("token", "this invitation link is invalid, expired, revoked, or already used")

func (s *Service) lookupPending(ctx context.Context, token string) (Invitation, error) {
	if token == "" {
		return Invitation{}, errLinkInvalid
	}
	inv, err := s.store.GetInvitationByTokenHash(ctx, hashInviteToken(token))
	if errors.Is(err, errs.ErrNotFound) {
		return Invitation{}, errLinkInvalid
	}
	if err != nil {
		return Invitation{}, err
	}
	if inv.Status(s.clock.Now(ctx)) != "pending" {
		return Invitation{}, errLinkInvalid
	}
	return inv, nil
}
