package payment

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// statusSetupStore answers GetPaymentSetup with a fixed result.
type statusSetupStore struct {
	ps  domain.CustomerPaymentSetup
	err error
}

func (s statusSetupStore) UpsertPaymentSetup(context.Context, string, domain.CustomerPaymentSetup) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{}, errors.New("not used")
}

func (s statusSetupStore) UpsertPaymentSetupAudited(context.Context, string, domain.CustomerPaymentSetup, func(*sql.Tx) error) (domain.CustomerPaymentSetup, error) {
	return domain.CustomerPaymentSetup{}, errors.New("not used")
}

func (s statusSetupStore) GetPaymentSetup(context.Context, string, string) (domain.CustomerPaymentSetup, error) {
	return s.ps, s.err
}

// TestCheckoutStatus_ErrorsAreNotMissing pins D-06 on the older card-status
// route: an unknown customer is 404 and a failed read is 500. Both used to
// answer 200 with setup_status "missing", which reads as "this customer has
// no card".
func TestCheckoutStatus_ErrorsAreNotMissing(t *testing.T) {
	cases := []struct {
		name  string
		store statusSetupStore
		want  int
	}{
		{"unknown customer", statusSetupStore{err: errs.ErrNotFound}, http.StatusNotFound},
		{"read failure", statusSetupStore{err: errors.New("connection reset")}, http.StatusInternalServerError},
		{"known customer", statusSetupStore{ps: domain.CustomerPaymentSetup{CustomerID: "vlx_cus_1", SetupStatus: domain.PaymentSetupReady}}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &CheckoutHandler{store: tc.store}
			r := chi.NewRouter()
			r.Get("/status/{customerID}", h.getPaymentStatus)
			req := httptest.NewRequest(http.MethodGet, "/status/vlx_cus_1", nil).
				WithContext(auth.WithTenantID(context.Background(), "vlx_ten_1"))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}
