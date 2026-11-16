package usage

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// answerResolver answers every customer with sim, or fails with err, and
// counts the reads.
type answerResolver struct {
	sim   clock.Sim
	err   error
	reads int
}

func (a *answerResolver) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	a.reads++
	return a.sim, a.err
}
func (a *answerResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return a.sim, a.err
}
func (a *answerResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return a.sim, a.err
}

func testMode() context.Context { return postgres.WithLivemode(context.Background(), false) }

// A customer whose test clock cannot be read: every ingest shape refuses the
// event as unavailable (503), with nothing recorded, so the client sends it
// again.
func TestIngest_AnUnreadableClockRefusesTheEvent(t *testing.T) {
	unreadable := errors.New("connection reset by peer")
	past := time.Now().UTC().Add(-time.Hour)
	for _, c := range []struct {
		name   string
		ingest func(*Service) error
	}{
		{"single", func(s *Service) error {
			_, err := s.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)})
			return err
		}},
		{"batch", func(s *Service) error {
			_, batchErrs := s.BatchIngest(testMode(), "t1", []IngestInput{{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)}})
			if len(batchErrs) != 1 {
				return fmt.Errorf("%d errors: %v", len(batchErrs), batchErrs)
			}
			return batchErrs[0]
		}},
		{"backfill", func(s *Service) error {
			_, err := s.Backfill(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &past})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := newMemStore()
			svc := NewService(store)
			svc.SetResolver(&answerResolver{err: unreadable})
			err := c.ingest(svc)
			if !errors.Is(err, errs.ErrUnavailable) || !errors.Is(err, clock.ErrUndecided) {
				t.Fatalf("err %v; want unavailable over clock.ErrUndecided", err)
			}
			if len(store.events) != 0 {
				t.Errorf("%d events recorded; want none", len(store.events))
			}
		})
	}
}

// A not-found under the decision (the customer, or its clock in a race with
// the clock's deletion) goes on at real time, read from the service's clock.
func TestIngest_ANotFoundGoesOnAtRealTime(t *testing.T) {
	serverNow := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore())
	svc.SetClock(clock.NewFake(serverNow))
	svc.SetResolver(&answerResolver{err: fmt.Errorf("read test clock clk_1: %w", errs.ErrNotFound)})
	e, err := svc.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)})
	if err != nil || !e.Timestamp.Equal(serverNow) {
		t.Errorf("err %v, timestamp %v; want accepted at real time %v", err, e.Timestamp, serverNow)
	}
}

// A customer on no clock is recorded at real time, read from the service's
// clock: not the instant the resolver's answer carries, and not the
// machine's time.
func TestIngest_ACustomerOnNoClockIsRecordedAtRealTime(t *testing.T) {
	serverNow := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore())
	svc.SetClock(clock.NewFake(serverNow))
	svc.SetResolver(&answerResolver{sim: clock.Sim{At: serverNow.Add(-7 * time.Hour)}})
	e, err := svc.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)})
	if err != nil || !e.Timestamp.Equal(serverNow) {
		t.Errorf("err %v, timestamp %v; want real time from the service's clock %v", err, e.Timestamp, serverNow)
	}
}

// A request whose clock was already decided (a route that named the
// customer) is not decided again.
func TestIngest_AClockAlreadyDecidedIsNotDecidedAgain(t *testing.T) {
	frozen := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	res := &answerResolver{err: errors.New("must not be read")}
	svc := NewService(newMemStore())
	svc.SetResolver(res)
	ctx := clock.WithSim(testMode(), clock.Sim{At: frozen, TestClockID: "clk_up"})
	e, err := svc.Ingest(ctx, "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)})
	if err != nil || !e.Timestamp.Equal(frozen) || res.reads != 0 {
		t.Errorf("err %v, timestamp %v, reads %d; want the decided clock's %v and no read", err, e.Timestamp, res.reads, frozen)
	}
}

// customerResolver answers each customer from its own entry, fails for a
// customer with none (an unreadable clock), and counts the reads.
type customerResolver struct {
	sims  map[string]clock.Sim
	reads int
}

func (c *customerResolver) SimForCustomer(_ context.Context, _, customerID string) (clock.Sim, error) {
	c.reads++
	if sim, ok := c.sims[customerID]; ok {
		return sim, nil
	}
	return clock.Sim{}, fmt.Errorf("read the test clock of %s: connection reset by peer", customerID)
}
func (c *customerResolver) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (c *customerResolver) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// In one batch, each event is dated by its own customer: customers on two
// different clocks and one on none, interleaved, and nothing one event's
// customer decided carries to the next event.
func TestBatchIngest_EachEventIsDatedByItsOwnCustomer(t *testing.T) {
	serverNow := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	frozenA := time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC)
	frozenC := time.Date(2027, 8, 20, 6, 0, 0, 0, time.UTC)
	store := newMemStore()
	svc := NewService(store)
	svc.SetClock(clock.NewFake(serverNow))
	res := &customerResolver{sims: map[string]clock.Sim{
		"cus_a": {At: frozenA, TestClockID: "clk_a"},
		"cus_b": {At: serverNow.Add(-7 * time.Hour)}, // on no clock
		"cus_c": {At: frozenC, TestClockID: "clk_c"},
	}}
	svc.SetResolver(res)
	want := map[string]time.Time{}
	var events []IngestInput
	for i, c := range []struct {
		customer string
		at       time.Time
	}{{"cus_a", frozenA}, {"cus_b", serverNow}, {"cus_c", frozenC}, {"cus_b", serverNow}, {"cus_a", frozenA}} {
		key := fmt.Sprintf("evt-%d", i)
		want[key] = c.at
		events = append(events, IngestInput{CustomerID: c.customer, MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: key})
	}
	if _, batchErrs := svc.BatchIngest(testMode(), "t1", events); len(batchErrs) != 0 {
		t.Fatalf("batch: %v", batchErrs)
	}
	if len(store.events) != len(events) {
		t.Fatalf("%d events stored; want %d", len(store.events), len(events))
	}
	for _, e := range store.events {
		if !e.Timestamp.Equal(want[e.IdempotencyKey]) {
			t.Errorf("%s (%s) at %v; want %v", e.IdempotencyKey, e.CustomerID, e.Timestamp, want[e.IdempotencyKey])
		}
	}
	if res.reads != len(events) {
		t.Errorf("%d resolver reads; want one per event (%d)", res.reads, len(events))
	}
}

