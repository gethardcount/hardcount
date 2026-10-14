package usage

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// errLookupInfra stands in for what the real lookup stores return when
// Postgres gives no answer: the raw driver error, never errs.ErrNotFound.
var errLookupInfra = errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

type stubCustomers struct {
	err   error
	calls int
}

func (s *stubCustomers) GetByExternalID(_ context.Context, _, externalID string) (domain.Customer, error) {
	s.calls++
	if s.err != nil {
		return domain.Customer{}, s.err
	}
	return domain.Customer{ID: "cus_" + externalID}, nil
}

type stubMeters struct{ err error }

func (s *stubMeters) GetMeterByKey(_ context.Context, _, key string) (domain.Meter, error) {
	if s.err != nil {
		return domain.Meter{}, s.err
	}
	return domain.Meter{ID: "mtr_" + key}, nil
}

const lookupEvent = `{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":"evt_lookup"}`

// ingestDoors lists the three public ingest doors with a body each accepts.
// All three resolve the customer and meter before they write anything.
func ingestDoors(h *Handler) map[string]struct {
	fn   func(http.ResponseWriter, *http.Request)
	body string
} {
	return map[string]struct {
		fn   func(http.ResponseWriter, *http.Request)
		body string
	}{
		"single":   {h.ingest, lookupEvent},
		"batch":    {h.batchIngest, "[" + lookupEvent + "]"},
		"backfill": {h.Backfill, lookupEvent},
	}
}

// TestUsageIngest_LookupStoreFailureIs503 pins that a lookup the store could
// not answer is a NON-verdict: 503 api_error/ingest_unavailable, so the client
// retries. Before, every lookup error was written as 422 "customer/meter not
// found", and a client that treats 4xx as permanent dropped the usage. The raw
// driver error never reaches the body (ADR-026).
// Mutation-verify: drop the errors.Is(err, errs.ErrNotFound) split in resolve
// → 422 at every door.
func TestUsageIngest_LookupStoreFailureIs503(t *testing.T) {
	lookups := map[string]func() *Handler{
		"customer": func() *Handler {
			return NewHandler(NewService(newMemStore()), &stubCustomers{err: errLookupInfra}, &stubMeters{})
		},
		"meter": func() *Handler {
			return NewHandler(NewService(newMemStore()), &stubCustomers{}, &stubMeters{err: errLookupInfra})
		},
	}
	for lookup, newHandler := range lookups {
		for door, d := range ingestDoors(newHandler()) {
			t.Run(door+"/"+lookup, func(t *testing.T) {
				rr := usageReq(t, d.fn, d.body)
				body := rr.Body.String()
				if rr.Code != http.StatusServiceUnavailable {
					t.Fatalf("status %d, want 503; body %s", rr.Code, body)
				}
				if !strings.Contains(body, `"type":"api_error"`) || !strings.Contains(body, `"code":"ingest_unavailable"`) {
					t.Errorf("body should carry api_error/ingest_unavailable: %s", body)
				}
				if strings.Contains(body, "not found") {
					t.Errorf("store failure masqueraded as an unknown %s: %s", lookup, body)
				}
				if strings.Contains(body, "5432") || strings.Contains(body, "connection refused") {
					t.Errorf("driver error leaked to the caller: %s", body)
				}
			})
		}
	}
}

// Negative control: a customer or meter that really does not exist is a
// verdict and stays 422. Without it, a handler that answered 503 for every
// lookup error would pass the test above and make clients retry forever.
func TestUsageIngest_UnknownCustomerOrMeterStays422(t *testing.T) {
	lookups := map[string]func() *Handler{
		"customer": func() *Handler {
			return NewHandler(NewService(newMemStore()), &stubCustomers{err: errs.ErrNotFound}, &stubMeters{})
		},
		"meter": func() *Handler {
			return NewHandler(NewService(newMemStore()), &stubCustomers{}, &stubMeters{err: errs.ErrNotFound})
		},
	}
	for lookup, newHandler := range lookups {
		for door, d := range ingestDoors(newHandler()) {
			t.Run(door+"/"+lookup, func(t *testing.T) {
				rr := usageReq(t, d.fn, d.body)
				body := rr.Body.String()
				if rr.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status %d, want 422; body %s", rr.Code, body)
				}
				if !strings.Contains(body, "not found") {
					t.Errorf("body should name the unknown %s: %s", lookup, body)
				}
			})
		}
	}
}

// One lookup failure answers 503 for the WHOLE batch, even when another event
// in it is invalid: a 422 listing only the invalid index would tell the client
// the rest of the batch had been checked. The batch also stops at the first
// failure rather than sending every remaining event at a store that is down.
func TestUsageBatch_LookupStoreFailureRefusesWholeBatch(t *testing.T) {
	cust := &stubCustomers{err: errLookupInfra}
	h := NewHandler(NewService(newMemStore()), cust, &stubMeters{})

	// event[0] fails validation before any lookup; events 1 and 2 reach the store.
	body := `[{"event_name":"api_call","quantity":1,"idempotency_key":"evt_0"},` + lookupEvent + `,` + lookupEvent + `]`
	rr := usageReq(t, h.batchIngest, body)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503; body %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "batch_rejected") {
		t.Errorf("store failure reported as a rejected batch: %s", rr.Body.String())
	}
	if cust.calls != 1 {
		t.Errorf("customer lookups = %d, want 1 (stop at the first store failure)", cust.calls)
	}
}
