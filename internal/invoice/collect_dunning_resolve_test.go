package invoice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// recordingDunning records ResolveByInvoice calls.
type recordingDunning struct{ resolutions []domain.DunningResolution }

func (d *recordingDunning) ResolveByInvoice(_ context.Context, _, _ string, resolution domain.DunningResolution) error {
	d.resolutions = append(d.resolutions, resolution)
	return nil
}

// voidingCreditApplier drains the invoice to $0 and then voids it, standing in
// for a void that lands after the charge hold expired mid-collect. It is the
// one way SettleZeroDue hands back an invoice it did not settle.
type voidingCreditApplier struct{ store *memStore }

func (f *voidingCreditApplier) ApplyToInvoiceAt(_ context.Context, _, _, invoiceID string, amountCents int64, _ time.Time, _ ...string) (int64, error) {
	inv := f.store.invoices[invoiceID]
	inv.AmountDueCents = 0
	inv.CreditsAppliedCents += amountCents
	inv.Status = domain.InvoiceVoided
	f.store.invoices[invoiceID] = inv
	return amountCents, nil
}

func collectReq(t *testing.T, h *Handler, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/invoices/"+id+"/collect", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	reqCtx := context.WithValue(req.Context(), auth.TestTenantIDKey(), "t1")
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.collectPayment(rr, req.WithContext(reqCtx))
	return rr
}

// A collect that credit settles must close the dunning run as
// payment_recovered, the same resolution the card branch writes. A collect
// whose settle did not happen must leave the run open: closing it would stop
// dunning chasing money still owed.
func TestCollect_CreditSettle_ResolvesDunningOnlyWhenPaid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		applier func(*memStore) CreditApplier
		want    []domain.DunningResolution
	}{
		{
			name:    "credit covers it: run resolved as payment_recovered",
			applier: func(s *memStore) CreditApplier { return &fakeInvCreditApplier{store: s, applyCents: 5000} },
			want:    []domain.DunningResolution{domain.ResolutionPaymentRecovered},
		},
		{
			name:    "invoice not settled: run left open",
			applier: func(s *memStore) CreditApplier { return &voidingCreditApplier{store: s} },
			want:    nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newMemStore()
			inv, err := store.Create(context.Background(), "t1", domain.Invoice{
				InvoiceNumber: "INV-001", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
				AmountDueCents: 5000, CustomerID: "cus_1", Currency: "USD",
			})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			svc := NewService(store, nil, nil)
			svc.SetCreditApplier(tc.applier(store))
			charger := &fakeCharger{}
			dun := &recordingDunning{}
			h := &Handler{
				svc:           svc,
				charger:       charger,
				paymentSetups: &fakePaymentSetups{},
				dunning:       dun,
			}

			rr := collectReq(t, h, inv.ID)
			if rr.Code != http.StatusOK {
				t.Fatalf("collect: status %d, body %s", rr.Code, rr.Body.String())
			}
			if charger.called {
				t.Error("a credit-covered invoice must not reach the card")
			}
			if len(dun.resolutions) != len(tc.want) || (len(tc.want) > 0 && dun.resolutions[0] != tc.want[0]) {
				t.Errorf("dunning resolutions = %v, want %v", dun.resolutions, tc.want)
			}
		})
	}
}
