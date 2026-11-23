package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/usage"
)

// onClock answers every customer as on test clock clk_usage at `at`, and
// counts the reads.
type onClock struct {
	at    time.Time
	reads int
}

func (o *onClock) SimForCustomer(context.Context, string, string) (clock.Sim, error) {
	o.reads++
	return clock.Sim{At: o.at, TestClockID: "clk_usage"}, nil
}
func (o *onClock) SimForSubscription(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: o.at, TestClockID: "clk_usage"}, nil
}
func (o *onClock) SimForInvoice(context.Context, string, string) (clock.Sim, error) {
	return clock.Sim{At: o.at, TestClockID: "clk_usage"}, nil
}

// A release for a customer on a test clock is bounded by the clock's time,
// not real time: a held row dated after the clock's time, though before real
// now, stays held; one dated before it is released.
func TestHeld_ReleaseForAClockCustomerIsBoundedByTheClock(t *testing.T) {
	f := newHeldFixture(t, "Held Clock Bound")
	clockAt := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second)
	if err := f.hold(t, "cus_on_clock", "clk-before:input", clockAt.Add(-time.Hour)); err != nil {
		t.Fatalf("hold before the clock: %v", err)
	}
	if err := f.hold(t, "cus_on_clock", "clk-after:input", clockAt.Add(time.Hour)); err != nil {
		t.Fatalf("hold after the clock: %v", err)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_on_clock")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")
	f.svc.SetResolver(&onClock{at: clockAt})

	res := f.release(t, "cus_on_clock", customerID, meterID)
	if res.Released != 1 || res.StillHeld != 1 {
		t.Fatalf("release = %+v; want 1 released (before the clock) and 1 still held (after it)", res)
	}
	if before, after := f.countEvents(t, "clk-before:input"), f.countEvents(t, "clk-after:input"); before != 1 || after != 0 {
		t.Errorf("usage events: before the clock %d, after it %d; want 1 and 0", before, after)
	}
}

// A LiteLLM call for a customer on a test clock decides its time once for
// the whole call: one resolver read, and every event of the call at the
// clock's time, whatever time the proxy sent. The call writes to the cache,
// so it also carries the pre-split mapping, which is built too.
func TestIngestCall_ForAClockCustomerDecidesOnce(t *testing.T) {
	f := newHeldFixture(t, "Call Clock Once")
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_call_clock")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")
	clockAt := time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Second)
	res := &onClock{at: clockAt}
	f.svc.SetResolver(res)
	proxyEnd := time.Now().UTC() // the proxy's real end time, past the clock's

	if _, err := f.svc.IngestCall(f.ctx, f.tenantID, usage.CallInput{
		CustomerID: customerID, MeterID: meterID,
		Events: []usage.CallEvent{
			{Quantity: decimal.NewFromInt(100), IdempotencyKey: "once-1:input"},
			{Quantity: decimal.NewFromInt(40), IdempotencyKey: "once-1:cache_write_5m"},
			{Quantity: decimal.NewFromInt(50), IdempotencyKey: "once-1:output", Timestamp: &proxyEnd},
		},
		Legacy: []usage.CallEvent{
			{Quantity: decimal.NewFromInt(140), IdempotencyKey: "once-1:input", Timestamp: &proxyEnd},
			{Quantity: decimal.NewFromInt(50), IdempotencyKey: "once-1:output", Timestamp: &proxyEnd},
		},
		LegacyKey: "once-1:input",
	}); err != nil {
		t.Fatalf("ingest call: %v", err)
	}
	if res.reads != 1 {
		t.Errorf("%d clock reads for one call; want 1", res.reads)
	}
	for _, key := range []string{"once-1:input", "once-1:cache_write_5m", "once-1:output"} {
		if at := eventTime(t, f, key); !at.Equal(clockAt) {
			t.Errorf("%s at %v; want the clock's %v", key, at, clockAt)
		}
	}
}

// eventTime is the timestamp of the usage event with this key.
func eventTime(t *testing.T, f heldFixture, key string) time.Time {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var at time.Time
	if err := tx.QueryRowContext(f.ctx, `SELECT timestamp FROM usage_events WHERE idempotency_key = $1`, key).Scan(&at); err != nil {
		t.Fatalf("event %s: %v", key, err)
	}
	return at.UTC()
}
