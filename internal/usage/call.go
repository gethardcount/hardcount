package usage

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// CallEvent is one usage row of a LiteLLM call.
type CallEvent struct {
	Quantity           decimal.Decimal
	Dimensions         map[string]any
	IdempotencyKey     string
	Timestamp          *time.Time
	ObservedCostMicros *int64
}

// CallInput is one LiteLLM call whose customer and meter exist.
type CallInput struct {
	CustomerID string
	MeterID    string
	Events     []CallEvent
	Legacy     []CallEvent
	LegacyKey  string
}

// HoldCallInput is one LiteLLM call whose customer or meter does not exist.
type HoldCallInput struct {
	Source             string
	ExternalCustomerID string
	MeterKey           string
	Reason             domain.HeldUsageReason
	Events             []CallEvent
	Legacy             []CallEvent
	LegacyKey          string
}

// CallResult reports what one call's write did.
type CallResult struct {
	Inserted     int
	Deduplicated int
	Legacy       bool
}

// IngestCall (stub: one event at a time, as before).
func (s *Service) IngestCall(ctx context.Context, tenantID string, in CallInput) (CallResult, error) {
	var res CallResult
	for _, ev := range in.Events {
		_, err := s.IngestResolvingHeld(ctx, tenantID, IngestInput{
			CustomerID: in.CustomerID, MeterID: in.MeterID, Quantity: ev.Quantity, Dimensions: ev.Dimensions,
			IdempotencyKey: ev.IdempotencyKey, Timestamp: ev.Timestamp, ObservedCostMicros: ev.ObservedCostMicros,
		})
		switch {
		case err == nil:
			res.Inserted++
		case errors.Is(err, errs.ErrDuplicateKey):
			res.Deduplicated++
		default:
			return res, err
		}
	}
	return res, nil
}

// HoldCall (stub: one event at a time, as before).
func (s *Service) HoldCall(ctx context.Context, tenantID string, in HoldCallInput) (CallResult, error) {
	var res CallResult
	for _, ev := range in.Events {
		_, err := s.Hold(ctx, tenantID, HoldInput{
			Source: in.Source, ExternalCustomerID: in.ExternalCustomerID, MeterKey: in.MeterKey,
			Quantity: ev.Quantity, Dimensions: ev.Dimensions, IdempotencyKey: ev.IdempotencyKey,
			EventAt: ev.Timestamp, ObservedCostMicros: ev.ObservedCostMicros, Reason: in.Reason,
		})
		switch {
		case err == nil:
			res.Inserted++
		case errors.Is(err, errs.ErrDuplicateKey):
			res.Deduplicated++
		default:
			return res, err
		}
	}
	return res, nil
}
