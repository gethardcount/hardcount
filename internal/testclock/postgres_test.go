package testclock_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestPostgresStore_Delete_HardDeletes_TearsDownPinnedSubs covers the teardown
// contract (ADR-086, Design B — supersedes ADR-016's soft-delete +
// cascade-cancel): deleting a clock HARD-deletes the clock row (gone from
// Get/List) and tears down every customer pinned to it together with their
// subscriptions — including one already in a terminal state, which the old
// model deliberately preserved. Nothing clock-scoped survives.
func TestPostgresStore_Delete_HardDeletes_TearsDownPinnedSubs(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := testclock.NewPostgresStore(db)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "Tenant")

	clk, err := store.Create(ctx, tenantID, domain.TestClock{
		Name:       "trial",
		FrozenTime: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}, noAuditRow)
	if err != nil {
		t.Fatalf("create clock: %v", err)
	}

	// A customer pinned to the clock (the ADR-027 customer-level pin) with two
	// subs on it: one active, one already canceled (terminal). The teardown keys
	// on the customer's pin, so it reaches every row the customer owns — both
	// subs go, regardless of state.
	customerID := insertCustomer(t, db, tenantID)
	pinCustomerToClock(t, db, customerID, clk.ID)
	insertSub(t, db, "sub_active", tenantID, customerID, clk.ID, "active")
	insertSub(t, db, "sub_canceled", tenantID, customerID, clk.ID, "canceled")

	if err := store.Delete(ctx, tenantID, clk.ID, noAuditRow); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The clock is hard-deleted: gone from Get and List.
	if _, err := store.Get(ctx, tenantID, clk.ID); err != errs.ErrNotFound {
		t.Errorf("Get after delete: expected ErrNotFound, got %v", err)
	}
	clocks, _, err := store.List(ctx, tenantID, 100, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, c := range clocks {
		if c.ID == clk.ID {
			t.Errorf("List returned deleted clock %s", c.ID)
		}
	}

	// The pinned customer and BOTH subs are torn down — including the canceled
	// one the old soft-delete model left in place.
	if rowExists(t, db, "customers", "id", customerID) {
		t.Error("pinned customer must be torn down")
	}
	if rowExists(t, db, "subscriptions", "code", "sub_active") {
		t.Error("active subscription must be torn down")
	}
	if rowExists(t, db, "subscriptions", "code", "sub_canceled") {
		t.Error("canceled (terminal) subscription must be torn down too")
	}

	// Idempotent: re-deleting a gone clock returns ErrNotFound.
	if err := store.Delete(ctx, tenantID, clk.ID, noAuditRow); err != errs.ErrNotFound {
		t.Errorf("repeat Delete: expected ErrNotFound, got %v", err)
	}
}

