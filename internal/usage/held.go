package usage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/shopspring/decimal"

	mw "github.com/sagarsuperuser/velox/internal/api/middleware"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// Held usage (#1044, ADR-122).
//
// The LiteLLM callback must answer 200 for a row whose customer or meter
// does not exist, or LiteLLM resends the whole batch forever. Before this,
// such a row was then lost. Now it is held: kept in usage_held_events with
// the idempotency key it would have had, shown to the operator, and
// released into usage_events at most once when the operator says so.

// HoldInput is one usage row the caller could not ingest.
type HoldInput struct {
	Source             string
	ExternalCustomerID string
	MeterKey           string
	Quantity           decimal.Decimal
	Dimensions         map[string]any
	IdempotencyKey     string
	// EventAt is the row's own time. Nil means the time Velox received it.
	EventAt            *time.Time
	ObservedCostMicros *int64
	Reason             domain.HeldUsageReason
}

// ErrHoldUnavailable is returned by Hold when the service has no held
// store wired. Callers treat it like any store failure.
var ErrHoldUnavailable = errors.New("usage: held store not wired")

var heldUsageEvents *prometheus.CounterVec

func init() {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "velox_usage_held_events_total",
		Help: "Usage rows held because their customer or meter did not exist (LiteLLM callback). Counted once per new held row; a resend of a held row is not counted.",
	}, []string{"reason"})
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			heldUsageEvents = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	} else {
		heldUsageEvents = c
	}
}

// Hold keeps one row that could not be ingested. It runs the same row
// checks as ingest (dimensions, quantity, future time), so a held row can
// later be released without failing them. Returns errs.ErrDuplicateKey when
// the key is already held.
func (s *Service) Hold(ctx context.Context, tenantID string, in HoldInput) (domain.HeldUsageEvent, error) {
	if s.held == nil {
		return domain.HeldUsageEvent{}, ErrHoldUnavailable
	}
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		// A row without a key cannot be released safely: a resend would
		// bill it twice.
		return domain.HeldUsageEvent{}, errs.Invalid("idempotency_key", missingKeyMessage)
	}
	if utf8.RuneCountInString(in.IdempotencyKey) > MaxIdempotencyKeyLen {
		return domain.HeldUsageEvent{}, errs.Invalid("idempotency_key", longKeyMessage)
	}
	if strings.TrimSpace(in.ExternalCustomerID) == "" {
		return domain.HeldUsageEvent{}, errs.Required("external_customer_id")
	}
	if strings.TrimSpace(in.MeterKey) == "" {
		return domain.HeldUsageEvent{}, errs.Required("meter_key")
	}
	switch in.Reason {
	case domain.HeldReasonCustomerNotFound, domain.HeldReasonMeterNotFound:
	default:
		return domain.HeldUsageEvent{}, errs.Invalid("reason", "unknown hold reason")
	}
	if err := validateDimensions(in.Dimensions); err != nil {
		return domain.HeldUsageEvent{}, err
	}
	if in.Quantity.Abs().Cmp(maxQuantityMagnitude) >= 0 {
		return domain.HeldUsageEvent{}, errs.Invalid("quantity", "magnitude too large — must be less than 10^26")
	}
	// No customer exists, so there is no test clock to read: wall-clock is
	// the only "now" there is.
	now := time.Now().UTC()
	at := now
	if in.EventAt != nil {
		if in.EventAt.After(now.Add(usageFutureSkew)) {
			return domain.HeldUsageEvent{}, errs.Invalid("timestamp", "must not be in the future")
		}
		at = in.EventAt.UTC()
	}
	source := in.Source
	if source == "" {
		source = domain.HeldUsageSourceLiteLLM
	}
	out, err := s.held.Hold(ctx, tenantID, domain.HeldUsageEvent{
		Source:             source,
		ExternalCustomerID: in.ExternalCustomerID,
		MeterKey:           in.MeterKey,
		Quantity:           in.Quantity,
		Dimensions:         in.Dimensions,
		IdempotencyKey:     in.IdempotencyKey,
		EventAt:            at,
		ObservedCostMicros: in.ObservedCostMicros,
		Reason:             in.Reason,
	})
	if err == nil {
		heldUsageEvents.WithLabelValues(string(in.Reason)).Inc()
	}
	return out, err
}

// ListHeld returns the rows still held, grouped, with totals.
func (s *Service) ListHeld(ctx context.Context, tenantID string) (HeldSummary, error) {
	if s.held == nil {
		return HeldSummary{Groups: []HeldUsageGroup{}}, nil
	}
	return s.held.ListHeldSummary(ctx, tenantID)
}

