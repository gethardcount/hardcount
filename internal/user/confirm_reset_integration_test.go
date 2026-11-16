package user

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// The password reset's all-or-nothing rule, on real Postgres. Each test
// gives the user two sessions (one per mode) and a live reset link, makes one
// step of the reset fail, and checks that nothing changed. The successful
// reset is tested over the real router in internal/api
// (TestConfirmReset_EndsEverySession).

const (
	resetITEmail       = "owner@confirm-reset.test"
	resetITOldPassword = "confirm-reset-old-pw-12"
	resetITNewPassword = "confirm-reset-new-pw-34"
)

type resetIT struct {
	db       *postgres.DB
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
	it := resetIT{db: db, userID: u.ID, sessSvc: sessSvc}
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

func (it resetIT) service(sessions SessionEnder, auditor AuditTxWriter) *Service {
	return NewService(NewPostgresStore(it.db), clock.Real(), sessions, auditor)
}

// confirm posts the reset form to a handler whose service uses sessions and
// auditor for the steps inside the reset's transaction.
func (it resetIT) confirm(t *testing.T, sessions SessionEnder, auditor AuditTxWriter) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHandler(it.service(sessions, auditor), it.sessSvc, session.CookieConfig{Path: "/"}, stubEmailSender{}, "", false)
	body, _ := json.Marshal(ConfirmResetReq{Token: it.token, Password: resetITNewPassword})
	req := httptest.NewRequest(http.MethodPost, "/password-reset/confirm", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec
}

// assertNothingChanged checks that the old password still signs in, the
// link still works and both sessions are still live.
func (it resetIT) assertNothingChanged(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	svc := it.service(it.sessSvc, audit.NewLogger(it.db))
	if _, _, err := svc.Authenticate(ctx, resetITEmail, resetITOldPassword); err != nil {
		t.Errorf("sign in with the old password: %v, want success (password unchanged)", err)
	}
	if err := svc.CheckResetToken(ctx, it.token); err != nil {
		t.Errorf("check the reset link: %v, want nil (token unspent)", err)
	}
	for i, raw := range it.sessions {
		if _, err := it.sessSvc.Resolve(ctx, raw); err != nil {
			t.Errorf("resolve session %d: %v, want it still live", i, err)
		}
	}
}

type failingAuditTx struct{}

func (failingAuditTx) LogInTx(context.Context, *sql.Tx, audit.Entry) error {
	return errors.New("audit insert failed")
}

type failingSessionEnder struct{}

func (failingSessionEnder) EndAllForUserTx(context.Context, *sql.Tx, string) error {
	return errors.New("session sign-out failed")
}

// TestConfirmReset_AFailedAuditRowLeavesNoChange: when the audit row cannot
// be written, the reset answers 500 and the password, the token and the
// sessions are as they were.
func TestConfirmReset_AFailedAuditRowLeavesNoChange(t *testing.T) {
	it := newResetIT(t)
	if rec := it.confirm(t, it.sessSvc, failingAuditTx{}); rec.Code != http.StatusInternalServerError {
		t.Fatalf("confirm with a failing audit row: status %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	it.assertNothingChanged(t)
}

// TestConfirmReset_AFailedSignOutLeavesNoChange: when the sessions cannot be
// ended, the reset answers 500 and the password and the token are as they
// were.
func TestConfirmReset_AFailedSignOutLeavesNoChange(t *testing.T) {
	it := newResetIT(t)
	if rec := it.confirm(t, failingSessionEnder{}, audit.NewLogger(it.db)); rec.Code != http.StatusInternalServerError {
		t.Fatalf("confirm with a failing sign-out: status %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	it.assertNothingChanged(t)
}

// TestConfirmReset_UserWithNoWorkspaceGetsTheDeadLinkAnswer: a user removed
// from their only workspace after requesting the link cannot sign in, and an
// audit row would have no tenant. The confirm answers the same 422 as an
// unknown link and changes nothing.
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

	rec := it.confirm(t, it.sessSvc, audit.NewLogger(it.db))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("confirm for a user with no workspace: status %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Error struct {
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the error: %v; body %s", err, rec.Body.String())
	}
	if body.Error.Param != "token" || body.Error.Message != "reset token is invalid, expired, or already used" {
		t.Errorf("error = %+v, want param token and the dead-link message", body.Error)
	}

	// Authenticate refuses a user with no membership, so check the password
	// hash directly.
	u, err := NewPostgresStore(it.db).GetByID(t.Context(), it.userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPassword(u.PasswordHash, resetITOldPassword); err != nil {
		t.Errorf("old password after the refused reset: %v, want it unchanged", err)
	}
	svc := it.service(it.sessSvc, audit.NewLogger(it.db))
	if err := svc.CheckResetToken(t.Context(), it.token); err != nil {
		t.Errorf("check the reset link: %v, want nil (token unspent)", err)
	}
	for i, raw := range it.sessions {
		if _, err := it.sessSvc.Resolve(t.Context(), raw); err != nil {
			t.Errorf("resolve session %d: %v, want it still live", i, err)
		}
	}
}
