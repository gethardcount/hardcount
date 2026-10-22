package usage_test

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestBatchIngest_Atomic is the real-Postgres proof of the batch
// keyless-retry fix. Pre-fix, BatchIngest committed one tx PER EVENT: a
// mid-batch abort left a committed prefix, and the standard client
// response — retry the whole batch — re-ingested that prefix. Events
// without idempotency keys have no dedup line of defense, so the retry
// double-billed every prefix event.
//
// Only a real DB proves the three behaviors under test: (1) a DB-level
// failure mid-batch (FK violation) rolls back EVERYTHING already inserted
// in the same batch; (2) ON CONFLICT dedup counts replays as success
// without poisoning the transaction; (3) the shared insert path still
// stamps provider COGS (ADR-079) on batch rows.
func TestBatchIngest_Atomic(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(postgres.WithLivemode(context.Background(), false), 15*time.Second)
	defer cancel()

	tenantID := testutil.CreateTestTenant(t, db, "Batch Atomic")
	customerID := insertTestCustomer(t, db, tenantID, "cus_batch_atomic")
	meterID := insertTestMeter(t, db, tenantID, "mtr_batch_atomic", "tokens_batch")

	store := usage.NewPostgresStore(db)
	now := time.Now().UTC()
	evt := func(key string, qty int64) domain.UsageEvent {
		return domain.UsageEvent{
			CustomerID: customerID, MeterID: meterID,
			Quantity: decimal.NewFromInt(qty), IdempotencyKey: key,
			Timestamp: now, Origin: domain.UsageOriginAPI,
		}
	}
	countEvents := func() int {
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin count tx: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM usage_events`).Scan(&n); err != nil {
			t.Fatalf("count events: %v", err)
		}
		return n
	}

	// (1) All-or-nothing: row 2's meter FK violates AFTER rows 0-1 have
	// been inserted on the tx. Pre-fix rows 0-1 stayed committed — the
	// double-billing prefix. Now nothing may land.
	bad := evt("", 3)
	bad.MeterID = "mtr_does_not_exist"
	if _, _, err := store.IngestBatch(ctx, tenantID, []domain.UsageEvent{
		evt("", 1), evt("", 2), bad,
	}); err == nil {
		t.Fatal("batch with an FK-violating row must fail")
	}
	if got := countEvents(); got != 0 {
		t.Fatalf("failed batch left %d committed rows, want 0 (committed prefix = double-billing on retry)", got)
	}

	// (2) Dedup-as-success: seed one keyed event, then replay it in a
	// batch alongside a fresh key and an intra-batch duplicate pair.
	if _, err := store.Ingest(ctx, tenantID, evt("key-a", 10)); err != nil {
		t.Fatalf("seed keyed event: %v", err)
	}
	inserted, deduped, err := store.IngestBatch(ctx, tenantID, []domain.UsageEvent{
		evt("key-a", 10), // replay of the seeded row
		evt("key-b", 20), // fresh
		evt("key-c", 30), // fresh…
		evt("key-c", 30), // …and its intra-batch duplicate
	})
	if err != nil {
		t.Fatalf("dedup batch must succeed: %v", err)
	}
	if len(inserted) != 2 || inserted[0] != 1 || inserted[1] != 2 || deduped != 2 {
		t.Fatalf("got inserted=%v deduped=%d, want indices [1 2] and 2 deduplicated", inserted, deduped)
	}
	if got := countEvents(); got != 3 { // seed + key-b + key-c
		t.Fatalf("event count: got %d, want 3 (no duplicate rows)", got)
	}

	// Keyless events never dedup against each other — two identical
	// keyless rows are two distinct usage facts.
	inserted, deduped, err = store.IngestBatch(ctx, tenantID, []domain.UsageEvent{
		evt("", 5), evt("", 5),
	})
	if err != nil || len(inserted) != 2 || deduped != 0 {
		t.Fatalf("keyless batch: got inserted=%v deduped=%d err=%v, want 2/0/nil", inserted, deduped, err)
	}

	// (3) GetByIdempotencyKey returns the original row for
	// replay-as-success responses.
	original, err := store.GetByIdempotencyKey(ctx, tenantID, "key-b")
	if err != nil {
		t.Fatalf("get by idempotency key: %v", err)
	}
	if !original.Quantity.Equal(decimal.NewFromInt(20)) {
		t.Fatalf("original quantity: got %s, want 20", original.Quantity)
	}

	// (4) COGS stamp rides the shared insert on the batch path too: a
	// costed event (provider/model/token_type dims + a matching rate)
	// lands with provider_cost_micros set.
	rateTx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin rate tx: %v", err)
	}
	if _, err := rateTx.ExecContext(ctx, `
		INSERT INTO provider_cost_rates (id, tenant_id, livemode, provider, model, token_type, cost_per_token)
		VALUES ('vlx_pcr_batchtest', $1, false, 'anthropic', 'claude-x', 'input', 0.000002)`,
		tenantID); err != nil {
		t.Fatalf("seed cost rate: %v", err)
	}
	if err := rateTx.Commit(); err != nil {
		t.Fatalf("commit rate: %v", err)
	}
	costed := evt("key-cogs", 1000)
	costed.Dimensions = map[string]any{"provider": "anthropic", "model": "claude-x", "token_type": "input"}
	if _, _, err := store.IngestBatch(ctx, tenantID, []domain.UsageEvent{costed}); err != nil {
		t.Fatalf("costed batch: %v", err)
	}
	stamped, err := store.GetByIdempotencyKey(ctx, tenantID, "key-cogs")
	if err != nil {
		t.Fatalf("get costed event: %v", err)
	}
	if stamped.ProviderCostMicros == nil || *stamped.ProviderCostMicros != 2000 {
		t.Fatalf("provider_cost_micros: got %v, want 2000 (0.000002 × 1000 × 1e6)", stamped.ProviderCostMicros)
	}
}
