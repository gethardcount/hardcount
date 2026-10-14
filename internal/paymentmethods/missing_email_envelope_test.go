package paymentmethods

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
)

type noEmailLookup struct{}

func (noEmailLookup) GetForSetupLink(context.Context, string, string) (string, string, error) {
	return "", "Acme", nil
}

type unusedEmailer struct{}

func (unusedEmailer) SendPaymentSetupLink(context.Context, string, string, string, string, string) error {
	return nil
}

// TestSendSetupEmail_MissingEmailEnvelope: a customer with no email answers
// 422 type invalid_request_error, code missing_email. The type used to be
// "invalid_state", which no other endpoint uses and no client can expect.
func TestSendSetupEmail_MissingEmailEnvelope(t *testing.T) {
	h := NewHandler(nil)
	h.SetCustomerLookup(noEmailLookup{})
	h.SetEmailer(unusedEmailer{})
	h.SetEmailConfigured(true)

	r := chi.NewRouter()
	r.Mount("/customers/{customer_id}/payment-methods", h.OperatorRoutes())

	req := httptest.NewRequest(http.MethodPost, "/customers/vlx_cus_1/payment-methods/send-setup-email", nil).
		WithContext(auth.WithTenantID(context.Background(), "vlx_ten_t1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if env.Error.Type != "invalid_request_error" || env.Error.Code != "missing_email" {
		t.Errorf("error = %+v, want invalid_request_error/missing_email", env.Error)
	}
}

type emailLookup struct{}

func (emailLookup) GetForSetupLink(context.Context, string, string) (string, string, error) {
	return "ap@acme.test", "Acme", nil
}

// TestSendSetupEmail_RefusesWhenEmailNotConfigured: the setup email is
// queued, so on a server with no SMTP the endpoint used to answer 202 and the
// queued row then failed unseen. It now answers 409 email_not_configured
// before a Stripe setup session is created (svc is nil here: reaching
// CreateSetupSession would panic).
func TestSendSetupEmail_RefusesWhenEmailNotConfigured(t *testing.T) {
	h := NewHandler(nil)
	h.SetCustomerLookup(emailLookup{})
	h.SetEmailer(unusedEmailer{})

	r := chi.NewRouter()
	r.Mount("/customers/{customer_id}/payment-methods", h.OperatorRoutes())
	req := httptest.NewRequest(http.MethodPost, "/customers/vlx_cus_1/payment-methods/send-setup-email", nil).
		WithContext(auth.WithTenantID(context.Background(), "vlx_ten_t1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "email_not_configured" || env.Error.Message != "Email isn't set up on this server, so nothing was sent." {
		t.Errorf("error = %+v", env.Error)
	}
}