// IngestResolvingHeld is Ingest for the LiteLLM callback. A row whose key
// was held and then discarded is not written (errs.ErrDuplicateKey, so the
// callback counts it as deduplicated), and a row whose key is still held is
// written and its held row marked released. See
// PostgresStore.IngestResolvingHeld. Without a held store no row can be
// held, so it is a plain Ingest.
func (s *Service) IngestResolvingHeld(ctx context.Context, tenantID string, input IngestInput) (domain.UsageEvent, error) {
	if s.held == nil {
		return s.Ingest(ctx, tenantID, input)
	}
	prepared, err := s.prepare(ctx, tenantID, input, domain.UsageOriginAPI)
	if err != nil {
		return domain.UsageEvent{}, err
	}
	event, err := s.held.IngestResolvingHeld(ctx, tenantID, prepared)
	if err == nil {
		mw.RecordUsageIngested(1)
	}
	return event, err
}

// HeldFor returns the meter keys and row count still held for one external
// customer id.
func (s *Service) HeldFor(ctx context.Context, tenantID, externalCustomerID string) ([]string, int, error) {
	if s.held == nil {
		return nil, 0, ErrHoldUnavailable
	}
	return s.held.HeldFor(ctx, tenantID, externalCustomerID)
}

// ReleaseInput names the held rows to release and the customer they go to.
type ReleaseInput struct {
	ExternalCustomerID string
	CustomerID         string
	// MeterIDs maps meter key to meter id for every meter that exists.
	MeterIDs map[string]string
}

// ReleaseResult is the response of POST /v1/usage-events/held/release.
type ReleaseResult struct {
	CustomerID string `json:"customer_id"`
	// Released counts rows newly written to usage_events.
	Released int `json:"released"`
	// Deduplicated counts rows usage_events already had under the same key
	// (LiteLLM resent the call after the customer existed). Marked released,
	// not written again.
	Deduplicated int `json:"deduplicated"`
	// StillHeld counts rows for this external id left held that a release
	// cannot take yet: their meter does not exist, or their time is after
	// the customer's current time.
	StillHeld int `json:"still_held"`
	// MoreToRelease counts rows left held only because one release takes at
	// most 5,000 rows. Release again to take them.
	MoreToRelease int `json:"more_to_release"`
	// InOpenPeriod counts released rows whose time falls in an active
	// subscription's current billing period and whose meter that
	// subscription's plan prices: that period's invoice counts them.
	InOpenPeriod int `json:"in_open_period"`
	// NotOnPlan counts released rows whose time falls in an active
	// subscription's current billing period, but whose meter no plan on that
	// subscription prices: no invoice charges them.
	NotOnPlan int `json:"not_on_plan"`
	// OutsideOpenPeriod counts released rows outside every current billing
	// period: before the subscription started, or in a period whose invoice
	// is already built. A subscription invoice does not count them (unless
	// that period's invoice has not been built yet); bill them with a one-off
	// invoice if they should be charged. Velox never moves usage onto a
	// later invoice (ADR-120).
	OutsideOpenPeriod int `json:"outside_open_period"`
}

// Release ingests the held rows for one external customer id into the
// given customer, at most once per idempotency key (see
// PostgresStore.ReleaseHeld). Rows stamped after the customer's current
// time stay held. subs are the customer's subscriptions and plans their
// items' plans by id, used only to say which released rows an open
// billing period's invoice charges.
func (s *Service) Release(ctx context.Context, tenantID string, in ReleaseInput, subs []domain.Subscription, plans map[string]domain.Plan) (ReleaseResult, error) {
	if s.held == nil {
		return ReleaseResult{}, ErrHoldUnavailable
	}
	if strings.TrimSpace(in.ExternalCustomerID) == "" {
		return ReleaseResult{}, errs.Required("external_customer_id")
	}
	if strings.TrimSpace(in.CustomerID) == "" {
		return ReleaseResult{}, errs.Required("customer_id")
	}
	sim := s.simNow(ctx, tenantID, in.CustomerID)
	res, err := s.held.ReleaseHeld(ctx, tenantID, ReleaseHeldInput{
		ExternalCustomerID: in.ExternalCustomerID,
		CustomerID:         in.CustomerID,
		MeterIDs:           in.MeterIDs,
		NotAfter:           sim.At,
	}, s.releaseEmission(clock.WithSim(ctx, sim), in))
	if err != nil {
		return ReleaseResult{}, err
	}
	out := ReleaseResult{CustomerID: in.CustomerID, StillHeld: res.StillHeld, MoreToRelease: res.MoreToRelease}
	var written []ReleasedUsage
	for _, r := range res.Rows {
		if r.Deduplicated {
			out.Deduplicated++
			continue
		}
		out.Released++
		written = append(written, ReleasedUsage{At: r.EventAt, MeterID: in.MeterIDs[r.MeterKey]})
	}
	if out.Released > 0 {
		out.InOpenPeriod, out.NotOnPlan, out.OutsideOpenPeriod = ClassifyByOpenPeriod(written, subs, plans)
	}
	return out, nil
}

