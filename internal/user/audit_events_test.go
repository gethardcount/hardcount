package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/session"
)

// recordingAuditRecorder records the action of each audit row the handler
// writes outside a transaction.
type recordingAuditRecorder struct {
	actions []string
}

func (r *recordingAuditRecorder) Log(_ context.Context, _, action, _, _, _ string, _ map[string]any) error {
	r.actions = append(r.actions, action)
	return nil
}

func newAuthHandler(t *testing.T, store *fakeUserStore) (*Handler, *recordingAuditRecorder, *recordingAuditTx) {
	t.Helper()
	svc, auditor := newRecordingService(store)
	h := NewHandler(svc, session.DefaultCookieConfig("local"), stubEmailSender{}, "", false)
	rec := &recordingAuditRecorder{}
	h.SetAuditLogger(rec)
	return h, rec, auditor
}

// TestLogin_FailureNotInAuditLog: a failed login writes no audit row through
// either writer. Before sign-in there is no tenant to file it under, and
// finding one would tell a guesser the email exists; it goes to the security
// log instead.
func TestLogin_FailureNotInAuditLog(t *testing.T) {
	h, rec, auditor := newAuthHandler(t, &fakeUserStore{}) // loginUser nil → GetByEmail misses → bad creds

	body, _ := json.Marshal(LoginReq{Email: "nobody@acme.com", Password: "some-long-password"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(rec.actions) != 0 || len(auditor.rows) != 0 {
		t.Errorf("failed login wrote %d + %d audit rows, want 0 (security slog only): %v %+v", len(rec.actions), len(auditor.rows), rec.actions, auditor.rows)
	}
}
