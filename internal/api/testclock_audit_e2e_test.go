package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// A test clock's create, advance, retry advance and delete each write one
// audit row: after a delete tears the simulation down (ADR-086), these rows
// are its only record. These tests drive the real server over HTTP.

// clockAuditRow is one test_clock row of the audit log, as stored.
type clockAuditRow struct {
	Action      string
	ResourceID  string
	Label       string
	Metadata    map[string]any
	SimAt       time.Time
	TestClockID string
}

// clockAuditRows reads the tenant's test_clock audit rows, oldest first.
func clockAuditRows(t *testing.T, db *postgres.DB, tenantID string) []clockAuditRow {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(t.Context(), `
		SELECT action, resource_id, resource_label, metadata, sim_effective_at, test_clock_id
		FROM audit_log WHERE tenant_id = $1 AND resource_type = 'test_clock'
		ORDER BY created_at, id`, tenantID)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []clockAuditRow
	for rows.Next() {
		var (
			r       clockAuditRow
			label   sql.NullString
			meta    []byte
			simAt   sql.NullTime
			clockID sql.NullString
		)
		if err := rows.Scan(&r.Action, &r.ResourceID, &label, &meta, &simAt, &clockID); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		if err := json.Unmarshal(meta, &r.Metadata); err != nil {
			t.Fatalf("decode audit metadata %s: %v", meta, err)
		}
		r.Label, r.SimAt, r.TestClockID = label.String, simAt.Time, clockID.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	return out
}

// execBypass runs one statement on its own committed transaction.
func execBypass(t *testing.T, db *postgres.DB, q string, args ...any) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(t.Context(), q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// failAdvance puts an advancing clock where a failed catch-up leaves it. The
// server's catch-up has no job that fails, so the test writes the state.
func failAdvance(t *testing.T, db *postgres.DB, clockID string) {
	t.Helper()
	execBypass(t, db, `UPDATE test_clocks SET status = 'internal_failure', last_failure_reason = 'a job failed' WHERE id = $1`, clockID)
}

// TestE2E_TestClockAudit_Rows pins the four rows an operator reads in the
// audit log: action, label, metadata and the clock's own time axis (the
// clock's time when the operator acted). A change to how they are written
// must leave them as they are.
func TestE2E_TestClockAudit_Rows(t *testing.T) {
	r := newPPRig(t, "clock audit rows")
	t0 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)

	id := r.clock("pinned", t0)
	r.must(time.Time{}, http.MethodPost, "/v1/test-clocks/"+id+"/advance", map[string]any{"frozen_time": t1.Format(time.RFC3339)}, http.StatusOK)
	failAdvance(t, r.db, id)
	r.must(time.Time{}, http.MethodPost, "/v1/test-clocks/"+id+"/retry-advance", nil, http.StatusOK)
	r.must(time.Time{}, http.MethodDelete, "/v1/test-clocks/"+id, nil, http.StatusOK)

	s0, s1 := t0.Format(time.RFC3339), t1.Format(time.RFC3339)
	want := []clockAuditRow{
		{Action: "create", ResourceID: id, Label: "pinned", SimAt: t0, TestClockID: id, Metadata: map[string]any{
			"frozen_time": s0, "sim_effective_at": s0, "test_clock_id": id,
		}},
		{Action: "update", ResourceID: id, Label: "pinned", SimAt: t1, TestClockID: id, Metadata: map[string]any{
			"action": "advanced", "frozen_time": s1, "advanced_from": s0, "sim_effective_at": s1, "test_clock_id": id,
		}},
		{Action: "update", ResourceID: id, Label: "pinned", SimAt: t1, TestClockID: id, Metadata: map[string]any{
			"action": "retry_advance", "frozen_time": s1, "sim_effective_at": s1, "test_clock_id": id,
		}},
		{Action: "delete", ResourceID: id, Label: "pinned", SimAt: t1, TestClockID: id, Metadata: map[string]any{
			"action": "teardown", "sim_effective_at": s1, "test_clock_id": id,
		}},
	}
	got := clockAuditRows(t, r.db, r.tenantID)
	if len(got) != len(want) {
		t.Fatalf("test_clock audit rows = %+v, want %d rows", got, len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Action != w.Action || g.ResourceID != w.ResourceID || g.Label != w.Label ||
			!g.SimAt.Equal(w.SimAt) || g.TestClockID != w.TestClockID || !reflect.DeepEqual(g.Metadata, w.Metadata) {
			t.Errorf("row %d:\n got %+v\nwant %+v", i, g, w)
		}
	}
}

// refuseClockAuditRows makes Postgres refuse every test_clock audit row until
// drop is called or the test ends. No request can make that insert fail, so
// the test installs a trigger as the database owner.
func refuseClockAuditRows(t *testing.T) (drop func()) {
	t.Helper()
	admin := testutil.AdminPool(t)
	if _, err := admin.ExecContext(t.Context(), `
		CREATE OR REPLACE FUNCTION test_refuse_clock_audit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected audit fault on %', NEW.action; END $$;
		CREATE TRIGGER test_refuse_clock_audit BEFORE INSERT ON audit_log
		FOR EACH ROW WHEN (NEW.resource_type = 'test_clock')
		EXECUTE FUNCTION test_refuse_clock_audit();
	`); err != nil {
		t.Fatalf("install fault: %v", err)
	}
	dropped := false
	drop = func() {
		if dropped {
			return
		}
		dropped = true
		if _, err := admin.ExecContext(context.Background(), `
			DROP TRIGGER IF EXISTS test_refuse_clock_audit ON audit_log;
			DROP FUNCTION IF EXISTS test_refuse_clock_audit();`); err != nil {
			t.Errorf("drop fault: %v", err)
		}
	}
	t.Cleanup(drop)
	return drop
}

// TestE2E_TestClockAudit_RefusedRowSavesNothing: a change whose audit row
// cannot be written is not saved, and the caller gets the standard 500. Each
// change runs once with the row refused.
func TestE2E_TestClockAudit_RefusedRowSavesNothing(t *testing.T) {
	r := newPPRig(t, "clock audit refused")
	t0 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	s0, s1 := t0.Format(time.RFC3339), t1.Format(time.RFC3339)
	path := func(id string) string { return "/v1/test-clocks/" + id }
	refused := func(change, method, target string, body any) {
		t.Helper()
		drop := refuseClockAuditRows(t)
		code, out, raw := r.do(time.Time{}, method, target, body)
		drop()
		if code != http.StatusInternalServerError || flowStr(out, "error", "code") != "internal_error" {
			t.Errorf("%s with its audit row refused: %d %s, want 500 internal_error", change, code, raw)
		}
	}
	clockAfter := func(id string) map[string]any {
		t.Helper()
		return r.must(time.Time{}, http.MethodGet, path(id), nil, http.StatusOK)
	}

	refused("create", http.MethodPost, "/v1/test-clocks", map[string]any{"name": "refused create", "frozen_time": s0})
	if n := flowCount(t, r.db, `SELECT count(*) FROM test_clocks WHERE tenant_id = $1 AND name = 'refused create'`, r.tenantID); n != 0 {
		t.Errorf("create: %d clocks saved, want none", n)
	}

	advanced := r.clock("refused advance", t0)
	refused("advance", http.MethodPost, path(advanced)+"/advance", map[string]any{"frozen_time": s1})
	if clk := clockAfter(advanced); flowStr(clk, "status") != "ready" || flowStr(clk, "frozen_time") != s0 || clk["last_advance_summary"] != nil {
		t.Errorf("advance: clock after = %v, want it ready at %s with no advance summary", clk, s0)
	}

	retried := r.clock("refused retry", t0)
	r.must(time.Time{}, http.MethodPost, path(retried)+"/advance", map[string]any{"frozen_time": s1}, http.StatusOK)
	failAdvance(t, r.db, retried)
	refused("retry advance", http.MethodPost, path(retried)+"/retry-advance", nil)
	if clk := clockAfter(retried); flowStr(clk, "status") != "internal_failure" || flowStr(clk, "last_failure_reason") != "a job failed" {
		t.Errorf("retry advance: clock after = %v, want it still internal_failure", clk)
	}

	deleted := r.clock("refused delete", t0)
	refused("delete", http.MethodDelete, path(deleted), nil)
	if clk := clockAfter(deleted); flowStr(clk, "id") != deleted {
		t.Errorf("delete: clock after = %v, want it still there", clk)
	}
}

// TestE2E_TestClockAudit_AdvancedFromIsTheReplacedTime: an advance's row
// names the time its own write replaced. Here the clock moves after the
// request read it and before its write lands, as when another advance
// completes in between; the row must still say where the clock moved from.
func TestE2E_TestClockAudit_AdvancedFromIsTheReplacedTime(t *testing.T) {
	r := newPPRig(t, "clock audit advanced from")
	t0 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	moved := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	target := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	id := r.clock("moved", t0)

	// The held row lock makes the advance's write wait after its reads saw
	// t0. Only an advance writes frozen_time, so the test writes it for the
	// advance that completed in between.
	tx, err := r.db.BeginTx(t.Context(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(t.Context(), `UPDATE test_clocks SET frozen_time = $1 WHERE id = $2`, moved, id); err != nil {
		t.Fatalf("move the clock: %v", err)
	}
	type answer struct {
		code int
		raw  []byte
	}
	done := make(chan answer, 1)
	go func() {
		code, _, raw := r.do(time.Time{}, http.MethodPost, "/v1/test-clocks/"+id+"/advance", map[string]any{"frozen_time": target.Format(time.RFC3339)})
		done <- answer{code, raw}
	}()
	testutil.WaitForLockWaiter(t, t.Context(), testutil.AdminPool(t), "UPDATE test_clocks SET status")
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the move: %v", err)
	}
	if a := <-done; a.code != http.StatusOK {
		t.Fatalf("advance: %d %s, want 200", a.code, a.raw)
	}

	movedS := moved.Format(time.RFC3339)
	clk := r.must(time.Time{}, http.MethodGet, "/v1/test-clocks/"+id, nil, http.StatusOK)
	if got := flowStr(clk, "last_advance_summary", "advanced_from"); got != movedS {
		t.Fatalf("setup: the clock's advanced_from = %q, want %s: the advance's write did not wait for the move", got, movedS)
	}
	for _, row := range clockAuditRows(t, r.db, r.tenantID) {
		if row.Metadata["action"] == "advanced" {
			if got := row.Metadata["advanced_from"]; got != movedS {
				t.Errorf("advance row advanced_from = %v, want %s, the time the advance replaced", got, movedS)
			}
			return
		}
	}
	t.Fatal("the advance wrote no audit row")
}
