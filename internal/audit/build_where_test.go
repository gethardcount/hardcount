package audit

import (
	"strings"
	"testing"
	"time"
)

// Pins PR2's core change at the code level: Query's WHERE must lead with the
// explicit tenant_id + livemode predicates. Removing them does NOT change
// query results (RLS returns the same rows) — it only silently destroys the
// query plan, turning every audit read back into a full multi-tenant seq
// scan. Result-based tests are therefore structurally blind to a revert;
// this test is not. Its integration twin (the EXPLAIN pin in
// read_path_integration_test.go) proves the predicate shape actually drives
// idx_audit_log_tenant_read.
func TestBuildListWhere_PinsPredicates(t *testing.T) {
	where, args, nextIdx := buildListWhere("t-123", false, QueryFilter{})

	if !strings.HasPrefix(where, " WHERE al.tenant_id = $1 AND al.livemode = $2") {
		t.Fatalf("WHERE must lead with explicit tenant+livemode predicates; got %q", where)
	}
	if len(args) != 2 || args[0] != "t-123" || args[1] != false {
		t.Fatalf("args must carry [tenantID, livemode]; got %v", args)
	}
	if nextIdx != 3 {
		t.Errorf("next placeholder: got %d, want 3", nextIdx)
	}
}

// Placeholder alignment across every optional filter — the class of bug the
// $n renumbering could introduce.
func TestBuildListWhere_AllFiltersAlignPlaceholders(t *testing.T) {
	from := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)

	simFrom := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	simTo := time.Date(2027, 3, 31, 0, 0, 0, 0, time.UTC)

	// EVERY filter at once. A $N that drifts out of step with its arg does not
	// fail loudly — Postgres happily compares resource_id against a timestamp
	// arg and returns the wrong rows — so the whole set has to be pinned
	// together. The three sim filters are the ones this axis added; they were
	// the ones not covered here.
	where, args, nextIdx := buildListWhere("t-1", true, QueryFilter{
		ResourceType: "invoice",
		ResourceID:   "vlx_inv_1",
		Action:       "invoice.finalize",
		ActorID:      "vlx_usr_1",
		TestClockID:  "vlx_tclk_1",
		SimFrom:      simFrom,
		SimTo:        simTo,
		DateFrom:     from,
		DateTo:       to,
	})

	// Clause order is NOT arbitrary: the sim predicates sit immediately after
	// tenant/livemode because that is the leading-key order of the partial clock
	// index (tenant_id, livemode, test_clock_id, created_at DESC, id DESC).
	want := " WHERE al.tenant_id = $1 AND al.livemode = $2" +
		" AND al.test_clock_id = $3" +
		" AND al.sim_effective_at >= $4 AND al.sim_effective_at <= $5" +
		" AND al.resource_type = $6 AND al.resource_id = $7" +
		" AND al.action = $8 AND al.actor_id = $9" +
		" AND al.created_at >= $10 AND al.created_at <= $11"
	if where != want {
		t.Fatalf("WHERE mismatch:\n got %q\nwant %q", where, want)
	}
	if len(args) != 11 {
		t.Fatalf("args: got %d, want 11 (%v)", len(args), args)
	}
	if nextIdx != 12 {
		t.Errorf("next placeholder: got %d, want 12 (LIMIT binds here)", nextIdx)
	}
	// Arg ordering must match placeholder order, position by position — this is
	// what catches a filter inserted in the SQL but appended in the wrong place.
	for i, want := range []any{
		"t-1", true, "vlx_tclk_1", simFrom, simTo,
		"invoice", "vlx_inv_1", "invoice.finalize", "vlx_usr_1",
		from, to,
	} {
		if args[i] != want {
			t.Errorf("arg $%d: got %v, want %v", i+1, args[i], want)
		}
	}
}
