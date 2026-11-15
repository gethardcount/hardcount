package usage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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
			_, errs := s.BatchIngest(testMode(), "t1", []IngestInput{{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)}})
			if len(errs) != 1 {
				return fmt.Errorf("%d errors: %v", len(errs), errs)
			}
			return errs[0]
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
// the clock's deletion) goes on at real time: the server's clock.
func TestIngest_ANotFoundGoesOnAtRealTime(t *testing.T) {
	serverNow := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore())
	svc.SetClock(clock.NewFake(serverNow))
	svc.SetResolver(&answerResolver{err: fmt.Errorf("read test clock clk_1: %w", errs.ErrNotFound)})
	e, err := svc.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)})
	if err != nil || !e.Timestamp.Equal(serverNow) {
		t.Errorf("err %v, timestamp %v; want accepted at the server's time %v", err, e.Timestamp, serverNow)
	}
}

// A customer on no clock is recorded at real time as the server reads it
// (the clock it bills by), not the machine's time.
func TestIngest_ACustomerOnNoClockTakesTheServersClock(t *testing.T) {
	serverNow := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore())
	svc.SetClock(clock.NewFake(serverNow))
	svc.SetResolver(&answerResolver{sim: clock.Sim{At: serverNow}})
	e, err := svc.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)})
	if err != nil || !e.Timestamp.Equal(serverNow) {
		t.Errorf("err %v, timestamp %v; want the server's time %v", err, e.Timestamp, serverNow)
	}
}

// A request whose time was decided upstream for a clock (a route that named
// the customer) is not decided again.
func TestIngest_AClockDecidedUpstreamIsNotDecidedAgain(t *testing.T) {
	frozen := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	res := &answerResolver{err: errors.New("must not be read")}
	svc := NewService(newMemStore())
	svc.SetResolver(res)
	ctx := clock.WithSim(testMode(), clock.Sim{At: frozen, TestClockID: "clk_up"})
	e, err := svc.Ingest(ctx, "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)})
	if err != nil || !e.Timestamp.Equal(frozen) || res.reads != 0 {
		t.Errorf("err %v, timestamp %v, reads %d; want the upstream clock's %v and no read", err, e.Timestamp, res.reads, frozen)
	}
}
