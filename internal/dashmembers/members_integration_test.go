package dashmembers_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/dashmembers"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/session"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/user"
)

// --- test doubles ---------------------------------------------------------

// capturingEmail records the invite email instead of sending it; the
// accept-URL token is how tests get hold of the plaintext token (it is
// never returned by the API, mirroring production).
type capturingEmail struct {
	sent []capturedInvite
	fail bool
}

type capturedInvite struct {
	tenantID, to, inviterEmail, tenantName, acceptURL string
}

func (c *capturingEmail) SendMemberInvite(_ context.Context, tenantID, to, inviterEmail, tenantName, acceptURL string) error {
	if c.fail {
		return errors.New("injected enqueue failure")
	}
	c.sent = append(c.sent, capturedInvite{tenantID, to, inviterEmail, tenantName, acceptURL})
	return nil
}

// lastToken pulls the raw token out of the most recent accept URL.
func (c *capturingEmail) lastToken(t *testing.T) string {
	t.Helper()
	if len(c.sent) == 0 {
		t.Fatal("no invite email was captured")
	}
	url := c.sent[len(c.sent)-1].acceptURL
	_, token, found := strings.Cut(url, "token=")
	if !found {
		t.Fatalf("accept URL %q has no token param", url)
	}
	return token
}

type staticTenantNamer struct{ name string }

func (s staticTenantNamer) GetTenantName(context.Context, string) (string, error) {
	return s.name, nil
}

// userDirAdapter mirrors the production memberUserDirectoryAdapter
// (internal/api) — service for CreateUser, store for lookups.
type userDirAdapter struct {
	svc   *user.Service
	store *user.PostgresStore
}

func (a userDirAdapter) GetByEmail(ctx context.Context, email string) (domain.User, error) {
	return a.store.GetByEmail(ctx, email)
}
func (a userDirAdapter) TenantsForUser(ctx context.Context, userID string) ([]domain.UserTenant, error) {
	return a.store.TenantsForUser(ctx, userID)
}
func (a userDirAdapter) CreateUser(ctx context.Context, email, plaintext, tenantID, role string) (domain.User, error) {
	return a.svc.CreateUser(ctx, email, plaintext, tenantID, role)
}
func (a userDirAdapter) AttachTenant(ctx context.Context, userID, tenantID, role string) error {
	return a.store.AttachTenant(ctx, userID, tenantID, role)
}

// --- harness --------------------------------------------------------------

type harness struct {
	db       *postgres.DB
	tenantID string
	svc      *dashmembers.Service
	email    *capturingEmail
	clk      *clock.Fake
	users    *user.Service
	sessions *session.Service
	owner    string // bootstrap owner user id
}

const testPassword = "correct-horse-battery-staple"

func newHarness(t *testing.T, tenantName string) *harness {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, tenantName)

	userStore := user.NewPostgresStore(db)
	userSvc := user.NewService(userStore, nil)
	sessionSvc := session.NewService(session.NewPostgresStore(db))
	emails := &capturingEmail{}
	clk := clock.NewFake(time.Now().UTC())

	svc := dashmembers.NewService(
		dashmembers.NewPostgresStore(db),
		userDirAdapter{svc: userSvc, store: userStore},
		sessionSvc,
		emails,
		staticTenantNamer{name: tenantName},
		clk,
		"http://dash.test",
	)
	svc.SetEmailConfigured(true)

	// Every workspace starts with a bootstrap owner (mirrors production:
	// tenants are created via the bootstrap flow which mints the first user).
	owner, err := userSvc.CreateUser(context.Background(),
		fmt.Sprintf("owner-%s@%s.test", tenantID[len(tenantID)-6:], strings.ToLower(strings.ReplaceAll(tenantName, " ", "-"))),
		testPassword, tenantID, "owner")
	if err != nil {
		t.Fatalf("create bootstrap owner: %v", err)
	}

	return &harness{
		db: db, tenantID: tenantID, svc: svc, email: emails, clk: clk,
		users: userSvc, sessions: sessionSvc, owner: owner.ID,
	}
}

// --- tests ----------------------------------------------------------------

