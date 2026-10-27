package api

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// Every ingest door reads its meter's aggregation on its own transaction's
// connection, through the production OpenPeriodReader and with the audit
// logger wired as the router wires it. Read on a second connection while the
// ingest's transaction holds one, a pool of one connection (or N ingests on
// a pool of N) waits for a connection that is never freed.
//
// Mutation-verify: read the aggregation through pricing GetMeter (its own
// transaction) after the insert → each door fails at its deadline with
// nothing written.
func TestIngest_ReadsTheAggregationOnItsOwnConnection(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Ingest one connection")
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	defer cancel()

	usageSvc := usage.NewService(usage.NewPostgresStore(db))
	usageSvc.SetAuditLogger(audit.NewLogger(db))
	pricingStore := pricing.NewPostgresStore(db)
	const ext, eventName = "ext_ingest_one_conn", "tokens_ingest_one_conn"
	now := time.Now().UTC()
	at := now.Add(-time.Hour).Truncate(time.Second)
	for _, key := range []string{"one-conn-call:input"} {
		if _, err := usageSvc.Hold(ctx, tenantID, usage.HoldInput{
			Source: domain.HeldUsageSourceLiteLLM, ExternalCustomerID: ext, EventName: eventName,
			Quantity: decimal.NewFromInt(5), IdempotencyKey: key, EventAt: &at,
			Reason: domain.HeldReasonCustomerNotFound,
		}); err != nil {
			t.Fatalf("hold %s: %v", key, err)
		}
	}
	meter, err := pricing.NewService(pricingStore).CreateMeter(ctx, tenantID, pricing.CreateMeterInput{
		EventName: eventName, Name: eventName, Unit: "tokens", Aggregation: "sum",
	})
	if err != nil {
		t.Fatalf("create meter: %v", err)
	}
	cus, err := customer.NewPostgresStore(db).Create(ctx, tenantID, domain.Customer{DisplayName: "One conn", ExternalID: ext})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	usageSvc.SetOpenPeriodReader(usageOpenPeriods{
		subs:       releaseSubs{meterID: meter.ID, start: now.Add(-2 * time.Hour), end: now.Add(24 * time.Hour)},
		pricing:    releasePricing{PostgresStore: pricingStore, meterID: meter.ID},
		thresholds: releaseThresholds{at: now.Add(-30 * time.Minute)},
	})
	in := func(key string) usage.IngestInput {
		return usage.IngestInput{CustomerID: cus.ID, MeterID: meter.ID, Quantity: decimal.NewFromInt(5), IdempotencyKey: key, Timestamp: &at}
	}

	db.Pool.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Pool.SetMaxOpenConns(5) })
	// sum is additive, and every row is before the paid threshold invoice's
	// end: the close does not count it, so each answer says after_close.
	for _, door := range []struct {
		name string
		run  func(ctx context.Context) (flagged, want int, err error)
	}{
		{"single", func(ctx context.Context) (int, int, error) {
			ev, err := usageSvc.Ingest(ctx, tenantID, in("one-conn-single"))
			return statusCount(ev), 1, err
		}},
		{"batch", func(ctx context.Context) (int, int, error) {
			res, errs := usageSvc.BatchIngest(ctx, tenantID, []usage.IngestInput{in("one-conn-batch-1"), in("one-conn-batch-2")})
			if len(errs) > 0 {
				return 0, 2, errs[0]
			}
			return res.Unbilled.AfterClose, 2, nil
		}},
		{"backfill", func(ctx context.Context) (int, int, error) {
			ev, err := usageSvc.Backfill(ctx, tenantID, in("one-conn-backfill"))
			return statusCount(ev), 1, err
		}},
		{"litellm call, resend of a held key", func(ctx context.Context) (int, int, error) {
			res, err := usageSvc.IngestCall(ctx, tenantID, usage.CallInput{CustomerID: cus.ID, MeterID: meter.ID, Events: []usage.CallEvent{
				{Quantity: decimal.NewFromInt(5), IdempotencyKey: "one-conn-call:input", Timestamp: &at},
				{Quantity: decimal.NewFromInt(7), IdempotencyKey: "one-conn-call:output", Timestamp: &at},
			}})
			return res.Unbilled.AfterClose, 2, err
		}},
	} {
		t.Run(door.name, func(t *testing.T) {
			dctx, dcancel := context.WithTimeout(ctx, 10*time.Second)
			defer dcancel()
			flagged, want, err := door.run(dctx)
			if err != nil {
				t.Fatalf("ingest on a pool of one connection: %v", err)
			}
			if flagged != want {
				t.Errorf("after_close = %d, want %d", flagged, want)
			}
		})
	}
}

func statusCount(ev domain.UsageEvent) int {
	if ev.BillingStatus == usage.BillingStatusAfterClose {
		return 1
	}
	return 0
}