// A batch stops at the first event whose customer's clock cannot be read:
// the rest of a batch it refuses are not read, and nothing is recorded.
func TestBatchIngest_StopsAtTheFirstUnreadableClock(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)
	res := &customerResolver{sims: map[string]clock.Sim{
		"cus_1": {At: time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC), TestClockID: "clk_1"},
		"cus_3": {At: time.Date(2025, 1, 10, 12, 0, 0, 0, time.UTC), TestClockID: "clk_3"},
	}}
	svc.SetResolver(res)
	_, batchErrs := svc.BatchIngest(testMode(), "t1", []IngestInput{
		{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)},
		{CustomerID: "cus_unreadable", MeterID: "mtr_1", Quantity: dec(1)},
		{CustomerID: "cus_3", MeterID: "mtr_1", Quantity: dec(1)},
	})
	if len(batchErrs) != 1 || !errors.Is(batchErrs[0], errs.ErrUnavailable) {
		t.Fatalf("errors %v; want the one unavailable event", batchErrs)
	}
	if res.reads != 2 {
		t.Errorf("%d clock reads; want 2 (none after the unreadable event)", res.reads)
	}
	if len(store.events) != 0 {
		t.Errorf("%d events recorded; want none", len(store.events))
	}
}

// A read whose window defaults to the customer's now (margin; customer usage
// shares customerNow) is refused for retry when the clock cannot be read,
// with a 503 that carries no cause.
func TestMargin_AnUnreadableClockIsRefusedForRetry(t *testing.T) {
	svc := NewService(newMemStore())
	svc.SetResolver(&answerResolver{err: errors.New("connection reset by peer")})
	h := NewProviderCostHandler(nil, NewMarginAssembler(nil, NewCustomerUsageService(svc, nil, nil, nil)))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", "cus_1")
	ctx := context.WithValue(auth.WithTenantID(testMode(), "t1"), chi.RouteCtxKey, route)
	rec := httptest.NewRecorder()
	h.Margin(rec, httptest.NewRequest("GET", "/v1/customers/cus_1/margin", nil).WithContext(ctx))
	body := rec.Body.String()
	if rec.Code != 503 || !strings.Contains(body, "test_clock_unavailable") || !strings.Contains(body, "Try again in a moment") || strings.Contains(body, "connection reset") {
		t.Errorf("margin: %d %s; want 503 test_clock_unavailable, a read's message, no cause", rec.Code, body)
	}
}

// secondReadFails answers its first read with a customer on no clock and
// fails every later one.
type secondReadFails struct{ reads int }

func (r *secondReadFails) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	r.reads++
	if r.reads == 1 {
		return clock.Sim{At: time.Now().UTC()}, nil
	}
	return clock.Sim{}, errors.New("connection reset by peer")
}
func (r *secondReadFails) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}
func (r *secondReadFails) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{}, errors.New("not used")
}

// For a customer on no clock the cost dashboard's usage view reads the
// customer's now a second time; when that read fails, the page answers with
// its own plain refusal, not the usage read's.
func TestCostDashboard_AFailedSecondClockReadIsThePagesRefusal(t *testing.T) {
	cu, _, subs := usageFixture(domain.RatedSubscription{})
	res := &secondReadFails{}
	cu.usage.SetResolver(res)
	a := NewCostDashboardAssembler(fakeTokenLookup{cust: domain.Customer{ID: "cus_1", TenantID: "t1"}}, cu, subs, &fakeCredits{})
	a.SetLiveBalanceReader(&fakeLive{})
	a.SetResolver(res)
	_, err := a.GetByToken(context.Background(), "hc_pcd_x")
	var de *errs.DomainError
	if !errors.Is(err, errs.ErrUnavailable) || !errors.As(err, &de) || de.Message != costDashboardUnavailable || res.reads != 2 {
		t.Fatalf("err %v, reads %d; want the page's refusal after the second read", err, res.reads)
	}
}

