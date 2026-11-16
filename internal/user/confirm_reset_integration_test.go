package user

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The password reset's all-or-nothing rule, on real Postgres. Each test
// gives the user two sessions (one per mode) and a live reset link. The
// failure tests make one step of the reset fail and check that nothing
// changed. The full flow over the real router is in internal/api
// (TestConfirmReset_EndsEverySession).

const (
	resetITEmail       = "owner@confirm-reset.test"
	resetITOldPassword = "confirm-reset-old-pw-12"
	resetITNewPassword = "confirm-reset-new-pw-34"
)

type resetIT struct {
	db       *postgres.DB
	tenantID string // the user's first-joined workspace
	userID   string
	token    string    // the plaintext from the reset link
	sessions [2]string // raw cookie values: test mode, live mode
	sessSvc  *session.Service
}

func newResetIT(t *testing.T) resetIT {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Confirm Reset")
	u := CreateTestUser(t, db, resetITEmail, resetITOldPassword, tenantID, "owner")
	sessSvc := session.NewService(session.NewPostgresStore(db))
	it := resetIT{db: db, tenantID: tenantID, userID: u.ID, sessSvc: sessSvc}
	for i, live := range []bool{false, true} {
		raw, _, err := sessSvc.Issue(t.Context(), session.IssueInput{UserID: u.ID, TenantID: tenantID, Livemode: live})
		if err != nil {
			t.Fatalf("issue session (live=%v): %v", live, err)
		}
		it.sessions[i] = raw
	}
	token, _, _, err := it.service(sessSvc, audit.NewLogger(db)).IssueResetToken(t.Context(), resetITEmail)
	if err != nil || token == "" {
		t.Fatalf("issue reset token: token %q, err %v", token, err)
	}
	it.token = token
	return it
}

func (it resetIT) service(sessions SessionTxWriter, auditor AuditTxWriter) *Service {
	return NewService(NewPostgresStore(it.db), clock.Real(), sessions, auditor)
}

func (it resetIT) handler(sessions SessionTxWriter, auditor AuditTxWriter) http.Handler {
	return NewHandler(it.service(sessions, auditor), it.sessSvc, session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false).Routes()
}

// confirm posts the reset form with password to a handler whose service
// uses sessions and auditor for the steps inside the reset's transaction.
func (it resetIT) confirm(t *testing.T, password string, sessions SessionTxWriter, auditor AuditTxWriter) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(ConfirmResetReq{Token: it.token, Password: password})
	req := httptest.NewRequest(http.MethodPost, "/password-reset/confirm", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	it.handler(sessions, auditor).ServeHTTP(rec, req)
	return rec
}

// errParam returns the error envelope's param, failing unless rec is a 422.
func errParam(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the error: %v; body %s", err, rec.Body.String())
	}
	return body.Error.Param
}

