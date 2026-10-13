package credit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// liveStub is the engine's side of GET /v1/credits/balance/{id}/live.
type liveStub struct {
	live  domain.CreditLiveBalance
	err   error
	calls int
}

func (l *liveStub) LiveCreditBalance(_ context.Context, _, customerID string) (domain.CreditLiveBalance, error) {
	l.calls++
	out := l.live
	out.CustomerID = customerID
	return out, l.err
}

// knownCustomers answers the scoped customer read: ids in the set exist in
// the caller's tenant and mode, every other id is not found (which is what
// row-level security returns for another tenant's customer).
type knownCustomers map[string]bool

func (k knownCustomers) Get(_ context.Context, _, id string) (domain.Customer, error) {
	if !k[id] {
		return domain.Customer{}, errs.ErrNotFound
	}
	return domain.Customer{ID: id}, nil
}

func liveHandler(live LiveBalanceReader) *Handler {
	svc := NewService(newMemStore())
	svc.SetCustomerReader(knownCustomers{"cus_mine": true})
	if live != nil {
		svc.SetLiveBalanceReader(live)
	}
	return NewHandler(svc)
}

func TestGetLiveBalance_ReturnsThePartsAndTheResult(t *testing.T) {
	at := time.Date(2026, 10, 13, 9, 30, 0, 0, time.UTC)
	h := liveHandler(&liveStub{live: domain.CreditLiveBalance{
		BalanceCents: 4000, UnbilledUsageCents: 1000, EstimatedRemainingCents: 3000, ComputedAt: at,
	}})

	rr := creditReq(t, h.getLiveBalance, http.MethodGet, "", map[string]string{"customer_id": "cus_mine"})
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// Decoded into a map so the test pins the wire names, and that nothing
	// beyond the five documented fields is sent.
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := map[string]any{
		"customer_id":               "cus_mine",
		"balance_cents":             float64(4000),
		"unbilled_usage_cents":      float64(1000),
		"estimated_remaining_cents": float64(3000),
		"computed_at":               "2026-10-13T09:30:00Z",
	}
	if len(body) != len(want) {
		t.Errorf("body has %d fields, want %d: %v", len(body), len(want), body)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("%s = %v, want %v", k, body[k], v)
		}
	}
}

// A customer id from another tenant or the other mode answers 404, and the
// aggregation never runs for it. Without the customer read the answer would
// be 200 with zeros, because row-level security hides every row behind the
// figure.
func TestGetLiveBalance_CustomerNotTheCallers_Returns404(t *testing.T) {
	live := &liveStub{live: domain.CreditLiveBalance{BalanceCents: 4000}}
	h := liveHandler(live)

	rr := creditReq(t, h.getLiveBalance, http.MethodGet, "", map[string]string{"customer_id": "cus_someone_elses"})
	if rr.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if live.calls != 0 {
		t.Errorf("the live figure was computed %d times for a customer the caller cannot see, want 0", live.calls)
	}
}

// A figure that could not be computed is a 500, never a 200 with a guess.
func TestGetLiveBalance_ComputeFailure_Returns500(t *testing.T) {
	for name, live := range map[string]LiveBalanceReader{
		"the engine errors":       &liveStub{err: errors.New("rate meter: rule missing")},
		"no live reader is wired": nil,
	} {
		t.Run(name, func(t *testing.T) {
			rr := creditReq(t, liveHandler(live).getLiveBalance, http.MethodGet, "", map[string]string{"customer_id": "cus_mine"})
			if rr.Code != http.StatusInternalServerError {
				t.Errorf("got %d, want 500; body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

// The route is registered on the credit router, which the API mounts behind
// the permission only secret keys and dashboard sessions hold.
func TestCreditRoutes_LiveBalanceIsRouted(t *testing.T) {
	h := liveHandler(&liveStub{})
	req := httptest.NewRequest(http.MethodGet, "/balance/cus_mine/live", nil)
	req = req.WithContext(auth.WithTenantID(req.Context(), "tnt_test"))
	rr := httptest.NewRecorder()
	h.Routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("GET /balance/cus_mine/live through the router: got %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}
