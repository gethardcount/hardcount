package user

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/request/requesttest"
	"github.com/gethardcount/hardcount/internal/session"
)

// countingResetEmail records sends so a test can prove the capped path sends
// nothing.
type countingResetEmail struct{ calls int }

func (c *countingResetEmail) SendPasswordReset(context.Context, string, string, string) error {
	c.calls++
	return nil
}

// TestRequestPasswordReset_CappedAccountIsSilentlySkipped: an account over
// its send budget gets no email and the same 200 as an address with no
// account, so the reply does not reveal the account. The cap is routine, so
// it logs at WARN, not ERROR, and the request is marked as audited.
func TestRequestPasswordReset_CappedAccountIsSilentlySkipped(t *testing.T) {
	const email = "victim@example.com"
	target := domain.User{ID: "usr_capped", Email: email}
	userStore := &fakeUserStore{
		loginUser:      &target,
		tenants:        []domain.UserTenant{{TenantID: "ten_1"}},
		createTokenErr: ErrResetSendCapped,
	}
	sender := &countingResetEmail{}
	h := NewHandler(newFakeService(userStore), session.DefaultCookieConfig("local"), sender, "https://dash.example.test", true)

	// Capture logs for the level assertion; restore the default after.
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	body, _ := json.Marshal(map[string]string{"email": email})
	req := httptest.NewRequest(http.MethodPost, "/password-reset/request", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	// The request's record, as the server's root makes it: the capped arm
	// must DECLARE the request accounted for (MarkSkip), or the coverage
	// detector reports this mutating 2xx as a mutation that lost its audit
	// row.
	reqCtx := requesttest.Context(req.Context())
	req = req.WithContext(reqCtx)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("capped request must keep the generic 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The response must be byte-identical to the one an address with NO
	// account gets. This cap is keyed on the account, so only a real account
	// can be capped — any distinguishable reply would be a reliable
	// account-existence oracle (four requests and an attacker knows).
	unknownStore := &fakeUserStore{}
	unknownH := NewHandler(newFakeService(unknownStore), session.DefaultCookieConfig("local"), &countingResetEmail{}, "https://dash.example.test", true)
	unknownBody, _ := json.Marshal(map[string]string{"email": "nobody@example.com"})
	unknownReq := httptest.NewRequest(http.MethodPost, "/password-reset/request", strings.NewReader(string(unknownBody)))
	unknownReq.Header.Set("Content-Type", "application/json")
	unknownReq = unknownReq.WithContext(requesttest.Context(unknownReq.Context()))
	unknownRec := httptest.NewRecorder()
	unknownH.Routes().ServeHTTP(unknownRec, unknownReq)

	if rec.Code != unknownRec.Code || rec.Body.String() != unknownRec.Body.String() {
		t.Fatalf("capped reply is distinguishable from the no-account reply — that is an account-existence oracle\n capped:  %d %s\n unknown: %d %s",
			rec.Code, rec.Body.String(), unknownRec.Code, unknownRec.Body.String())
	}
	if sender.calls != 0 {
		t.Fatalf("capped account: %d email(s) sent, want 0", sender.calls)
	}
	var sawWarn, sawError bool
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.Level == "ERROR" {
			sawError = true
			t.Logf("unexpected ERROR record: %s", rec.Msg)
		}
		if rec.Level == "WARN" && strings.Contains(rec.Msg, "throttled") {
			sawWarn = true
		}
	}
	if sawError {
		t.Error("a routine per-account cap must not log at ERROR — it would read as a system failure to anyone alerting on error rate")
	}
	if !sawWarn {
		t.Error("a capped send must leave a WARN record naming the throttle")
	}
	if !audit.WasHandled(reqCtx) {
		t.Fatal("the capped path must MarkSkip — otherwise the audit-coverage detector reads this 2xx as a mutation that lost its row")
	}
}