// releaseEmission writes one audit row per release call, on the release
// transaction: a release changes what a customer is billed, like a
// backfill, so it is recorded.
func (s *Service) releaseEmission(ctx context.Context, in ReleaseInput) func(tx *sql.Tx, res ReleaseHeldResult) error {
	if s.audit == nil {
		return nil
	}
	return func(tx *sql.Tx, res ReleaseHeldResult) error {
		written, dedup := 0, 0
		var first, last time.Time
		for i, r := range res.Rows {
			if r.Deduplicated {
				dedup++
			} else {
				written++
			}
			if i == 0 || r.EventAt.Before(first) {
				first = r.EventAt
			}
			if i == 0 || r.EventAt.After(last) {
				last = r.EventAt
			}
		}
		return s.audit.LogInTx(ctx, tx, audit.Entry{
			Action:        domain.AuditActionCreate,
			ResourceType:  "usage_event",
			ResourceLabel: in.ExternalCustomerID,
			Metadata: map[string]any{
				"action":               "held_usage_released",
				"external_customer_id": in.ExternalCustomerID,
				"customer_id":          in.CustomerID,
				"released":             written,
				"deduplicated":         dedup,
				"first_event_at":       first.UTC().Format(time.RFC3339),
				"last_event_at":        last.UTC().Format(time.RFC3339),
				"origin":               string(domain.UsageOriginBackfill),
			},
		})
	}
}

// Discard marks the held rows for one external customer id (and one meter
// key, when given) discarded. They stay in the table as a record and are
// never ingested.
func (s *Service) Discard(ctx context.Context, tenantID, externalCustomerID, meterKey string) (int, error) {
	if s.held == nil {
		return 0, ErrHoldUnavailable
	}
	if strings.TrimSpace(externalCustomerID) == "" {
		return 0, errs.Required("external_customer_id")
	}
	var emit func(tx *sql.Tx, n int) error
	if s.audit != nil {
		emit = func(tx *sql.Tx, n int) error {
			return s.audit.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionDelete,
				ResourceType:  "usage_event",
				ResourceLabel: externalCustomerID,
				Metadata: map[string]any{
					"action":               "held_usage_discarded",
					"external_customer_id": externalCustomerID,
					"meter_key":            meterKey,
					"rows":                 n,
				},
			})
		}
	}
	return s.held.DiscardHeld(ctx, tenantID, externalCustomerID, meterKey, emit)
}

// ReleasedUsage is one released row, for ClassifyByOpenPeriod.
type ReleasedUsage struct {
	At      time.Time
	MeterID string
}

// ClassifyByOpenPeriod splits released rows three ways:
//
//   - inOpen: the time falls in an active or trialing subscription's
//     current billing period [start, end), and a plan on one of that
//     subscription's items prices the row's meter. That period's invoice
//     charges it.
//   - notOnPlan: the time falls in such a period, but no plan on the
//     subscription prices the meter. No invoice charges it.
//   - outside: the time falls in no current period. It reads only the
//     current period: Velox never re-opens a built invoice, so a time in an
//     earlier period is outside (ADR-120).
//
// plans maps plan id to plan; a plan missing from it prices nothing.
func ClassifyByOpenPeriod(rows []ReleasedUsage, subs []domain.Subscription, plans map[string]domain.Plan) (inOpen, notOnPlan, outside int) {
	for _, row := range rows {
		inPeriod, priced := false, false
		for _, sub := range subs {
			if sub.Status != domain.SubscriptionActive && sub.Status != domain.SubscriptionTrialing {
				continue
			}
			if sub.CurrentBillingPeriodStart == nil || sub.CurrentBillingPeriodEnd == nil {
				continue
			}
			if row.At.Before(*sub.CurrentBillingPeriodStart) || !row.At.Before(*sub.CurrentBillingPeriodEnd) {
				continue
			}
			inPeriod = true
			if subPricesMeter(sub, plans, row.MeterID) {
				priced = true
				break
			}
		}
		switch {
		case priced:
			inOpen++
		case inPeriod:
			notOnPlan++
		default:
			outside++
		}
	}
	return inOpen, notOnPlan, outside
}

func subPricesMeter(sub domain.Subscription, plans map[string]domain.Plan, meterID string) bool {
	for _, it := range sub.Items {
		for _, id := range plans[it.PlanID].MeterIDs {
			if id == meterID {
				return true
			}
		}
	}
	return false
}
