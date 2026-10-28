package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// TestIngest_IdempotencyKeyHeaderIsIgnored sends two different events under
// one Idempotency-Key header to the single and the batch route, through the
// full router to real Postgres. The usage routes ignore the header (the
// router passes isIngestPath to mw.Idempotency): each event's own
// idempotency_key is the one dedup rule there. So all four are stored, no
// answer is a replay, and no idempotency_keys row is written. The
// middleware's own test passes a predicate that always skips, so only this
// test sees the router's wiring.
//
// Mutation-verify: pass nil instead of isIngestPath in router.go → the second
// event on each route answers 422 idempotency_error, and rows are written.
func TestIngest_IdempotencyKeyHeaderIsIgnored(t *testing.T) {
	skipShort(t)
	r := newLagRigBare(t, "in_arrears", 0)
	ts := lagP0.Add(24 * time.Hour)
	at := lagP0.Add(48 * time.Hour)
	ev := func(key string, qty int) map[string]any {
		return map[string]any{
			"external_customer_id": r.custExt, "event_name": "calls", "quantity": qty,
			"timestamp": ts.Format(time.RFC3339), "idempotency_key": key,
		}
	}
	for _, req := range []struct {
		path string
		body any
	}{
		{"/v1/usage-events", ev("hdr-1", 1)},
		{"/v1/usage-events", ev("hdr-2", 2)},
		{"/v1/usage-events/batch", []map[string]any{ev("hdr-3", 3)}},
		{"/v1/usage-events/batch", []map[string]any{ev("hdr-4", 4)}},
	} {
		rec := r.doKey(http.MethodPost, req.path, "one-header-key", at, req.body)
		if rec.Code != http.StatusCreated {
			t.Errorf("%s %v: status %d, want 201; body %s", req.path, req.body, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Idempotent-Replayed"); got != "" {
			t.Errorf("%s %v: Idempotent-Replayed = %q, want unset", req.path, req.body, got)
		}
	}

	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var events, total, keys int
	if err := tx.QueryRowContext(ctx, `SELECT count(*), COALESCE(sum(quantity), 0)::int FROM usage_events WHERE tenant_id = $1`,
		r.tenantID).Scan(&events, &total); err != nil {
		t.Fatalf("read events: %v", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM idempotency_keys WHERE tenant_id = $1`, r.tenantID).Scan(&keys); err != nil {
		t.Fatalf("read idempotency keys: %v", err)
	}
	if events != 4 || total != 10 || keys != 0 {
		t.Errorf("stored %d events (sum %d) and %d idempotency_keys rows, want 4 (sum 10) and 0", events, total, keys)
	}
}
