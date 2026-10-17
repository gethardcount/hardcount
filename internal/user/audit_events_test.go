package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/session"
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

func newAuthHandler(t *testing.T, store *fakeUserStore) (*Handler, *recordingAuditRecorder) {
	t.Helper()
	h := NewHandler(NewService(store, clock.Real()), session.NewService(&recordingSessionStore{}),
		session.DefaultCookieConfig(), stubEmailSender{}, "", false)
	rec := &recordingAuditRecorder{}
	h.SetAuditLogger(rec)
	return h, rec
}

// TestLogin_AuditsSuccessAsOperator: a successful login writes one audit row
// attributed to the operator (actor_type=user, the user id) — not 'system' —
// scoped to their tenant. Pins C3 + the C1 actor resolution end-to-end.
func TestLogin_AuditsSuccessAsOperator(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("a-good-password-123"), bcrypt.DefaultCost)
	u := domain.User{ID: "usr_1", Email: "op@acme.com", PasswordHash: string(hash)}
	store := &fakeUserStore{loginUser: &u, tenants: []domain.UserTenant{{UserID: "usr_1", TenantID: "ten_acme"}}}
	h, rec := newAuthHandler(t, store)

	body, _ := json.Marshal(loginReq{Email: "op@acme.com", Password: "a-good-password-123"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	req.RemoteAddr = "203.0.113.9:5555"
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("login status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if len(rec.calls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(rec.calls))
	}
	c := rec.calls[0]
	if c.action != "login" || c.tenantID != "ten_acme" || c.resourceID != "usr_1" {
		t.Errorf("audit row = %+v, want action=login tenant=ten_acme resource=usr_1", c)
	}
	if c.actorType != "user" || c.actorID != "usr_1" {
		t.Errorf("actor = %s/%s, want user/usr_1 (operator, not system)", c.actorType, c.actorID)
	}
}

// TestLogin_FailureNotInAuditLog: a failed login writes NO per-tenant audit row
// — it can't (no tenant pre-auth, enumeration risk) and goes to the security
// slog instead. Guards against a regression that scopes failed logins to a
// tenant via an enumeration-leaking lookup.
func TestLogin_FailureNotInAuditLog(t *testing.T) {
	h, rec := newAuthHandler(t, &fakeUserStore{}) // loginUser nil → GetByEmail misses → bad creds

	body, _ := json.Marshal(loginReq{Email: "nobody@acme.com", Password: "some-long-password"})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if len(rec.calls) != 0 {
		t.Errorf("failed login wrote %d audit rows, want 0 (security slog only): %+v", len(rec.calls), rec.calls)
	}
}

// TestConfirmPasswordReset_Audits: a completed password reset writes a
// password_reset_completed row attributed to the account owner, scoped to their
// tenant (resolved via TenantForUser since domain.User carries none).
func TestConfirmPasswordReset_Audits(t *testing.T) {
	const userID = "usr_reset"
	plaintext := "reset-token-plaintext-0123456789abcdef"
	store := &fakeUserStore{
		user:           domain.User{ID: userID, Email: "op@acme.com"},
		resetTokenHash: hashResetToken(plaintext),
		tenants:        []domain.UserTenant{{UserID: userID, TenantID: "ten_acme"}},
	}
	h, rec := newAuthHandler(t, store)

	body, _ := json.Marshal(confirmResetReq{Token: plaintext, Password: "a-sufficiently-long-password"})
	req := httptest.NewRequest(http.MethodPost, "/password-reset/confirm", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if len(rec.calls) != 1 || rec.calls[0].action != "password_reset_completed" {
		t.Fatalf("audit calls = %+v, want one password_reset_completed", rec.calls)
	}
	if rec.calls[0].actorType != "user" || rec.calls[0].actorID != userID {
		t.Errorf("actor = %s/%s, want user/%s", rec.calls[0].actorType, rec.calls[0].actorID, userID)
	}
}