// auditRows counts the user's password_reset_completed rows, read outside
// row-level security.
func (it resetIT) auditRows(t *testing.T) (n int, tenantIDs []string) {
	t.Helper()
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(t.Context(), `
		SELECT tenant_id FROM audit_log
		WHERE action = 'password_reset_completed' AND resource_id = $1`, it.userID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		tenantIDs = append(tenantIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return len(tenantIDs), tenantIDs
}

// assertPasswordAndLinkUnchanged checks that the old password hash still
// verifies, the link still checks valid and no audit row was written.
func (it resetIT) assertPasswordAndLinkUnchanged(t *testing.T) {
	t.Helper()
	u, err := NewPostgresStore(it.db).GetByID(t.Context(), it.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(u.PasswordHash, resetITOldPassword); err != nil {
		t.Errorf("old password: %v, want it unchanged", err)
	}
	if err := it.service(it.sessSvc, audit.NewLogger(it.db)).CheckResetToken(t.Context(), it.token); err != nil {
		t.Errorf("check the reset link: %v, want nil (token unspent)", err)
	}
	if n, _ := it.auditRows(t); n != 0 {
		t.Errorf("password_reset_completed rows = %d, want 0", n)
	}
}

// assertNothingChanged also checks that the old password signs in and both
// sessions are still live.
func (it resetIT) assertNothingChanged(t *testing.T) {
	t.Helper()
	it.assertPasswordAndLinkUnchanged(t)
	svc := it.service(it.sessSvc, audit.NewLogger(it.db))
	if _, err := svc.Login(context.Background(), LoginInput{Email: resetITEmail, Password: resetITOldPassword}); err != nil {
		t.Errorf("sign in with the old password: %v, want success", err)
	}
	for i, raw := range it.sessions {
		if _, err := it.sessSvc.Resolve(context.Background(), raw); err != nil {
			t.Errorf("resolve session %d: %v, want it still live", i, err)
		}
	}
}

type failingAuditTx struct{}

func (failingAuditTx) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("audit insert failed")
}

// endThenFail ends the sessions on the transaction, then fails, so a test
// sees whether the sign-out itself rolls back.
type endThenFail struct{ *session.Service }

func (e endThenFail) EndAllForUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	if err := e.Service.EndAllForUserTx(ctx, tx, userID); err != nil {
		return err
	}
	return errors.New("session sign-out failed")
}

// notFoundEnder fails inside the reset's transaction with a not-found.
type notFoundEnder struct{ SessionTxWriter }

func (notFoundEnder) EndAllForUserTx(context.Context, *sql.Tx, string) error {
	return errs.ErrNotFound
}

// TestConfirmReset_AFailedAuditRowLeavesNoChange: when the audit row cannot
// be written, the reset answers 500 and the password, the token and the
// sessions are as they were.
func TestConfirmReset_AFailedAuditRowLeavesNoChange(t *testing.T) {
	it := newResetIT(t)
	if rec := it.confirm(t, resetITNewPassword, it.sessSvc, failingAuditTx{}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("confirm with a failing audit row: status %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	it.assertNothingChanged(t)
}

// TestConfirmReset_AFailedSignOutLeavesNoChange: the sessions are ended on
// the transaction and then a step fails; the reset answers 500, and the
// sign-out rolls back with the password and the token.
func TestConfirmReset_AFailedSignOutLeavesNoChange(t *testing.T) {
	it := newResetIT(t)
	if rec := it.confirm(t, resetITNewPassword, endThenFail{it.sessSvc}, audit.NewLogger(it.db)); rec.Code != http.StatusInternalServerError {
		t.Fatalf("confirm with a failing sign-out: status %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	it.assertNothingChanged(t)
}

// TestConfirmReset_ANotFoundInsideTheTransactionIsNotADeadLink: only the
// store's own dead-link cases become the token error. A not-found from a
// step inside the transaction is refused as itself, and nothing changes.
func TestConfirmReset_ANotFoundInsideTheTransactionIsNotADeadLink(t *testing.T) {
	it := newResetIT(t)
	rec := it.confirm(t, resetITNewPassword, notFoundEnder{}, audit.NewLogger(it.db))
	if rec.Code < 400 || rec.Code == http.StatusUnprocessableEntity {
		t.Fatalf("confirm with a not-found inside the transaction: status %d, want a refusal that is not the dead-link 422; body %s", rec.Code, rec.Body.String())
	}
	it.assertNothingChanged(t)
}

// TestConfirmReset_ARefusedPasswordSpendsNothing: a live link with a
// password that fails the rules answers 422 on password, and the link, the
// password and the sessions are as they were.
func TestConfirmReset_ARefusedPasswordSpendsNothing(t *testing.T) {
	it := newResetIT(t)
	if got := errParam(t, it.confirm(t, "short", it.sessSvc, audit.NewLogger(it.db))); got != "password" {
		t.Errorf("error param = %q, want password", got)
	}
	it.assertNothingChanged(t)
}

// TestConfirmReset_EndsSessionsInEveryWorkspace: a user who belongs to a
// second workspace has a session there too. A reset ends it with the others,
// and the audit row goes to the first-joined workspace.
func TestConfirmReset_EndsSessionsInEveryWorkspace(t *testing.T) {
	it := newResetIT(t)
	second := testutil.CreateTestTenant(t, it.db, "Confirm Reset Second")
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := NewPostgresStore(it.db).AttachTenantInTx(t.Context(), tx, it.userID, second, "member"); err != nil {
		t.Fatalf("join the second workspace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	other, _, err := it.sessSvc.Issue(t.Context(), session.IssueInput{UserID: it.userID, TenantID: second, Livemode: false})
	if err != nil {
		t.Fatalf("issue a session in the second workspace: %v", err)
	}

	if rec := it.confirm(t, resetITNewPassword, it.sessSvc, audit.NewLogger(it.db)); rec.Code != http.StatusOK {
		t.Fatalf("confirm: status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	for name, raw := range map[string]string{"test mode": it.sessions[0], "live mode": it.sessions[1], "second workspace": other} {
		if _, err := it.sessSvc.Resolve(t.Context(), raw); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("resolve the %s session after the reset: err = %v, want session.ErrNotFound", name, err)
		}
	}
	if n, tenants := it.auditRows(t); n != 1 || tenants[0] != it.tenantID {
		t.Errorf("password_reset_completed rows in tenants %v, want one in %s (the first-joined workspace)", tenants, it.tenantID)
	}
}

// TestConfirmReset_UserWithNoWorkspaceGetsTheDeadLinkAnswer: a user removed
// from their only workspace after requesting the link cannot sign in, and an
// audit row would have no tenant. The page's check and the confirm both
// answer the dead-link 422, and nothing changes.
func TestConfirmReset_UserWithNoWorkspaceGetsTheDeadLinkAnswer(t *testing.T) {
	it := newResetIT(t)
	// Raw SQL: member removal lives in internal/dashmembers, which imports
	// this package, and it also ends the member's sessions, which this test
	// checks separately. The state is the one removal leaves: no membership.
	tx, err := it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `DELETE FROM user_tenants WHERE user_id = $1`, it.userID); err != nil {
		t.Fatalf("remove the membership: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	check := httptest.NewRecorder()
	it.handler(it.sessSvc, audit.NewLogger(it.db)).ServeHTTP(check,
		httptest.NewRequest(http.MethodGet, "/password-reset/check?token="+url.QueryEscape(it.token), nil))
	if got := errParam(t, check); got != "token" {
		t.Errorf("check error param = %q, want token", got)
	}

	if got := errParam(t, it.confirm(t, resetITNewPassword, it.sessSvc, audit.NewLogger(it.db))); got != "token" {
		t.Errorf("confirm error param = %q, want token", got)
	}

	// The transaction refuses by itself too: a membership can be removed
	// between the look at the link and the spend.
	if _, err := NewPostgresStore(it.db).ResetPassword(t.Context(), hashResetToken(it.token), "$2a$12$unused-hash-for-this-test",
		func(*sql.Tx, string) error { return nil }); !errors.Is(err, errResetLinkDead) {
		t.Errorf("ResetPassword for a user with no workspace: %v, want the dead-link error", err)
	}

	// The link now checks dead by rule, so look at the token row itself.
	var usedAt sql.NullTime
	tx, err = it.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRowContext(t.Context(), `SELECT used_at FROM password_reset_tokens WHERE token_hash = $1`,
		hashResetToken(it.token)).Scan(&usedAt); err != nil {
		t.Fatal(err)
	}
	if usedAt.Valid {
		t.Errorf("token used_at = %v, want unspent", usedAt.Time)
	}
	u, err := NewPostgresStore(it.db).GetByID(t.Context(), it.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(u.PasswordHash, resetITOldPassword); err != nil {
		t.Errorf("old password after the refused reset: %v, want it unchanged", err)
	}
	for i, raw := range it.sessions {
		if _, err := it.sessSvc.Resolve(t.Context(), raw); err != nil {
			t.Errorf("resolve session %d: %v, want it still live", i, err)
		}
	}
}