// TestPostgresStore_AdvanceSummary_RoundTrips locks the last_advance_summary
// JSONB column + the scan adapter: a never-advanced clock reads back a nil
// summary; MarkAdvancing stores advanced_from; CompleteAdvance stores the
// summary in its own statement, its RETURNING carries it, and the next Get
// decodes the stored counts and span exactly.
func TestPostgresStore_AdvanceSummary_RoundTrips(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := testclock.NewPostgresStore(db)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleTestClockCatchup)
	tenantID := testutil.CreateTestTenant(t, db, "Tenant")

	clk, err := store.Create(ctx, tenantID, domain.TestClock{
		Name:       "summary",
		FrozenTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, noAuditRow)
	if err != nil {
		t.Fatalf("create clock: %v", err)
	}
	// A fresh clock has never been advanced → nil summary.
	if clk.LastAdvanceSummary != nil {
		t.Fatalf("fresh clock should have nil summary, got %+v", clk.LastAdvanceSummary)
	}

	to := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	marked, err := store.MarkAdvancing(ctx, tenantID, clk.ID, to, noAuditRow)
	if err != nil {
		t.Fatalf("mark advancing: %v", err)
	}
	// MarkAdvancing restarts the summary with where the advance starts from,
	// read off the row before it moves frozen_time.
	if marked.LastAdvanceSummary == nil || !marked.LastAdvanceSummary.AdvancedFrom.Equal(clk.FrozenTime) {
		t.Fatalf("summary after MarkAdvancing: %+v, want advanced_from %s", marked.LastAdvanceSummary, clk.FrozenTime)
	}

	want := domain.AdvanceSummary{
		AdvancedFrom:      time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		AdvancedTo:        to,
		InvoicesGenerated: 2,
		DunningAdvanced:   1,
		CreditsExpired:    3,
	}
	// The completing transition stores the summary, and its RETURNING reads
	// it back.
	done, err := store.CompleteAdvance(ctx, tenantID, clk.ID, want)
	if err != nil {
		t.Fatalf("complete advance: %v", err)
	}
	if done.LastAdvanceSummary == nil {
		t.Fatal("CompleteAdvance did not return the persisted summary")
	}
	got := *done.LastAdvanceSummary
	if got.InvoicesGenerated != 2 || got.DunningAdvanced != 1 || got.CreditsExpired != 3 {
		t.Errorf("counts: got inv=%d dun=%d cred=%d, want 2/1/3",
			got.InvoicesGenerated, got.DunningAdvanced, got.CreditsExpired)
	}
	if !got.AdvancedTo.Equal(to) {
		t.Errorf("advanced_to: got %s, want %s", got.AdvancedTo, to)
	}

	// And a plain Get decodes it too.
	reloaded, err := store.Get(ctx, tenantID, clk.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if reloaded.LastAdvanceSummary == nil || reloaded.LastAdvanceSummary.InvoicesGenerated != 2 {
		t.Errorf("Get did not decode the summary: %+v", reloaded.LastAdvanceSummary)
	}
}

// TestPostgresStore_MarkFailed_ReasonCutInsideACharacter: a catch-up fails
// with a reason over 500 bytes whose cut lands inside a character. The store
// keeps the bytes before that character and adds "…". Postgres refuses text
// that is not valid UTF-8 (SQLSTATE 22021). A cut that split the character
// would fail the write and leave the clock advancing, where neither Advance
// (it needs ready) nor Retry advance (it needs internal_failure) applies.
func TestPostgresStore_MarkFailed_ReasonCutInsideACharacter(t *testing.T) {
	db := testutil.SetupTestDB(t)
	store := testclock.NewPostgresStore(db)
	ctx := leadertest.Token(t, testutil.AdminPool(t), postgres.WithLivemode(context.Background(), false), leader.RoleTestClockCatchup)
	tenantID := testutil.CreateTestTenant(t, db, "Tenant")

	clk, err := store.Create(ctx, tenantID, domain.TestClock{
		Name:       "long reason",
		FrozenTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, noAuditRow)
	if err != nil {
		t.Fatalf("create clock: %v", err)
	}
	if _, err := store.MarkAdvancing(ctx, tenantID, clk.ID, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), noAuditRow); err != nil {
		t.Fatalf("mark advancing: %v", err)
	}

	// "é" takes bytes 498 and 499, so the 499-byte cut lands inside it.
	kept := strings.Repeat("a", 498)
	got, err := store.MarkFailed(ctx, tenantID, clk.ID, kept+"é"+strings.Repeat("b", 100), domain.AdvanceSummary{HadErrors: true})
	if err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if got.Status != domain.TestClockStatusInternalFailed {
		t.Errorf("status = %q, want %q", got.Status, domain.TestClockStatusInternalFailed)
	}
	if want := kept + "…"; got.LastFailureReason != want {
		t.Errorf("last_failure_reason = %q, want the first 498 bytes, which end before the split character, and …", got.LastFailureReason)
	}
}

// noAuditRow stands in for the audit row in tests of the store alone. The
// service builds the real one; internal/api's test-clock audit tests pin it.
func noAuditRow(*sql.Tx, domain.TestClock) error { return nil }

// Helpers — minimal raw inserts so the test doesn't pull the
// subscription package's full Create surface (plans, items,
// currency) which the teardown behavior doesn't care about.
// Use TxBypass so the RLS predicate doesn't block fixture setup.

func rowExists(t *testing.T, db *postgres.DB, table, col, val string) bool {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	var ok bool
	if err := tx.QueryRowContext(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE `+col+` = $1)`, val).Scan(&ok); err != nil {
		t.Fatalf("exists %s.%s: %v", table, col, err)
	}
	return ok
}

func pinCustomerToClock(t *testing.T, db *postgres.DB, customerID, clockID string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(context.Background(),
		`SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("set livemode: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE customers SET test_clock_id = $2 WHERE id = $1`, customerID, clockID); err != nil {
		t.Fatalf("pin customer: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func insertCustomer(t *testing.T, db *postgres.DB, tenantID string) string {
	t.Helper()
	id := "cus_test_" + randHex(8)
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(context.Background(),
		`SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("set livemode: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(), `
		INSERT INTO customers (id, tenant_id, external_id, display_name, livemode)
		VALUES ($1, $2, $1, 'test', false)
	`, id, tenantID); err != nil {
		t.Fatalf("insert customer: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return id
}

func insertSub(t *testing.T, db *postgres.DB, code, tenantID, customerID, clockID, status string) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	// Some BEFORE-INSERT triggers (api_keys, possibly subscriptions
	// via livemode propagation) read app.livemode and overwrite the
	// row's livemode column from it. TxBypass doesn't set this. Pin
	// it to test mode explicitly so the trigger doesn't reject the
	// row with the test_clock_requires_testmode CHECK.
	if _, err := tx.ExecContext(context.Background(),
		`SELECT set_config('app.livemode', 'off', true)`); err != nil {
		t.Fatalf("set livemode: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(), `
		INSERT INTO subscriptions (id, tenant_id, code, display_name, customer_id, status, livemode, test_clock_id)
		VALUES ($1, $2, $3, $3, $4, $5, false, $6)
	`, "sub_test_"+code, tenantID, code, customerID, status, clockID); err != nil {
		t.Fatalf("insert sub: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