// TestInviteAcceptNewUser is the golden path: invite → email captured →
// preview says new account → accept with password → member listed, token
// burned (single-use), second accept and stale preview both rejected.
func TestInviteAcceptNewUser(t *testing.T) {
	h := newHarness(t, "Team Golden")
	ctx := context.Background()

	inv, err := h.svc.Invite(ctx, h.tenantID, h.owner, "Newbie@Example.COM")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if inv.Email != "newbie@example.com" {
		t.Errorf("invite email not normalized: got %q", inv.Email)
	}
	if inv.Status(h.clk.Now(ctx)) != "pending" {
		t.Errorf("fresh invite status: got %q, want pending", inv.Status(h.clk.Now(ctx)))
	}
	if inv.InvitedByEmail == "" {
		t.Error("InvitedByEmail must be hydrated from the inviter join")
	}
	token := h.email.lastToken(t)

	preview, err := h.svc.PreviewInvite(ctx, token)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if !preview.NeedsNewAccount {
		t.Error("preview.NeedsNewAccount must be true for an unknown email")
	}
	if preview.TenantName != "Team Golden" {
		t.Errorf("preview tenant name: got %q", preview.TenantName)
	}

	res, err := h.svc.AcceptInvite(ctx, token, testPassword)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !res.MintSession || !res.NewAccount {
		t.Errorf("new-account accept must mint a session: got %+v", res)
	}
	if res.TenantID != h.tenantID {
		t.Errorf("accept tenant: got %q, want %q", res.TenantID, h.tenantID)
	}

	members, invs, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("member count after accept: got %d, want 2", len(members))
	}
	foundNew := false
	for _, m := range members {
		if m.Email == "newbie@example.com" {
			foundNew = true
			if m.Role != "member" {
				t.Errorf("joined role: got %q, want member", m.Role)
			}
		}
	}
	if !foundNew {
		t.Error("accepted invitee must appear in the member list")
	}
	if len(invs) != 1 || invs[0].Status(h.clk.Now(ctx)) != "accepted" {
		t.Errorf("invitation must read accepted after use: %+v", invs)
	}

	// The new account can authenticate with the password it set.
	if _, _, err := h.users.Authenticate(ctx, "newbie@example.com", testPassword); err != nil {
		t.Errorf("new member must be able to log in: %v", err)
	}

	// Single-use: replaying the same link is rejected with the generic error.
	if _, err := h.svc.AcceptInvite(ctx, token, testPassword); err == nil {
		t.Fatal("second accept of the same token must fail")
	}
	if _, err := h.svc.PreviewInvite(ctx, token); err == nil {
		t.Fatal("preview of a consumed token must fail")
	}
}

// TestInviteAcceptExistingUser: accepting with an email that already has
// an account attaches the membership WITHOUT minting a session — email
// possession alone must not log into an already-privileged account.
func TestInviteAcceptExistingUser(t *testing.T) {
	h := newHarness(t, "Team Existing")
	ctx := context.Background()

	// The invitee already has an account on ANOTHER workspace.
	otherTenant := testutil.CreateTestTenant(t, h.db, "Other Workspace")
	existing, err := h.users.CreateUser(ctx, "veteran@example.com", testPassword, otherTenant, "owner")
	if err != nil {
		t.Fatalf("create existing user: %v", err)
	}

	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "veteran@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	preview, err := h.svc.PreviewInvite(ctx, token)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.NeedsNewAccount {
		t.Error("preview must recognize the existing account")
	}

	// Password field is ignored on the existing-account path.
	res, err := h.svc.AcceptInvite(ctx, token, "")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.MintSession || res.NewAccount {
		t.Errorf("existing-account accept must NOT mint a session: %+v", res)
	}
	if res.UserID != existing.ID {
		t.Errorf("accept user: got %q, want existing %q", res.UserID, existing.ID)
	}

	tenants, err := user.NewPostgresStore(h.db).TenantsForUser(ctx, existing.ID)
	if err != nil {
		t.Fatalf("tenants for user: %v", err)
	}
	if len(tenants) != 2 {
		t.Fatalf("existing user must now belong to 2 tenants, got %d", len(tenants))
	}
}

// TestInviteGates: inviter identity required (API-key callers have none),
// duplicate pending blocked, already-a-member blocked, bad email blocked.
func TestInviteGates(t *testing.T) {
	h := newHarness(t, "Team Gates")
	ctx := context.Background()

	// No user identity (Bearer API key path) → rejected.
	if _, err := h.svc.Invite(ctx, h.tenantID, "", "a@example.com"); err == nil {
		t.Fatal("invite without a session user must be rejected")
	}

	// Malformed email.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "not-an-email"); err == nil {
		t.Fatal("malformed email must be rejected")
	}

	// Duplicate pending invite → typed conflict.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com"); err != nil {
		t.Fatalf("first invite: %v", err)
	}
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com"); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("duplicate pending invite: got %v, want ErrAlreadyExists", err)
	}

	// Revoking the pending invite frees the slot for a re-send.
	_, invs, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := h.svc.Revoke(ctx, h.tenantID, invs[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com"); err != nil {
		t.Fatalf("re-invite after revoke must succeed: %v", err)
	}

	// Already a member → conflict (the invitee holds an account here).
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "member2@example.com"); err != nil {
		t.Fatalf("invite member2: %v", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, h.email.lastToken(t), testPassword); err != nil {
		t.Fatalf("accept member2: %v", err)
	}
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "member2@example.com"); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("inviting an existing member: got %v, want ErrAlreadyExists", err)
	}
}

