package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
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
