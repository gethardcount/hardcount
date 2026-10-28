package usage

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	mw "github.com/gethardcount/hardcount/internal/api/middleware"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
)

// A LiteLLM call is written whole (#1044 held semantics per key, one
// transaction per call). The mapper turns one call into up to five events
// that share a customer and a meter: input, cache_read, cache_write_5m,
// cache_write_1h and output. Writing them in one transaction means a crash
// or a store failure leaves all of the call or none of it, which the replay
// guard below depends on.

// CallEvent is one usage row of a call. The customer and meter are the
// call's.
type CallEvent struct {
	Quantity       decimal.Decimal
	Dimensions     map[string]any
	IdempotencyKey string
	// Timestamp nil means the customer's effective now, as for Ingest.
	Timestamp *time.Time
	// ObservedCostMicros is the provider's own cost for this row (ADR-079
	// D4); nil falls to the rate table.
	ObservedCostMicros *int64
}

// CallInput is one call whose customer and meter exist.
type CallInput struct {
	CustomerID string
	MeterID    string
	// Events is the call as the current mapper records it.
	Events []CallEvent
	// Legacy is the same call as the mapper before the cache-write split
	// recorded it, nil when the two agree. LegacyKey is the key both carry
	// with different quantities (the call's input key): if a row is already
	// stored under it with Legacy's quantity, the call was first recorded
	// the old way and is written as Legacy, so its cache writes, already
	// inside that input row, are not billed a second time.
	Legacy    []CallEvent
	LegacyKey string
}

// HoldCallInput is one call whose customer or meter does not exist yet.
type HoldCallInput struct {
	Source             string
	ExternalCustomerID string
	EventName          string
	Reason             domain.HeldUsageReason
	Events             []CallEvent
	Legacy             []CallEvent
	LegacyKey          string
}

// CallResult reports what writing one call did.
type CallResult struct {
	// Inserted counts rows newly written (or newly held).
	Inserted int
	// Deduplicated counts rows already stored, already held, or held and
	// then discarded.
	Deduplicated int
	// Legacy is true when the call was written the pre-split way.
	Legacy bool
	// Written lists, for IngestCallResolvingHeld, the indices of the rows it
	// newly wrote to usage_events, into the mapping it wrote (the events,
	// or the legacy events when Legacy is true). IngestCall counts the
	// unbilled rows from it.
	Written []int
	// Unbilled counts, for IngestCall, the rows written by billing_status
	// (open_period.go).
	Unbilled UnbilledCounts
}

// CallInputKey is the key of the call's input row for a cache-write row's
// key, and false for any other key. It follows the key layout LiteLLM's
// mapper writes, <call id>:<role> (pinned by a test there); ReleaseHeld uses
// it to find a held cache-write row's input row.
func CallInputKey(key string) (string, bool) {
	for _, suf := range []string{":cache_write_5m", ":cache_write_1h"} {
		if strings.HasSuffix(key, suf) {
			return strings.TrimSuffix(key, suf) + ":input", true
		}
	}
	return "", false
}