// Every entry checks the event before it decides the customer's time: an
// invalid event is refused as invalid, with no clock read, even when the
// customer's clock cannot be read. Besides a missing meter, the rows a real
// caller can reach (its handler resolves the meter first): a backfill with
// bad dimensions or a quantity of 13 decimal places, and a LiteLLM call
// whose Legacy mapping alone is invalid.
func TestUsage_AnInvalidEventIsRefusedBeforeItsClockIsRead(t *testing.T) {
	unreadable := errors.New("connection reset by peer")
	past := time.Now().UTC().Add(-time.Hour)
	for _, c := range []struct {
		name   string
		ingest func(*Service) error
	}{
		{"single", func(s *Service) error {
			_, err := s.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_1", Quantity: dec(1)})
			return err
		}},
		{"batch", func(s *Service) error {
			_, batchErrs := s.BatchIngest(testMode(), "t1", []IngestInput{{CustomerID: "cus_1", Quantity: dec(1)}})
			if len(batchErrs) != 1 {
				return fmt.Errorf("%d errors: %v", len(batchErrs), batchErrs)
			}
			return batchErrs[0]
		}},
		{"backfill", func(s *Service) error {
			_, err := s.Backfill(testMode(), "t1", IngestInput{CustomerID: "cus_1", Quantity: dec(1), Timestamp: &past})
			return err
		}},
		{"a LiteLLM call", func(s *Service) error {
			_, err := s.IngestCall(testMode(), "t1", CallInput{CustomerID: "cus_1", Events: []CallEvent{{Quantity: dec(1), IdempotencyKey: "c:input"}}})
			return err
		}},
		{"backfill, bad dimensions", func(s *Service) error {
			_, err := s.Backfill(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &past,
				Dimensions: map[string]any{"model": map[string]any{"nested": "object"}}})
			return err
		}},
		{"backfill, a quantity of 13 decimal places", func(s *Service) error {
			_, err := s.Backfill(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: decimal.RequireFromString("1.0000000000001"), Timestamp: &past})
			return err
		}},
		{"a LiteLLM call whose Legacy mapping alone is invalid", func(s *Service) error {
			_, err := s.IngestCall(testMode(), "t1", CallInput{CustomerID: "cus_1", MeterID: "mtr_1",
				Events:    []CallEvent{{Quantity: dec(1), IdempotencyKey: "c:input"}},
				Legacy:    []CallEvent{{Quantity: dec(1), IdempotencyKey: "c:input", Dimensions: map[string]any{"model": []any{"a", "b"}}}},
				LegacyKey: "c:input"})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc := NewService(&heldMemStore{memStore: newMemStore()})
			res := &answerResolver{err: unreadable}
			svc.SetResolver(res)
			err := c.ingest(svc)
			if !errors.Is(err, errs.ErrValidation) || errors.Is(err, errs.ErrUnavailable) || res.reads != 0 {
				t.Errorf("err %v, %d clock reads; want invalid (not unavailable), and no read", err, res.reads)
			}
		})
	}
}

// A LiteLLM call checks every event of both mappings before it builds any:
// a call with a future timestamp in Events and bad dimensions in Legacy is
// refused for the dimensions (a check), not for the timestamp (which build
// refuses). Both are invalid (422); the error's field is the dimensions.
func TestIngestCall_ChecksBothMappingsBeforeBuilding(t *testing.T) {
	svc := NewService(&heldMemStore{memStore: newMemStore()})
	svc.SetResolver(&answerResolver{sim: clock.Sim{At: time.Now().UTC()}})
	future := time.Now().UTC().Add(48 * time.Hour)
	_, err := svc.IngestCall(testMode(), "t1", CallInput{CustomerID: "cus_1", MeterID: "mtr_1",
		Events:    []CallEvent{{Quantity: dec(1), IdempotencyKey: "c:input", Timestamp: &future}},
		Legacy:    []CallEvent{{Quantity: dec(1), IdempotencyKey: "c:input", Dimensions: map[string]any{"model": []any{"a", "b"}}}},
		LegacyKey: "c:input"})
	var de *errs.DomainError
	if !errors.Is(err, errs.ErrValidation) || !errors.As(err, &de) || de.Field != "dimensions" {
		t.Errorf("err %v; want invalid, on the field dimensions", err)
	}
}

// build's caller decides; on a context nobody decided, build refuses with an
// internal error, and the ingest writes nothing, rather than stamp the
// machine's time.
func TestBuild_RefusesAnUndecidedContext(t *testing.T) {
	store := newMemStore()
	svc := NewService(store)
	_, err := svc.ingestAudited(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)}, domain.UsageOriginAPI, nil)
	if !errors.Is(err, errBuildUndecided) {
		t.Fatalf("err %v; want errBuildUndecided", err)
	}
	if len(store.events) != 0 {
		t.Errorf("%d events written; want none", len(store.events))
	}
}
