package dashmembers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/email"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request/requesttest"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/user"
)

// --- test doubles ---------------------------------------------------------

// capturingEmail queues the invite email through the real OutboxSender, on
// the invitation's transaction, and records it. The accept-URL token is how
// tests get hold of the plaintext token (it is never returned by the API,
// mirroring production). With fail set, it queues the email and then fails,
// so a test sees whether the email row rolls back with the invitation.
type capturingEmail struct {
	outbox *email.OutboxSender
	sent   []capturedInvite
	fail   bool
}

type capturedInvite struct {
	tenantID, to, inviterEmail, tenantName, acceptURL string
	hasAccount                                        bool
}

func (c *capturingEmail) SendMemberInviteTx(ctx context.Context, tx *sql.Tx, tenantID, to, inviterEmail, tenantName, acceptURL string, hasAccount bool) error {
	if err := c.outbox.SendMemberInviteTx(ctx, tx, tenantID, to, inviterEmail, tenantName, acceptURL, hasAccount); err != nil {
		return err
	}
	if c.fail {
		return errors.New("injected enqueue failure")
	}
	c.sent = append(c.sent, capturedInvite{tenantID, to, inviterEmail, tenantName, acceptURL, hasAccount})
	return nil
}

// countInviteEmails counts the tenant's member_invite rows in email_outbox.
func countInviteEmails(t *testing.T, db *postgres.DB, tenantID string) int {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM email_outbox WHERE tenant_id = $1 AND email_type = $2`,
		tenantID, email.TypeMemberInvite).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
// (internal/api): the store for lookups and writes, user.HashPassword for
// the password rules.
type userDirAdapter struct{ *user.PostgresStore }

func (userDirAdapter) HashPassword(plaintext string) (string, error) {
	return user.HashPassword(plaintext)
}

// backdateAccount moves the account's created_at back by age.
func backdateAccount(t *testing.T, db *postgres.DB, userID string, age time.Duration) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(t.Context(), `UPDATE users SET created_at = created_at - make_interval(secs => $2) WHERE id = $1`,
		userID, age.Seconds()); err != nil {
		t.Fatalf("backdate account %s: %v", userID, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// createUser adds an account with testPassword that belongs to tenantID
// with role, the way bootstrap does: user.HashPassword, then CreateInTx on
// one transaction.
func createUser(t *testing.T, db *postgres.DB, address, tenantID, role string) domain.User {
	t.Helper()
	hash, err := user.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("hash the password for %s: %v", address, err)
	}
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx for %s: %v", address, err)
	}
	defer postgres.Rollback(tx)
	u, err := user.NewPostgresStore(db).CreateInTx(t.Context(), tx, address, hash, tenantID, role)
	if err != nil {
		t.Fatalf("create user %s: %v", address, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit user %s: %v", address, err)
	}
	return u
}

// accountInNoWorkspace adds an account with testPassword that is a member of
// no workspace: it joins a workspace of its own and that workspace's owner
// removes it. Only such an existing account can accept an invitation.
func accountInNoWorkspace(t *testing.T, h *harness, address string) domain.User {
	t.Helper()
	home := testutil.CreateTestTenant(t, h.db, "Former Home")
	owner := createUser(t, h.db, "owner-of-"+address, home, "owner")
	u := createUser(t, h.db, address, home, "member")
	if err := h.svc.RemoveMember(postgres.WithLivemode(t.Context(), false), home, owner.ID, u.ID); err != nil {
		t.Fatalf("remove %s from its first workspace: %v", address, err)
	}
	return u
}

// servePublic sends one request to the accept routes, which take no session,
// and returns the answer.
func servePublic(h *harness, method, path, body string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Use(requesttest.Root)
	dashmembers.NewHandler(h.svc, session.CookieConfig{Path: "/"}).RegisterPublicRoutes(r)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// errorBody reads the code and the message from an answer's error envelope.
func errorBody(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the answer %s: %v", rec.Body.String(), err)
	}
	return body.Error.Code, body.Error.Message
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
	audit    *audit.Logger
	owner    string // bootstrap owner user id
}

const testPassword = "correct-horse-battery-staple"

func newHarness(t *testing.T, tenantName string) *harness {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, tenantName)

	userStore := user.NewPostgresStore(db)
	sessionSvc := session.NewService(session.NewPostgresStore(db))
	auditLogger := audit.NewLogger(db)
	userSvc := user.NewService(userStore, nil, sessionSvc, auditLogger)
	emails := &capturingEmail{outbox: email.NewOutboxSender(email.NewOutboxStore(db))}
	clk := clock.NewFake(time.Now().UTC())

	svc := dashmembers.NewService(
		dashmembers.NewPostgresStore(db),
		userDirAdapter{userStore},
		sessionSvc,
		auditLogger,
		emails,
		staticTenantNamer{name: tenantName},
		clk,
		"http://dash.test",
	)
	svc.SetEmailConfigured(true)

	// Every workspace starts with a bootstrap owner (mirrors production:
	// tenants are created via the bootstrap flow which mints the first user).
	owner := createUser(t, db,
		fmt.Sprintf("owner-%s@%s.test", tenantID[len(tenantID)-6:], strings.ToLower(strings.ReplaceAll(tenantName, " ", "-"))),
		tenantID, "owner")

	return &harness{
		db: db, tenantID: tenantID, svc: svc, email: emails, clk: clk,
		users: userSvc, sessions: sessionSvc, audit: auditLogger, owner: owner.ID,
	}
}

// --- tests ----------------------------------------------------------------

// TestInviteAcceptNewUser is the golden path: invite → email captured →
// preview says new account → accept with password → member listed and the
// invitation gone from the list, token burned (single-use), second accept
// and stale preview both rejected.
func TestInviteAcceptNewUser(t *testing.T) {
	h := newHarness(t, "Team Golden")
	ctx := postgres.WithLivemode(context.Background(), false)

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
	if h.email.sent[0].hasAccount {
		t.Error("the invite email of a person with no account says they have one")
	}

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

	res, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: testPassword})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !res.NewAccount || res.RawSessionID == "" {
		t.Errorf("new-account accept must start a session: got %+v", res)
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
	if len(invs) != 0 {
		t.Errorf("invitations listed after the accept = %+v, want none: the invitee is a member now", invs)
	}

	// The new account can authenticate with the password it set.
	if _, err := h.users.Login(ctx, user.LoginInput{Email: "newbie@example.com", Password: testPassword}); err != nil {
		t.Errorf("new member must be able to log in: %v", err)
	}

	// Single-use: replaying the same link is rejected with the generic error.
	if _, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: testPassword}); err == nil {
		t.Fatal("second accept of the same token must fail")
	}
	if _, err := h.svc.PreviewInvite(ctx, token); err == nil {
		t.Fatal("preview of a consumed token must fail")
	}
}

// TestInviteAcceptExistingUser: accepting an invite for an email that already
// has an account, in no workspace, attaches the membership WITHOUT minting a
// session — email possession alone must not log into an already-privileged
// account. The invite email is worded for someone with an account, and the
// members list dates them from the day they joined this workspace.
func TestInviteAcceptExistingUser(t *testing.T) {
	h := newHarness(t, "Team Existing")
	ctx := postgres.WithLivemode(context.Background(), false)

	existing := accountInNoWorkspace(t, h, "veteran@example.com")
	// The account is a month older than this workspace's owner. Production
	// writes created_at as now(), so only SQL can age it.
	backdateAccount(t, h.db, existing.ID, 30*24*time.Hour)

	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "veteran@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)
	if !h.email.sent[0].hasAccount {
		t.Error("the invite email of a person with an account says they have none")
	}

	preview, err := h.svc.PreviewInvite(ctx, token)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.NeedsNewAccount {
		t.Error("preview must recognize the existing account")
	}

	// Password field is ignored on the existing-account path.
	res, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: ""})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if res.NewAccount || res.RawSessionID != "" {
		t.Errorf("existing-account accept must NOT start a session: %+v", res)
	}
	if res.UserID != existing.ID {
		t.Errorf("accept user: got %q, want existing %q", res.UserID, existing.ID)
	}

	// The list is in joining order, and the older account joined last.
	members, _, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("member count after accept: got %d, want 2", len(members))
	}
	if members[0].UserID != h.owner || members[1].UserID != existing.ID {
		t.Errorf("members = %+v, want the owner, then the invitee who joined after", members)
	}
	for _, m := range members {
		if age := time.Since(m.JoinedAt); m.UserID == existing.ID && age > 24*time.Hour {
			t.Errorf("the invitee joined %v ago, want today: that is the age of their account, not of this membership", age.Round(time.Hour))
		}
	}
}

// TestInviteAcceptRefusedForAMemberElsewhere: an address belongs to one
// workspace. An invitation to a member of another workspace is created like
// any other. Its accept page and its accept answer 409 member_elsewhere with
// the address in the message, and write nothing. The same link joins the
// account once it has left the other workspace.
func TestInviteAcceptRefusedForAMemberElsewhere(t *testing.T) {
	h := newHarness(t, "Team Refusal")
	ctx := postgres.WithLivemode(context.Background(), false)
	const invitee = "veteran@example.com"
	otherTenant := testutil.CreateTestTenant(t, h.db, "Other Workspace")
	otherOwner := createUser(t, h.db, "owner@other.example.com", otherTenant, "owner")
	existing := createUser(t, h.db, invitee, otherTenant, "member")

	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, invitee); err != nil {
		t.Fatalf("invite an address that is a member of another workspace: %v, want it created", err)
	}
	token := h.email.lastToken(t)
	preview := func() *httptest.ResponseRecorder {
		return servePublic(h, http.MethodGet, "/invite/"+token, "")
	}
	accept := func() *httptest.ResponseRecorder {
		return servePublic(h, http.MethodPost, "/accept-invite", `{"token":"`+token+`"}`)
	}

	for name, rec := range map[string]*httptest.ResponseRecorder{"preview": preview(), "accept": accept()} {
		code, message := errorBody(t, rec)
		if rec.Code != http.StatusConflict || code != "member_elsewhere" || !strings.HasPrefix(message, invitee+" already belongs to another workspace") {
			t.Errorf("%s for a member of another workspace answered %d %q %q, want 409 member_elsewhere and a message that starts with the address", name, rec.Code, code, message)
		}
	}
	if isMember(t, h, existing.ID, h.tenantID) {
		t.Fatal("the refused accept wrote a membership")
	}

	if err := h.svc.RemoveMember(ctx, otherTenant, otherOwner.ID, existing.ID); err != nil {
		t.Fatalf("remove the invitee from the other workspace: %v", err)
	}
	if rec := preview(); rec.Code != http.StatusOK {
		t.Fatalf("preview once the account has left the other workspace answered %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if rec := accept(); rec.Code != http.StatusOK || !isMember(t, h, existing.ID, h.tenantID) {
		t.Fatalf("accept once the account has left the other workspace answered %d, want 200 and the membership; body %s", rec.Code, rec.Body.String())
	}
}

// TestInviteGates: inviter identity required (API-key callers have none),
// an inviter who is not a member refused, a revoke by a non-member refused,
// a revoked invitation frees its address, already-a-member blocked, bad
// email blocked.
func TestInviteGates(t *testing.T) {
	h := newHarness(t, "Team Gates")
	ctx := postgres.WithLivemode(context.Background(), false)

	// No user identity (Bearer API key path) → rejected.
	if _, err := h.svc.Invite(ctx, h.tenantID, "", "a@example.com"); err == nil {
		t.Fatal("invite without a session user must be rejected")
	}

	// Malformed email.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "not-an-email"); err == nil {
		t.Fatal("malformed email must be rejected")
	}

	// An account of another workspace cannot invite here, and nothing is
	// written.
	outsiderHome := testutil.CreateTestTenant(t, h.db, "Outsider Home")
	outsider := createUser(t, h.db, "outsider@example.com", outsiderHome, "owner")
	if _, err := h.svc.Invite(ctx, h.tenantID, outsider.ID, "a@example.com"); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("invite by a non-member: got %v, want ErrForbidden", err)
	}
	if _, invs, err := h.svc.List(ctx, h.tenantID); err != nil || len(invs) != 0 {
		t.Fatalf("invitations after the refused invite: got %d (err %v), want 0", len(invs), err)
	}

	// Revoking the pending invite frees the slot for a re-send.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com"); err != nil {
		t.Fatalf("first invite: %v", err)
	}
	_, invs, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	// The outsider cannot revoke it: from their own workspace it is not found,
	// and in this one they are not a member.
	if err := h.svc.Revoke(ctx, outsiderHome, outsider.ID, invs[0].ID); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("revoke from another workspace: got %v, want ErrNotFound", err)
	}
	if err := h.svc.Revoke(ctx, h.tenantID, outsider.ID, invs[0].ID); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("revoke by a non-member: got %v, want ErrForbidden", err)
	}
	if err := h.svc.Revoke(ctx, h.tenantID, h.owner, invs[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "dup@example.com"); err != nil {
		t.Fatalf("re-invite after revoke must succeed: %v", err)
	}

	// Already a member → conflict (the invitee holds an account here).
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "member2@example.com"); err != nil {
		t.Fatalf("invite member2: %v", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: h.email.lastToken(t), Password: testPassword}); err != nil {
		t.Fatalf("accept member2: %v", err)
	}
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "member2@example.com"); !errors.Is(err, errs.ErrAlreadyExists) {
		t.Fatalf("inviting an existing member: got %v, want ErrAlreadyExists", err)
	}
}

// TestRevokedAndExpiredTokens: a revoked invite's link stops working; an
// expired invite's link stops working; both collapse into the same
// generic validation error (no state oracle). The list keeps the expired
// invitation and drops the revoked one.
func TestRevokedAndExpiredTokens(t *testing.T) {
	h := newHarness(t, "Team Tokens")
	ctx := postgres.WithLivemode(context.Background(), false)

	// Revoked.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "revoked@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	revokedToken := h.email.lastToken(t)
	_, invs, _ := h.svc.List(ctx, h.tenantID)
	if err := h.svc.Revoke(ctx, h.tenantID, h.owner, invs[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: revokedToken, Password: testPassword}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("accept revoked: got %v, want ErrValidation", err)
	}

	// Expired: the clock stands on the expiry instant itself, where the accept
	// already refuses.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "late@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	lateToken := h.email.lastToken(t)
	h.clk.Advance(dashmembers.InviteTTL)
	if _, err := h.svc.PreviewInvite(ctx, lateToken); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("preview expired: got %v, want ErrValidation", err)
	}
	if _, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: lateToken, Password: testPassword}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("accept expired: got %v, want ErrValidation", err)
	}

	// Garbage token → same generic error, not a distinguishable one.
	if _, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: "deadbeef", Password: testPassword}); !errors.Is(err, errs.ErrValidation) {
		t.Fatalf("accept garbage: got %v, want ErrValidation", err)
	}

	// The list holds the invitations someone can still act on: the expired
	// one and a pending one, not the revoked one.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "waiting@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	_, invs, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed := map[string]string{}
	for _, inv := range invs {
		listed[inv.Email] = inv.Status(h.clk.Now(ctx))
	}
	if len(listed) != 2 || listed["late@example.com"] != "expired" || listed["waiting@example.com"] != "pending" {
		t.Errorf("invitations listed = %v, want late@example.com expired and waiting@example.com pending", listed)
	}
}

// TestRemoveMember: self-removal is blocked, removing someone who is not a
// member is not found, a real removal deletes the membership, and a removal
// by someone who is no longer a member is refused and removes nobody.
func TestRemoveMember(t *testing.T) {
	h := newHarness(t, "Team Remove")
	ctx := postgres.WithLivemode(context.Background(), false)

	// The only member cannot remove themselves.
	if err := h.svc.RemoveMember(ctx, h.tenantID, h.owner, h.owner); !errors.Is(err, errs.ErrInvalidState) {
		t.Fatalf("self removal of the only member: got %v, want ErrInvalidState", err)
	}

	// Bring in a second member.
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "second@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	res, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: h.email.lastToken(t), Password: testPassword})
	if err != nil {
		t.Fatalf("accept: %v", err)
	}

	// Removing a non-member → not found.
	if err := h.svc.RemoveMember(ctx, h.tenantID, h.owner, "hc_user_nonexistent"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("remove non-member: got %v, want ErrNotFound", err)
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

	// The removed member is no longer a member, so their removal of someone
	// else is refused, and so is their removal of themselves, as a non-member.
	third := createUser(t, h.db, "third@example.com", h.tenantID, "member")
	if err := h.svc.RemoveMember(ctx, h.tenantID, res.UserID, res.UserID); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("self removal by a former member: got %v, want ErrForbidden", err)
	}
	if err := h.svc.RemoveMember(ctx, h.tenantID, res.UserID, third.ID); !errors.Is(err, errs.ErrForbidden) {
		t.Fatalf("removal by a former member: got %v, want ErrForbidden", err)
	}
	if !isMember(t, h, third.ID, h.tenantID) {
		t.Error("the refused removal deleted the membership")
	}
}

// TestInviteEmailEnqueueFailure: the invitation and its email commit
// together or not at all. When the enqueue fails, no invitation row is left
// (a pending one would block re-invites via the pending-unique index while
// the invitee never got a link) and no email row either.
//
// Mutation: queue the email after CreateInvitation commits, and a failed
// enqueue leaves an invitation row.
func TestInviteEmailEnqueueFailure(t *testing.T) {
	h := newHarness(t, "Team EmailFail")
	ctx := postgres.WithLivemode(context.Background(), false)

	h.email.fail = true
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "ghost@example.com"); err == nil {
		t.Fatal("invite must fail loudly when the email enqueue fails")
	}
	_, invs, err := h.svc.List(ctx, h.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(invs) != 0 {
		t.Fatalf("a failed email enqueue left %d invitation rows, want none: the invitation and its email commit together or not at all", len(invs))
	}
	if n := countInviteEmails(t, h.db, h.tenantID); n != 0 {
		t.Fatalf("a failed invite left %d invite email rows, want none", n)
	}

	// The slot is free: a retry with a working sender succeeds, and its
	// email row commits with it.
	h.email.fail = false
	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "ghost@example.com"); err != nil {
		t.Fatalf("retry after enqueue failure must succeed: %v", err)
	}
	if n := countInviteEmails(t, h.db, h.tenantID); n != 1 {
		t.Fatalf("invite email rows after the retry: got %d, want 1", n)
	}
}

// TestInviteRefusedWhenEmailNotConfigured: the accept link exists only in
// the email, so on a server with no SMTP an invite is a dead end. Invite
// refuses with email_not_configured before any invitation row exists — a
// later invite of the same address, once email works, is not blocked.
func TestInviteRefusedWhenEmailNotConfigured(t *testing.T) {
	h := newHarness(t, "Team NoSMTP")
	ctx := postgres.WithLivemode(context.Background(), false)

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
	ctx := postgres.WithLivemode(context.Background(), false)

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
			res, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: testPassword})
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

// TestAcceptInvite_RejectedPasswordKeepsTheInvite: a new account's password
// is checked before the invitation is used up. Before, the token was claimed
// first and the password checked after it, so a password that is too
// short, too long or on the breached list burned the invitation: the invitee
// saw "too short", fixed it, and got "this invitation is no longer valid".
//
// Mutation: commit the claim on its own and check the password after it, and
// the retry with a good password is refused.
func TestAcceptInvite_RejectedPasswordKeepsTheInvite(t *testing.T) {
	h := newHarness(t, "Team Retry Password")
	ctx := postgres.WithLivemode(context.Background(), false)

	if _, err := h.svc.Invite(ctx, h.tenantID, h.owner, "retry@example.com"); err != nil {
		t.Fatalf("invite: %v", err)
	}
	token := h.email.lastToken(t)

	for _, bad := range []string{"short", strings.Repeat("x", 73)} {
		_, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: bad})
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != "password" {
			t.Fatalf("accept with %d-char password: err = %v, want a password validation error", len(bad), err)
		}
	}
	if _, err := h.svc.PreviewInvite(ctx, token); err != nil {
		t.Fatalf("the invitation must still be pending after a rejected password: %v", err)
	}
	res, err := h.svc.AcceptInvite(ctx, dashmembers.AcceptInput{Token: token, Password: testPassword})
	if err != nil {
		t.Fatalf("accept with a good password after a rejected one: %v", err)
	}
	if !res.NewAccount || res.RawSessionID == "" {
		t.Errorf("accept result = %+v, want a new account with a session", res)
	}
}