// IngestCall writes one call's events in one transaction. Each event gets
// Ingest's checks and timestamp rules; a key held and then discarded is not
// written, and a key still held is written and its held row released (see
// PostgresStore.IngestCallResolvingHeld). The customer's open periods are
// read before the write, and the rows the store newly wrote, in the mapping
// it wrote, are classified on its transaction after the inserts
// (open_period.go); only they are counted by billing_status.
func (s *Service) IngestCall(ctx context.Context, tenantID string, in CallInput) (CallResult, error) {
	if s.held == nil {
		return CallResult{}, ErrHoldUnavailable
	}
	// One clock lookup for the call: build reads the bound instant.
	sim, err := s.simNow(ctx, tenantID, in.CustomerID)
	if err != nil {
		return CallResult{}, err
	}
	ctx = clock.WithSim(ctx, sim)
	// Both mappings are built, the store writes one, and only that one is
	// recorded as late: a replay would otherwise count rows never written.
	prep := func(evs []CallEvent) ([]domain.UsageEvent, []domain.UsageEvent, error) {
		if evs == nil {
			return nil, nil, nil
		}
		out := make([]domain.UsageEvent, 0, len(evs))
		var late []domain.UsageEvent
		for _, e := range evs {
			ev, isLate, err := s.build(ctx, tenantID, IngestInput{
				CustomerID:         in.CustomerID,
				MeterID:            in.MeterID,
				Quantity:           e.Quantity,
				Dimensions:         e.Dimensions,
				IdempotencyKey:     e.IdempotencyKey,
				Timestamp:          e.Timestamp,
				ObservedCostMicros: e.ObservedCostMicros,
			}, domain.UsageOriginAPI)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, ev)
			if isLate {
				late = append(late, ev)
			}
		}
		return out, late, nil
	}
	events, eventsLate, err := prep(in.Events)
	if err != nil {
		return CallResult{}, err
	}
	legacy, legacyLate, err := prep(in.Legacy)
	if err != nil {
		return CallResult{}, err
	}
	// The open periods are read BEFORE the write, the meter's aggregation on
	// the write's transaction after the inserts: see open_period.go.
	periods, err := s.openPeriodsFor(ctx, tenantID, in.CustomerID, periodCacheFrom(ctx))
	if err != nil {
		return CallResult{}, err
	}
	var writtenClass map[int]Class
	res, err := s.held.IngestCallResolvingHeld(ctx, tenantID, StoreCall{Events: events, Legacy: legacy, LegacyKey: in.LegacyKey}, func(tx *sql.Tx, res CallResult) error {
		// Set afresh on every run: the store runs the call again after a
		// deadlock.
		writtenClass = make(map[int]Class, len(res.Written))
		evs := events
		if res.Legacy {
			evs = legacy
		}
		meters := map[string]MeterBuckets{}
		for _, i := range res.Written {
			c, err := s.classifyWritten(ctx, tx, evs[i], periods, meters)
			if err != nil {
				return err
			}
			writtenClass[i] = c
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	written, writtenLate := events, eventsLate
	if res.Legacy {
		written, writtenLate = legacy, legacyLate
	}
	for _, ev := range writtenLate {
		recordLate(ctx, ev)
	}
	if res.Inserted > 0 {
		mw.RecordUsageIngested(res.Inserted)
	}
	// Only rows this call wrote: a row deduplicated against an earlier write
	// was flagged, or not, when that write happened.
	for _, i := range res.Written {
		status := s.statusOf(writtenClass[i])
		res.Unbilled.Add(status)
		recordUnbilled(ctx, written[i], status)
	}
	return res, nil
}

// HoldCall holds one call's events in one transaction, each with
// heldEvent's checks. A key already held is counted as deduplicated.
func (s *Service) HoldCall(ctx context.Context, tenantID string, in HoldCallInput) (CallResult, error) {
	if s.held == nil {
		return CallResult{}, ErrHoldUnavailable
	}
	prep := func(evs []CallEvent) ([]domain.HeldUsageEvent, error) {
		if evs == nil {
			return nil, nil
		}
		out := make([]domain.HeldUsageEvent, 0, len(evs))
		for _, e := range evs {
			ev, err := heldEvent(HoldInput{
				Source:             in.Source,
				ExternalCustomerID: in.ExternalCustomerID,
				EventName:          in.EventName,
				Quantity:           e.Quantity,
				Dimensions:         e.Dimensions,
				IdempotencyKey:     e.IdempotencyKey,
				EventAt:            e.Timestamp,
				ObservedCostMicros: e.ObservedCostMicros,
				Reason:             in.Reason,
			})
			if err != nil {
				return nil, err
			}
			out = append(out, ev)
		}
		return out, nil
	}
	events, err := prep(in.Events)
	if err != nil {
		return CallResult{}, err
	}
	legacy, err := prep(in.Legacy)
	if err != nil {
		return CallResult{}, err
	}
	res, err := s.held.HoldCall(ctx, tenantID, HeldCall{Events: events, Legacy: legacy, LegacyKey: in.LegacyKey})
	if err == nil && res.Inserted > 0 {
		heldUsageEvents.WithLabelValues(string(in.Reason)).Add(float64(res.Inserted))
	}
	return res, err
}
