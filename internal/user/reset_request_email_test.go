package user

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/session"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// capturingEmailSender records the context passed to SendPasswordReset so a
// test can assert the handler stamped an explicit livemode on it.
type capturingEmailSender struct {
	called  bool
	sendCtx context.Context
}

func (c *capturingEmailSender) SendPasswordReset(ctx context.Context, tenantID, email, resetLink string) error {
	c.called = true
	c.sendCtx = ctx
	return nil
}

// TestRequestPasswordReset_SendsWithCanonicalLivemode is the regression lock for
// the defect where the reset email never sent.
//
// requestPasswordReset runs pre-auth (the /v1/auth group has no session
// middleware), so its ctx carried no livemode — and the production email path
// is the outbox, whose TxTenant refuses to open without one. The send failed and
// the matched operator never got the link, while the audit row (which sets its
// own livemode) wrote fine, hiding the break: a token in the DB, an audit row,
// and no email.
//
// testutil.MustLivemode panics when the send ctx has no mode, and the assert
// pins it to FALSE (the account-plane canonical partition, same as the audit
// row), so the test passes only once the handler wraps the ctx.
func TestRequestPasswordReset_SendsWithCanonicalLivemode(t *testing.T) {
	u := domain.User{ID: "usr_1", Email: "op@acme.com"}
	store := &fakeUserStore{
		loginUser: &u, // GetByEmail matches → a token is issued and the send fires
		tenants:   []domain.UserTenant{{UserID: "usr_1", TenantID: "ten_acme"}},
	}
	email := &capturingEmailSender{}
	// Non-empty dashboardBaseURL so buildResetLink succeeds (else no send).
	h := NewHandler(NewService(store, clock.Real()), session.NewService(&recordingSessionStore{}),
		session.DefaultCookieConfig("local"), email, "https://app.example.com", true)
	h.SetAuditLogger(&recordingAuditRecorder{})

	body, _ := json.Marshal(RequestResetReq{Email: "op@acme.com"})
	req := httptest.NewRequest(http.MethodPost, "/password-reset/request", strings.NewReader(string(body)))
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if !email.called {
		t.Fatal("SendPasswordReset was never called — the matched account got no reset email")
	}
	if testutil.MustLivemode(email.sendCtx) {
		t.Error("reset email ctx is live — want the canonical partition (livemode=false), " +
			"the same partition as the audit row")
	}
}
