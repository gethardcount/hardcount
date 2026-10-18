package usage_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// Held usage (#1044, ADR-122) against real Postgres: a row whose customer
// does not exist is kept, and a release writes it to usage_events at most
// once, whatever order the release, a LiteLLM resend and a second release
// arrive in.

type heldFixture struct {
	db       *postgres.DB
	tenantID string
	ctx      context.Context
	svc      *usage.Service
	store    *usage.PostgresStore
}

func newHeldFixture(t *testing.T, name string) heldFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, name)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 60*time.Second)
	t.Cleanup(cancel)
	store := usage.NewPostgresStore(db)
	return heldFixture{db: db, tenantID: tenantID, ctx: ctx, svc: usage.NewService(store), store: store}
}

func (f heldFixture) hold(t *testing.T, ext, key string, at time.Time) error {
	t.Helper()
	_, err := f.svc.Hold(f.ctx, f.tenantID, usage.HoldInput{
		Source:             domain.HeldUsageSourceLiteLLM,
		ExternalCustomerID: ext,
		MeterKey:           "tokens",
		Quantity:           decimal.NewFromInt(100),
		Dimensions:         map[string]any{"model": "gpt-4o", "token_type": "input"},
		IdempotencyKey:     key,
		EventAt:            &at,
		Reason:             domain.HeldReasonCustomerNotFound,
	})
	return err
}

// countEvents counts usage_events rows with the given keys, in the
// fixture's mode, through RLS like every reader.
func (f heldFixture) countEvents(t *testing.T, keys ...string) int {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(f.ctx,
		`SELECT COUNT(*) FROM usage_events WHERE idempotency_key = ANY($1::text[])`,
		postgres.StringArray(keys)).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func (f heldFixture) heldStatus(t *testing.T, key string) (status string, eventID *string) {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := tx.QueryRowContext(f.ctx,
		`SELECT status, released_event_id FROM usage_held_events WHERE idempotency_key = $1`, key,
	).Scan(&status, &eventID); err != nil {
		t.Fatalf("held row %s: %v", key, err)
	}
	return status, eventID
}

func (f heldFixture) release(t *testing.T, ext, customerID, meterID string) usage.ReleaseResult {
	t.Helper()
	res, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
		ExternalCustomerID: ext,
		CustomerID:         customerID,
		MeterIDs:           map[string]string{"tokens": meterID},
	}, nil, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	return res
}

