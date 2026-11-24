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

	// EVERY filter at once. A $N that drifts out of step with its arg does not
	// fail loudly — Postgres happily compares resource_id against a timestamp
	// arg and returns the wrong rows — so the whole set has to be pinned
	// together.
	where, args, nextIdx := buildListWhere("t-1", true, QueryFilter{
		ResourceType: "invoice",
		ResourceID:   "hc_inv_1",
		Action:       "invoice.finalize",
		ActorID:      "hc_usr_1",
		DateFrom:     from,
		DateTo:       to,
	})

	want := " WHERE al.tenant_id = $1 AND al.livemode = $2" +
		" AND al.resource_type = $3 AND al.resource_id = $4" +
		" AND al.action = $5 AND al.actor_id = $6" +
		" AND al.created_at >= $7 AND al.created_at <= $8"
	if where != want {
		t.Fatalf("WHERE mismatch:\n got %q\nwant %q", where, want)
	}
	if len(args) != 8 {
		t.Fatalf("args: got %d, want 8 (%v)", len(args), args)
	}
	if nextIdx != 9 {
		t.Errorf("next placeholder: got %d, want 9 (LIMIT binds here)", nextIdx)
	}
	// Arg ordering must match placeholder order, position by position — this is
	// what catches a filter inserted in the SQL but appended in the wrong place.
	for i, want := range []any{
		"t-1", true,
		"invoice", "hc_inv_1", "invoice.finalize", "hc_usr_1",
		from, to,
	} {
		if args[i] != want {
			t.Errorf("arg $%d: got %v, want %v", i+1, args[i], want)
		}
	}
}