// TestRevokedAndExpiredTokens: a revoked invite's link stops working; an
// expired invite's link stops working; both collapse into the same
// generic validation error (no state oracle).
func TestRevokedAndExpiredTokens(t *testing.T) {
	h := newHarness(t, "Team Tokens")
	ctx := context.Background()

	// Revoked.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "revoked@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	revokedToken := h.email.lastToken(t)
	_, invs, _ := h.svc.List(ctx, h.tenantID)
	if err := h.svc.Revoke(ctx, h.tenantID, invs[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, revokedToken, testPassword); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("accept revoked: got %v, want ErrValidation", err)
	}

	// Expired: advance the fake clock past the 7-day TTL.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "late@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	lateToken := h.email.lastToken(t)
	h.clk.Advance(dashmembers.InviteTTL + time.Hour)
	if _, err := h.svc.PreviewInvite(ctx, lateToken); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("preview expired: got %v, want ErrValidation", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, lateToken, testPassword); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("accept expired: got %v, want ErrValidation", err)
	}

	// Garbage token → same generic error, not a distinguishable one.
	if _, err := h.svc.AcceptInvite(ctx, "deadbeef", testPassword); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("accept garbage: got %v, want ErrValidation", err)
	}
}

// TestRemoveMember: self-removal and last-member removal are blocked; a
// real removal deletes the membership AND revokes the target's sessions
// (sessions pin the tenant — without revocation an active session keeps
// full access after the membership row is gone).
func TestRemoveMember(t *testing.T) {
	h := newHarness(t, "Team Remove")
	ctx := context.Background()

	// Last member: the bootstrap owner can't be removed.
	if err := h.svc.RemoveMember(ctx, h.tenantID, h.owner, h.owner); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("self+last removal: got %v, want ErrInvalidState", err)
	}

	// Bring in a second member.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "second@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	res, err := h.svc.AcceptInvite(ctx, h.email.lastToken(t), testPassword)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Self-removal still blocked even with 2 members.
	if err := h.svc.RemoveMember(ctx, h.tenantID, res.UserID, res.UserID); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("self removal: got %v, want ErrInvalidState", err)
	}

	// Removing a non-member → not found.
	if err := h.svc.RemoveMember(ctx, h.tenantID, h.owner, "vlx_user_nonexistent"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("remove non-member: got %v, want ErrNotFound", err)
	}

	// Give the target a live session, then remove them: the membership
	// goes away and the session is revoked.
	rawID, _, err := h.sessions.Issue(ctx, session.IssueInput{
		UserID: res.UserID, TenantID: h.tenantID, Livemode: false,
	})
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}
	if err := h.svc.RemoveMember(ctx, h.tenantID, h.owner, res.UserID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	members, _, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 1 {
		t.Fatalf("member count after removal: got %d, want 1", len(members))
	}
	if _, err := h.sessions.Resolve(ctx, rawID); err == nil {
		t.Fatal("removed member's session must be revoked — an active session would keep full tenant access")
	}
}

// TestInviteEmailEnqueueFailure: if the outbox enqueue fails, the invite
// row must not survive as a pending dead end (it would block re-invites
// via the pending-unique index while the invitee never got a link).
func TestInviteEmailEnqueueFailure(t *testing.T) {
	h := newHarness(t, "Team EmailFail")
	ctx := context.Background()

	h.email.fail = true
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "ghost@example.com"); err == nil {
		t.Fatal("invite must fail loudly when the email enqueue fails")
	}

	// The slot is free: a retry with a working sender succeeds.
	h.email.fail = false
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "ghost@example.com"); err != nil {
		t.Fatalf("retry after enqueue failure must succeed: %v", err)
	}
}

// TestInviteRefusedWhenEmailNotConfigured: the accept link exists only in
// the email, so on a server with no SMTP an invite is a dead end. Invite
// refuses with email_not_configured before any invitation row exists — a
// later invite of the same address, once email works, is not blocked.
func TestInviteRefusedWhenEmailNotConfigured(t *testing.T) {
	h := newHarness(t, "Team NoSMTP")
	ctx := context.Background()

	h.svc.SetEmailConfigured(false)
	_, err := h.svc.Invite(ctx, h.tenantID, h.owner, "ghost@example.com")
	if errs.Code(err) != errs.CodeEmailNotConfigured || !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("err = %v (code %q), want 409-class email_not_configured", err, errs.Code(err))
	}
	if len(h.email.sent) != 0 {
		t.Fatal("an invite email was queued on a server that cannot send it")
	}
	_, invs, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(invs) != 0 {
		t.Fatalf("refused invite left %d invitation rows", len(invs))
	}

	h.svc.SetEmailConfigured(true)
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "ghost@example.com"); err != nil {
		t.Fatalf("invite once email works: %v", err)
	}
}

// TestConcurrentAccept: two racing accepts of the same token — exactly
// one wins the CAS; the loser gets the generic invalid-token error and
// no duplicate membership/account is created.
func TestConcurrentAccept(t *testing.T) {
	h := newHarness(t, "Team Race")
	ctx := context.Background()

	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "race@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	type outcome struct {
		res dashmembers.AcceptResult
		err error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			res, err := h.svc.AcceptInvite(ctx, token, testPassword)
			results <- outcome{res, err}
		}()
	}
	var wins, losses int
	for range 2 {
		o := <-results
		if o.err == nil {
			wins++
		} else {
			losses++
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("concurrent accept: got %d wins / %d losses, want exactly 1/1", wins, losses)
	}
	members, _, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("member count after race: got %d, want 2 (no duplicate join)", len(members))
	}
}