func TestHeld_HoldThenReleaseWritesExactlyOnce(t *testing.T) {
	f := newHeldFixture(t, "Held Once")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	if err := f.hold(t, "cus_late", "call-1:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if err := f.hold(t, "cus_late", "call-1:output", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	// A LiteLLM resend of a held row writes nothing.
	if err := f.hold(t, "cus_late", "call-1:input", at); !errors.Is(err, errs.ErrDuplicateKey) {
		t.Fatalf("re-hold: err %v, want ErrDuplicateKey", err)
	}

	summary, err := f.svc.ListHeld(f.ctx, f.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	groups := summary.Groups
	if summary.TotalRows != 2 || summary.TotalCustomers != 1 {
		t.Fatalf("totals = %d rows / %d customers, want 2 / 1", summary.TotalRows, summary.TotalCustomers)
	}
	if len(groups) != 1 || groups[0].Rows != 2 || groups[0].ExternalCustomerID != "cus_late" ||
		!groups[0].TotalQuantity.Equal(decimal.NewFromInt(200)) || !groups[0].FirstEventAt.Equal(at) {
		t.Fatalf("summary = %+v, want one group of 2 rows / 200 for cus_late", groups)
	}

	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_late")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")

	res := f.release(t, "cus_late", customerID, meterID)
	if res.Released != 2 || res.Deduplicated != 0 || res.StillHeld != 0 {
		t.Fatalf("release = %+v, want 2 released", res)
	}
	if n := f.countEvents(t, "call-1:input", "call-1:output"); n != 2 {
		t.Fatalf("usage_events rows = %d, want 2", n)
	}
	// The released event keeps the held row's time and is tagged backfill.
	evt, err := f.svc.GetByIdempotencyKey(f.ctx, f.tenantID, "call-1:input")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !evt.Timestamp.Equal(at) || evt.Origin != domain.UsageOriginBackfill || evt.CustomerID != customerID {
		t.Fatalf("released event = %+v, want ts %v, origin backfill, customer %s", evt, at, customerID)
	}
	if status, id := f.heldStatus(t, "call-1:input"); status != "released" || id == nil || *id != evt.ID {
		t.Fatalf("held row = %s/%v, want released with event id %s", status, id, evt.ID)
	}

	// A second release finds nothing held and writes nothing.
	again := f.release(t, "cus_late", customerID, meterID)
	if again.Released != 0 || again.Deduplicated != 0 {
		t.Fatalf("second release = %+v, want nothing", again)
	}
	if n := f.countEvents(t, "call-1:input", "call-1:output"); n != 2 {
		t.Fatalf("usage_events rows after second release = %d, want 2", n)
	}
	if summary, _ := f.svc.ListHeld(f.ctx, f.tenantID); len(summary.Groups) != 0 || summary.TotalRows != 0 {
		t.Fatalf("summary after release = %+v, want empty", summary)
	}
}

// LiteLLM resends the call after the customer exists: the resend goes
// straight to usage_events under the same key. The later release must
// not write it again — usage_events' ON CONFLICT absorbs it and the held
// row is marked released with no event id.
// Mutation-verify: drop ON CONFLICT from ingestOneTx → the release fails
// with a unique violation (and the batch paths with it).
func TestHeld_ResendBeforeReleaseIsNotBilledTwice(t *testing.T) {
	f := newHeldFixture(t, "Held Resend")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := f.hold(t, "cus_resend", "call-2:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_resend")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")

	if _, err := f.svc.Ingest(f.ctx, f.tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(100),
		IdempotencyKey: "call-2:input", Timestamp: &at,
	}); err != nil {
		t.Fatalf("resend ingest: %v", err)
	}

	res := f.release(t, "cus_resend", customerID, meterID)
	if res.Released != 0 || res.Deduplicated != 1 {
		t.Fatalf("release = %+v, want 0 released / 1 deduplicated", res)
	}
	if n := f.countEvents(t, "call-2:input"); n != 1 {
		t.Fatalf("usage_events rows = %d, want 1", n)
	}
	if status, id := f.heldStatus(t, "call-2:input"); status != "released" || id != nil {
		t.Fatalf("held row = %s/%v, want released with no event id", status, id)
	}
}

// Two releases at once claim disjoint rows: every held row lands exactly
// once. Mutation-verify: drop the outer "AND status = 'held'" from the
// claiming UPDATE in ReleaseHeld → the second release, whose subquery read
// the rows as held before the first committed, re-claims them; its inserts
// come back deduplicated and this test fails on deduplicated != 0.
func TestHeld_ConcurrentReleasesWriteOnce(t *testing.T) {
	f := newHeldFixture(t, "Held Concurrent")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	const n = 40
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("conc-%d:input", i)
		if err := f.hold(t, "cus_conc", keys[i], at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_conc")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")

	var wg sync.WaitGroup
	results := make([]usage.ReleaseResult, 2)
	errsOut := make([]error, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errsOut[i] = f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
				ExternalCustomerID: "cus_conc", CustomerID: customerID,
				MeterIDs: map[string]string{"tokens": meterID},
			}, nil, nil)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errsOut {
		if err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	if got := results[0].Released + results[1].Released; got != n {
		t.Fatalf("released %d + %d = %d, want %d", results[0].Released, results[1].Released, got, n)
	}
	if d := results[0].Deduplicated + results[1].Deduplicated; d != 0 {
		t.Fatalf("deduplicated %d, want 0: the two releases claimed the same rows", d)
	}
	if c := f.countEvents(t, keys...); c != n {
		t.Fatalf("usage_events rows = %d, want %d", c, n)
	}
}

