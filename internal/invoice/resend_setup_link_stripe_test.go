package invoice

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// stripeDisconnected answers that Stripe is not connected for any mode.
type stripeDisconnected struct{}

func (stripeDisconnected) HasFor(context.Context, string, bool) (bool, error) {
	return false, nil
}

// stripeConnectedAll answers that Stripe is connected for any mode.
type stripeConnectedAll struct{}

func (stripeConnectedAll) HasFor(context.Context, string, bool) (bool, error) {
	return true, nil
}

// connectedService is a Service with Stripe connected, for tests of the
// resend-setup-link paths that run after the Stripe check.
func connectedService(store *memStore) *Service {
	svc := NewService(store, nil, nil)
	svc.SetStripeChecker(stripeConnectedAll{})
	return svc
}

// TestResendSetupLink_RefusesWithoutStripe: with no Stripe connected for the
// mode, the setup-link email tells the customer to add a card that can never
// be saved or charged. The route must answer 409 stripe_not_connected and
// send nothing.
func TestResendSetupLink_RefusesWithoutStripe(t *testing.T) {
	ctx := postgres.WithLivemode(context.Background(), true)
	store := newMemStore()
	inv, err := store.Create(ctx, "t1", domain.Invoice{
		InvoiceNumber: "INV-200", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentPending, AmountDueCents: 5000,
		CustomerID: "cus_1", Currency: "USD",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	notifier := &outcomeNoPMNotifier{outcome: domain.NotifySent}
	svc := NewService(store, nil, nil)
	svc.SetStripeChecker(stripeDisconnected{})
	h := &Handler{svc: svc, noPMNotifier: notifier, emailConfigured: true}

	req := httptest.NewRequest(http.MethodPost, "/"+inv.ID+"/resend-setup-link", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", inv.ID)
	reqCtx := auth.WithTenantID(req.Context(), "t1")
	reqCtx = postgres.WithLivemode(reqCtx, true)
	reqCtx = context.WithValue(reqCtx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.resendSetupLink(rr, req.WithContext(reqCtx))

	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409 stripe_not_connected. body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"code":"stripe_not_connected"`) {
		t.Errorf("body = %s, want code stripe_not_connected", rr.Body.String())
	}
	if notifier.calls != 0 {
		t.Errorf("notifier calls = %d, want 0: nothing may be sent without Stripe", notifier.calls)
	}
}
