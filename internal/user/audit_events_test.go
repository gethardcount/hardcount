package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/session"
)

// recordingAuditRecorder captures each audit write. It resolves the actor from
// the ctx the handler stamped (the same audit.ResolveActor the real Logger.Log
// runs), so a test can assert the operator — not 'system' — was attributed.
type recordingAuditRecorder struct {
	calls []recordedAudit
}

type recordedAudit struct {
	tenantID, action, resourceType, resourceID, label string
	actorType, actorID                                string
	ip                                                string
	meta                                              map[string]any
}

func (r *recordingAuditRecorder) Log(ctx context.Context, tenantID, action, resourceType, resourceID, label string, meta map[string]any) error {
	at, aid := audit.ResolveActor(ctx)
	r.calls = append(r.calls, recordedAudit{tenantID, action, resourceType, resourceID, label, at, aid, audit.ClientIP(ctx), meta})
	return nil
}

func newAuthHandler(t *testing.T, store *fakeUserStore) (*Handler, *recordingAuditRecorder, *recordingAuditTx) {
	t.Helper()
	svc, _, auditor := newRecordingService(store)
	h := NewHandler(svc, session.DefaultCookieConfig("local"), stubEmailSender{}, "", false)
	rec := &recordingAuditRecorder{}
	h.SetAuditLogger(rec)
	return h, rec, auditor
}

// TestLogin_AuditsSuccessAsOperator: a successful login writes one audit row
// on the sign-in's transaction, attributed to the operator (actor_type=user,
// the user id), not 'system', for the user's account. The real
// transaction is in login_integration_test.go.
func TestLogin_AuditsSuccessAsOperator(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("a-good-password-123"), bcrypt.DefaultCost)
	u := domain.User{ID: "usr_1", Email: "op@acme.com", PasswordHash: string(hash)}
	store := &fakeUserStore{loginUser: &u, tenants: []domain.UserTenant{{UserID: "usr_1", TenantID: "ten_acme"}}}
	h, rec, auditor := newAuthHandler(t, store)

	body, _ := json.Marshal(LoginReq{Email: "op@acme.com", Password: "a-good-password-123"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	req.RemoteAddr = "203.0.113.9:5555"
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if len(auditor.rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(auditor.rows))
	}
	c := auditor.rows[0]
	if c.entry.Action != "login" || c.entry.ResourceType != "user" || c.entry.ResourceID != "usr_1" {
		t.Errorf("audit row = %+v, want action=login resource=user/usr_1", c.entry)
	}
	if c.entry.Metadata["livemode"] != false {
		t.Errorf("metadata = %v, want livemode false", c.entry.Metadata)
	}
	if c.actorType != "user" || c.actorID != "usr_1" {
		t.Errorf("actor = %s/%s, want user/usr_1 (operator, not system)", c.actorType, c.actorID)
	}
	if c.ip != "203.0.113.9" {
		t.Errorf("audit row IP = %q, want 203.0.113.9", c.ip)
	}
	if len(rec.calls) != 0 {
		t.Errorf("login also wrote %d row(s) outside its transaction: %+v", len(rec.calls), rec.calls)
	}
}

// TestLogin_FailureNotInAuditLog: a failed login writes NO per-tenant audit row
// — it can't (no tenant pre-auth, enumeration risk) and goes to the security
// slog instead. Guards against a regression that scopes failed logins to a
// tenant via an enumeration-leaking lookup.
func TestLogin_FailureNotInAuditLog(t *testing.T) {
	h, rec, auditor := newAuthHandler(t, &fakeUserStore{}) // loginUser nil → GetByEmail misses → bad creds

	body, _ := json.Marshal(LoginReq{Email: "nobody@acme.com", Password: "some-long-password"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(rec.calls) != 0 || len(auditor.rows) != 0 {
		t.Errorf("failed login wrote %d + %d audit rows, want 0 (security slog only): %+v %+v", len(rec.calls), len(auditor.rows), rec.calls, auditor.rows)
	}
}