// Held rows live in one tenant and one mode, like usage_events.
func TestHeld_TenantAndModeIsolation(t *testing.T) {
	f := newHeldFixture(t, "Held Isolation")
	at := time.Now().UTC().Add(-time.Hour)
	if err := f.hold(t, "cus_iso", "iso-1:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}

	live := postgres.WithLivemode(context.Background(), true)
	if summary, err := f.svc.ListHeld(live, f.tenantID); err != nil || len(summary.Groups) != 0 || summary.TotalRows != 0 {
		t.Fatalf("live mode sees test-mode held rows: %+v (%v)", summary, err)
	}
	other := testutil.CreateTestTenant(t, f.db, "Held Other Tenant")
	if summary, err := f.svc.ListHeld(f.ctx, other); err != nil || len(summary.Groups) != 0 || summary.TotalRows != 0 {
		t.Fatalf("another tenant sees held rows: %+v (%v)", summary, err)
	}
	// The same key may be held in another tenant: keys are per tenant.
	if _, err := f.svc.Hold(f.ctx, other, usage.HoldInput{
		ExternalCustomerID: "cus_iso", MeterKey: "tokens", Quantity: decimal.NewFromInt(1),
		IdempotencyKey: "iso-1:input", EventAt: &at, Reason: domain.HeldReasonCustomerNotFound,
	}); err != nil {
		t.Fatalf("hold same key in another tenant: %v", err)
	}
}

// A row stamped after the customer's current time stays held: releasing it
// would put usage in the customer's future (a test-clock customer frozen
// in the past).
func TestHeld_RowsAfterNowStayHeld(t *testing.T) {
	f := newHeldFixture(t, "Held Future")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := f.hold(t, "cus_clock", "clock-1:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_clock")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")

	res, err := f.store.ReleaseHeld(f.ctx, f.tenantID, usage.ReleaseHeldInput{
		ExternalCustomerID: "cus_clock", CustomerID: customerID,
		MeterIDs: map[string]string{"tokens": meterID},
		NotAfter: at.Add(-24 * time.Hour),
	}, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(res.Rows) != 0 || res.StillHeld != 1 {
		t.Fatalf("release = %+v, want nothing released and 1 still held", res)
	}
	if n := f.countEvents(t, "clock-1:input"); n != 0 {
		t.Fatalf("usage_events rows = %d, want 0", n)
	}
}

// Rows whose meter does not exist are not claimed: they stay held.
func TestHeld_MissingMeterStaysHeld(t *testing.T) {
	f := newHeldFixture(t, "Held Meter")
	at := time.Now().UTC().Add(-time.Hour)
	if err := f.hold(t, "cus_meter", "meter-1:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_meter")
	other := insertTestMeter(t, f.db, f.tenantID, "Other", "other_key")
	res, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
		ExternalCustomerID: "cus_meter", CustomerID: customerID,
		MeterIDs: map[string]string{"other_key": other},
	}, nil, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Released != 0 || res.StillHeld != 1 {
		t.Fatalf("release = %+v, want 0 released / 1 still held", res)
	}
}

// Discard keeps the row as a record and takes it out of every later
// release.
func TestHeld_DiscardIsFinal(t *testing.T) {
	f := newHeldFixture(t, "Held Discard")
	at := time.Now().UTC().Add(-time.Hour)
	if err := f.hold(t, "cus_typo", "typo-1:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	n, err := f.svc.Discard(f.ctx, f.tenantID, "cus_typo", "")
	if err != nil || n != 1 {
		t.Fatalf("discard = %d (%v), want 1", n, err)
	}
	if status, _ := f.heldStatus(t, "typo-1:input"); status != "discarded" {
		t.Fatalf("status = %s, want discarded", status)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_typo")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")
	if res := f.release(t, "cus_typo", customerID, meterID); res.Released != 0 {
		t.Fatalf("release after discard = %+v, want nothing", res)
	}
	if n := f.countEvents(t, "typo-1:input"); n != 0 {
		t.Fatalf("usage_events rows = %d, want 0", n)
	}
}

// A release is an operator action on the money path: it writes one audit
// row on its own transaction, and an audit failure rolls the release back
// (the rows stay held).
func TestHeld_ReleaseIsAudited(t *testing.T) {
	f := newHeldFixture(t, "Held Audit")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := f.hold(t, "cus_audit", "audit-1:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_audit")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")

	f.svc.SetAuditLogger(failingProviderCostEmitter{})
	if _, err := f.svc.Release(f.ctx, f.tenantID, usage.ReleaseInput{
		ExternalCustomerID: "cus_audit", CustomerID: customerID,
		MeterIDs: map[string]string{"tokens": meterID},
	}, nil, nil); err == nil {
		t.Fatal("release with a failing audit emitter succeeded")
	}
	if status, _ := f.heldStatus(t, "audit-1:input"); status != "held" {
		t.Fatalf("status after failed release = %s, want held", status)
	}
	if n := f.countEvents(t, "audit-1:input"); n != 0 {
		t.Fatalf("usage_events rows after failed release = %d, want 0", n)
	}

	logger := audit.NewLogger(f.db)
	f.svc.SetAuditLogger(logger)
	if res := f.release(t, "cus_audit", customerID, meterID); res.Released != 1 {
		t.Fatalf("release = %+v, want 1", res)
	}
	rows, _, err := logger.Query(f.ctx, f.tenantID, audit.QueryFilter{ResourceType: "usage_event"})
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	if len(rows) != 1 || rows[0].Metadata["action"] != "held_usage_released" || rows[0].Metadata["customer_id"] != customerID {
		t.Fatalf("audit rows = %+v, want one held_usage_released row", rows)
	}
}

// Review finding (#1151): a discarded row stays unbilled when LiteLLM
// resends it after the customer exists. Before the fix the resend found the
// customer, skipped the held table and wrote the row to usage_events.
// Mutation-verify: drop the discarded check in
// PostgresStore.IngestResolvingHeld → the resend is written and this fails
// on the usage_events count.
func TestHeld_ResendAfterDiscardIsNotBilled(t *testing.T) {
	f := newHeldFixture(t, "Held Discard Resend")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := f.hold(t, "cus_gone", "gone-1:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	if n, err := f.svc.Discard(f.ctx, f.tenantID, "cus_gone", ""); err != nil || n != 1 {
		t.Fatalf("discard = %d (%v), want 1", n, err)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_gone")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")

	_, err := f.svc.IngestResolvingHeld(f.ctx, f.tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(100),
		IdempotencyKey: "gone-1:input", Timestamp: &at,
	})
	if !errors.Is(err, errs.ErrDuplicateKey) {
		t.Fatalf("resend after discard: err %v, want ErrDuplicateKey", err)
	}
	if n := f.countEvents(t, "gone-1:input"); n != 0 {
		t.Fatalf("usage_events rows = %d, want 0: a discarded row was billed", n)
	}
	if status, _ := f.heldStatus(t, "gone-1:input"); status != "discarded" {
		t.Fatalf("status = %s, want discarded", status)
	}
}

// A LiteLLM resend after the customer exists writes the row and marks its
// held row released, in one transaction. A later discard then finds nothing
// to discard, so the held table never says "discarded" for usage that is in
// usage_events, and a later release writes nothing.
func TestHeld_ResendWhileHeldResolvesTheHeldRow(t *testing.T) {
	f := newHeldFixture(t, "Held Resend Resolves")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := f.hold(t, "cus_back", "back-1:input", at); err != nil {
		t.Fatalf("hold: %v", err)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_back")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")

	evt, err := f.svc.IngestResolvingHeld(f.ctx, f.tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(100),
		IdempotencyKey: "back-1:input", Timestamp: &at,
	})
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if status, id := f.heldStatus(t, "back-1:input"); status != "released" || id == nil || *id != evt.ID {
		t.Fatalf("held row = %s/%v, want released with event id %s", status, id, evt.ID)
	}
	if n, err := f.svc.Discard(f.ctx, f.tenantID, "cus_back", ""); err != nil || n != 0 {
		t.Fatalf("discard after resend = %d (%v), want 0", n, err)
	}
	if res := f.release(t, "cus_back", customerID, meterID); res.Released != 0 || res.Deduplicated != 0 {
		t.Fatalf("release after resend = %+v, want nothing", res)
	}
	if n := f.countEvents(t, "back-1:input"); n != 1 {
		t.Fatalf("usage_events rows = %d, want 1", n)
	}
	// A second resend is a plain duplicate.
	if _, err := f.svc.IngestResolvingHeld(f.ctx, f.tenantID, usage.IngestInput{
		CustomerID: customerID, MeterID: meterID, Quantity: decimal.NewFromInt(100),
		IdempotencyKey: "back-1:input", Timestamp: &at,
	}); !errors.Is(err, errs.ErrDuplicateKey) {
		t.Fatalf("second resend: err %v, want ErrDuplicateKey", err)
	}
}

