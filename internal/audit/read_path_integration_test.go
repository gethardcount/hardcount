package audit_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Read-path integration coverage (audit e2e 2026-07-13 — the read path had
// ZERO tests). Pins: tenant+livemode isolation, page continuation, FilterOptions mode scoping (with a
// mode-distinct action so leakage is visible), the 0147 index columns, the
// negative-offset 422 at the handler, and — the core of PR2 — an EXPLAIN
// assertion that the explicit predicates actually drive the read index
// (results alone cannot pin that: RLS returns identical rows either way,
// only the plan differs; see TestBuildListWhere_PinsPredicates for the
// code-level twin).
func TestAuditReadPath_PredicatesPagingAndIndex(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenant1 := testutil.CreateTestTenant(t, db, "Read Path T1")
	tenant2 := testutil.CreateTestTenant(t, db, "Read Path T2")

	base := context.Background()
	ctxLive := postgres.WithLivemode(base, true)
	ctxTest := postgres.WithLivemode(base, false)
	ctx, cancel := context.WithTimeout(base, 30*time.Second)
	defer cancel()

	logger := audit.NewLogger(db)

	// Seed: 3 live rows for tenant1; 1 test-mode row for tenant1 with a
	// MODE-DISTINCT action+resource (so FilterOptions leakage across the
	// mode plane is observable, not masked by identical vocabulary); 1 live
	// row for tenant2 with a TENANT-DISTINCT action.
	for _, action := range []string{"invoice.finalize", "invoice.void", "invoice.send"} {
		if err := logger.Log(ctxLive, tenant1, action, "invoice", "hc_inv_t1", "", nil); err != nil {
			t.Fatalf("seed t1 live: %v", err)
		}
		// Distinct created_at per row so the order is deterministic.
		time.Sleep(5 * time.Millisecond)
	}
	if err := logger.Log(ctxTest, tenant1, "testmode.probe", "probe", "hc_prb_1", "", nil); err != nil {
		t.Fatalf("seed t1 test-mode: %v", err)
	}
	if err := logger.Log(ctxLive, tenant2, "customer.delete", "customer", "hc_cus_t2", "", nil); err != nil {
		t.Fatalf("seed t2 live: %v", err)
	}

	t.Run("tenant and livemode isolation", func(t *testing.T) {
		live, total, err := logger.Query(ctxLive, tenant1, audit.QueryFilter{})
		if err != nil {
			t.Fatalf("query t1 live: %v", err)
		}
		if len(live) != 3 || total != 3 {
			t.Fatalf("t1 live rows: got %d (total %d), want 3", len(live), total)
		}
		for _, e := range live {
			if e.TenantID != tenant1 {
				t.Errorf("cross-tenant row leaked: %+v", e)
			}
			if e.Action == "testmode.probe" {
				t.Errorf("test-mode row leaked into live query")
			}
		}

		testMode, total, err := logger.Query(ctxTest, tenant1, audit.QueryFilter{})
		if err != nil {
			t.Fatalf("query t1 test-mode: %v", err)
		}
		if len(testMode) != 1 || total != 1 {
			t.Fatalf("t1 test-mode rows: got %d (total %d), want 1 — live/test planes must not mix", len(testMode), total)
		}
	})

	t.Run("offset page 2 continues page 1 without overlap", func(t *testing.T) {
		page1, total1, err := logger.Query(ctxLive, tenant1, audit.QueryFilter{Limit: 1})
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if len(page1) != 1 {
			t.Fatalf("page 1 rows: got %d, want 1", len(page1))
		}
		page2, total2, err := logger.Query(ctxLive, tenant1, audit.QueryFilter{Limit: 2, Offset: 1})
		if err != nil {
			t.Fatalf("page 2: %v", err)
		}
		if len(page2) != 2 || total1 != 3 || total2 != 3 {
			t.Fatalf("page 2: %d rows, totals %d and %d; want 2 rows and total 3", len(page2), total1, total2)
		}
		for _, e := range page2 {
			if e.ID == page1[0].ID {
				t.Errorf("page 2 re-served the page-1 row %s", e.ID)
			}
		}
		// DESC ordering must hold across the page boundary.
		if page1[0].CreatedAt.Before(page2[0].CreatedAt) || page2[0].CreatedAt.Before(page2[1].CreatedAt) {
			t.Errorf("pages not newest first: %v, %v, %v", page1[0].CreatedAt, page2[0].CreatedAt, page2[1].CreatedAt)
		}
	})

	t.Run("filter options scoped to tenant and mode", func(t *testing.T) {
		actions, resourceTypes, err := logger.FilterOptions(ctxLive, tenant1)
		if err != nil {
			t.Fatalf("filter options: %v", err)
		}
		for _, a := range actions {
			if a == "customer.delete" {
				t.Error("tenant2's action leaked into tenant1's filter vocabulary")
			}
			if a == "testmode.probe" {
				t.Error("test-mode action leaked into the live-mode filter vocabulary")
			}
		}
		for _, rt := range resourceTypes {
			if rt == "customer" || rt == "probe" {
				t.Errorf("foreign-plane resource_type %q leaked into live vocabulary", rt)
			}
		}
		if len(actions) != 3 {
			t.Errorf("actions: got %v, want the 3 live-mode t1 actions", actions)
		}
	})

	t.Run("0147 index columns", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		rows, err := tx.QueryContext(ctx,
			`SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'audit_log'`)
		if err != nil {
			t.Fatalf("pg_indexes: %v", err)
		}
		defer func() { _ = rows.Close() }()
		defs := map[string]string{}
		for rows.Next() {
			var n, d string
			if err := rows.Scan(&n, &d); err != nil {
				t.Fatalf("scan: %v", err)
			}
			defs[n] = d
		}
		readIdx, ok := defs["idx_audit_log_tenant_read"]
		if !ok {
			t.Fatal("idx_audit_log_tenant_read missing — migration 0147 not applied?")
		}
		if !strings.Contains(readIdx, "(tenant_id, livemode, created_at DESC, id DESC)") {
			t.Errorf("read index columns drifted from the seek shape: %s", readIdx)
		}
		if _, exists := defs["idx_audit_log_created"]; exists {
			t.Error("idx_audit_log_created survived 0147 (0030 already dropped its duplicate twin)")
		}
	})

	// The core PR2 pin: with the explicit predicates the planner drives
	// idx_audit_log_tenant_read via an Index Cond; on RLS quals alone it
	// structurally cannot (the policy's bypass OR-arm references no
	// columns). enable_seqscan=off removes the tiny-table bias so the
	// assertion is about qual derivability, not cost: if the predicates
	// are ever reverted, no Index Cond on tenant_id can exist and this
	// fails even with seq scans disabled.
	t.Run("EXPLAIN uses the read index via Index Cond", func(t *testing.T) {
		tx, err := db.BeginTx(postgres.WithLivemode(ctx, true), postgres.TxTenant, tenant1)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatalf("disable seqscan: %v", err)
		}
		rows, err := tx.QueryContext(ctx, `EXPLAIN (COSTS OFF)
			SELECT al.id FROM audit_log al
			WHERE al.tenant_id = $1 AND al.livemode = $2
			ORDER BY al.created_at DESC, al.id DESC LIMIT 50`, tenant1, true)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		defer func() { _ = rows.Close() }()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan: %v", err)
			}
			plan.WriteString(line + "\n")
		}
		p := plan.String()
		if !strings.Contains(p, "idx_audit_log_tenant_read") {
			t.Errorf("plan does not use idx_audit_log_tenant_read:\n%s", p)
		}
		if !strings.Contains(p, "Index Cond:") || !strings.Contains(p, "tenant_id") {
			t.Errorf("plan lacks a tenant_id Index Cond (predicates not index-driving):\n%s", p)
		}
	})

	t.Run("handler refuses a negative offset", func(t *testing.T) {
		h := audit.NewHandler(logger)
		req := httptest.NewRequest("GET", "/?offset=-5", nil)
		reqCtx := context.WithValue(postgres.WithLivemode(base, true), auth.TestTenantIDKey(), tenant1)
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, req.WithContext(reqCtx))
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"param":"offset"`) {
			t.Fatalf("a negative offset must be a 422 naming offset, not clamped: got %d body=%s", rec.Code, rec.Body.String())
		}
	})
}
