package usage

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

func testMode() context.Context { return postgres.WithLivemode(context.Background(), false) }

// An event with no timestamp is recorded at real time, read from the
// service's clock, not the machine's; one dated past that instant and the
// skew is refused as in the future.
func TestIngest_IsRecordedAtTheServersClock(t *testing.T) {
	serverNow := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	svc := NewService(newMemStore())
	svc.SetClock(clock.NewFake(serverNow))
	e, err := svc.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1)})
	if err != nil || !e.Timestamp.Equal(serverNow) {
		t.Errorf("err %v, timestamp %v; want real time from the service's clock %v", err, e.Timestamp, serverNow)
	}
	beyond := serverNow.Add(usageFutureSkew + time.Minute)
	if _, err := svc.Ingest(testMode(), "t1", IngestInput{CustomerID: "cus_1", MeterID: "mtr_1", Quantity: dec(1), Timestamp: &beyond}); err == nil {
		t.Error("an event dated past the server's time and the skew was accepted")
	}
}

// In one batch, each event binds its own instant: a read of the server's
// clock each, so nothing one event bound carries to the next.
func TestBatchIngest_EachEventBindsItsOwnInstant(t *testing.T) {
	past := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	store := newMemStore()
	svc := NewService(store)
	server := &usageSteppingClock{at: past}
	svc.SetClock(server)
	var events []IngestInput
	for i, customer := range []string{"cus_a", "cus_b", "cus_a"} {
		events = append(events, IngestInput{CustomerID: customer, MeterID: "mtr_1", Quantity: dec(1), IdempotencyKey: fmt.Sprintf("evt-%d", i)})
	}
	if _, batchErrs := svc.BatchIngest(testMode(), "t1", events); len(batchErrs) != 0 {
		t.Fatalf("batch: %v", batchErrs)
	}
	if len(store.events) != len(events) || server.reads != len(events) {
		t.Fatalf("%d events stored after %d server reads; want %d and one read each", len(store.events), server.reads, len(events))
	}
	seen := map[time.Time]bool{}
	for _, e := range store.events {
		if seen[e.Timestamp] {
			t.Errorf("%s shares %v with an earlier event; want its own instant", e.IdempotencyKey, e.Timestamp)
		}
		seen[e.Timestamp] = true
	}
}

// Every entry checks the event before it binds its time: an invalid event is
// refused as invalid, with no clock read. Besides a missing meter, the rows a
// real caller can reach (its handler resolves the meter first): a backfill
// with bad dimensions or a quantity of 13 decimal places, and a LiteLLM call
// whose Legacy mapping alone is invalid.
func TestUsage_AnInvalidEventIsRefusedBeforeItsClockIsRead(t *testing.T) {
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
			server := &usageSteppingClock{at: past}
			svc.SetClock(server)
			err := c.ingest(svc)
			if !errors.Is(err, errs.ErrValidation) || server.reads != 0 {
				t.Errorf("err %v, %d clock reads; want invalid, and no read", err, server.reads)
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

// build's caller binds; on a context with no instant, build refuses with an
// internal error, and the ingest writes nothing, rather than stamp the
// machine's time.
func TestBuild_RefusesAContextWithNoInstant(t *testing.T) {
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