// Review finding (#1151): rows left only because one release stops at its
// limit are reported as more_to_release, not as still_held (which says a
// meter is missing or the time is in the customer's future).
func TestHeld_ReleaseLimitIsReportedApart(t *testing.T) {
	f := newHeldFixture(t, "Held Limit")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for i := 0; i < 3; i++ {
		if err := f.hold(t, "cus_many", fmt.Sprintf("many-%d:input", i), at.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
	}
	// One row whose meter does not exist: still held, for a real reason.
	if _, err := f.svc.Hold(f.ctx, f.tenantID, usage.HoldInput{
		ExternalCustomerID: "cus_many", MeterKey: "no_such_meter", Quantity: decimal.NewFromInt(1),
		IdempotencyKey: "many-x:input", EventAt: &at, Reason: domain.HeldReasonMeterNotFound,
	}); err != nil {
		t.Fatalf("hold missing meter: %v", err)
	}
	customerID := insertTestCustomer(t, f.db, f.tenantID, "cus_many")
	meterID := insertTestMeter(t, f.db, f.tenantID, "Tokens", "tokens")
	in := usage.ReleaseHeldInput{
		ExternalCustomerID: "cus_many", CustomerID: customerID,
		MeterIDs: map[string]string{"tokens": meterID},
		NotAfter: time.Now().UTC(), Limit: 2,
	}
	res, err := f.store.ReleaseHeld(f.ctx, f.tenantID, in, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(res.Rows) != 2 || res.MoreToRelease != 1 || res.StillHeld != 1 {
		t.Fatalf("release = %d rows / more %d / still %d, want 2 / 1 / 1", len(res.Rows), res.MoreToRelease, res.StillHeld)
	}
	res, err = f.store.ReleaseHeld(f.ctx, f.tenantID, in, nil)
	if err != nil {
		t.Fatalf("second release: %v", err)
	}
	if len(res.Rows) != 1 || res.MoreToRelease != 0 || res.StillHeld != 1 {
		t.Fatalf("second release = %d rows / more %d / still %d, want 1 / 0 / 1", len(res.Rows), res.MoreToRelease, res.StillHeld)
	}
}

// Review finding (#1151): the held list is cut at HeldListCustomers
// external ids, and its totals still count every held row, so the
// dashboard can say the list is cut instead of undercounting. A listed id
// carries all its groups.
func TestHeld_ListTotalsCountPastTheCap(t *testing.T) {
	f := newHeldFixture(t, "Held List Cap")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	n := usage.HeldListCustomers + 2
	for i := 0; i < n; i++ {
		if err := f.hold(t, fmt.Sprintf("cus_cap_%03d", i), fmt.Sprintf("cap-%d:input", i), at); err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
	}
	// The newest id has two meters: both groups are listed.
	if _, err := f.svc.Hold(f.ctx, f.tenantID, usage.HoldInput{
		ExternalCustomerID: fmt.Sprintf("cus_cap_%03d", n-1), MeterKey: "other", Quantity: decimal.NewFromInt(1),
		IdempotencyKey: "cap-extra:input", EventAt: &at, Reason: domain.HeldReasonCustomerNotFound,
	}); err != nil {
		t.Fatalf("hold extra: %v", err)
	}
	summary, err := f.svc.ListHeld(f.ctx, f.tenantID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	listed := map[string]int{}
	for _, g := range summary.Groups {
		listed[g.ExternalCustomerID]++
	}
	if len(listed) != usage.HeldListCustomers {
		t.Fatalf("listed %d customers, want %d", len(listed), usage.HeldListCustomers)
	}
	if summary.TotalCustomers != n || summary.TotalRows != n+1 {
		t.Fatalf("totals = %d customers / %d rows, want %d / %d", summary.TotalCustomers, summary.TotalRows, n, n+1)
	}
	if got := listed[fmt.Sprintf("cus_cap_%03d", n-1)]; got != 2 {
		t.Fatalf("newest customer has %d groups listed, want 2", got)
	}
}
