package litellm

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestSpend_RealStoreContract proves the classification against the REAL
// stores (fake-fidelity rule): an unmapped customer on a live database is
// errs.ErrNotFound → 200 with the rows held (#1044); the same lookup against a closed pool
// is a raw driver error → 503. If a store ever returned ErrNotFound for an
// infrastructure failure, the second half goes red.
func TestSpend_RealStoreContract(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "LiteLLM 503 Contract")

	svc := usage.NewService(usage.NewPostgresStore(db))
	h := New(customer.NewPostgresStore(db), pricing.NewService(pricing.NewPostgresStore(db)), svc, svc)
	post := func(t *testing.T) (int, string) {
		t.Helper()
		code, body := postSpendWithCtx(t, h, ctx, tenantID, spendPayload("real-1"))
		return code, body
	}

	code, body := post(t)
	if code != http.StatusOK || !strings.Contains(body, `"held":2`) {
		t.Fatalf("unmapped customer on a live DB: status %d body %s, want 200 with both rows held", code, body)
	}

	_ = db.Pool.Close() // the store is now unreachable — every lookup is a raw driver error
	code, body = post(t)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("closed pool: status %d body %s, want 503 (a store failure must not masquerade as not-found)", code, body)
	}
	if strings.Contains(body, "not found") || strings.Contains(body, "closed") {
		t.Fatalf("closed pool: body leaks or misclassifies: %s", body)
	}
}

// TestSpend_HeldThenReleasedOnce_RealStore is #1044 end to end on real
// Postgres: usage for a customer that does not exist yet is held (not lost),
// a LiteLLM resend of it is deduplicated, the release writes it once, and a
// resend after the release is deduplicated again — two token rows, never
// four.
// Mutation-verify: change the release's ingest key (e.g. append a suffix
// in ReleaseHeld) → the post-release resend writes two more rows and the
// final count is 4.
func TestSpend_HeldThenReleasedOnce_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "LiteLLM Held")

	svc := usage.NewService(usage.NewPostgresStore(db))
	h := New(customer.NewPostgresStore(db), pricing.NewService(pricing.NewPostgresStore(db)), svc, svc)
	payload := spendPayload("held-1")
	payload.User = "cus_not_yet"

	spend := func(t *testing.T) SpendResponse {
		t.Helper()
		code, body := postSpendWithCtx(t, h, ctx, tenantID, payload)
		if code != http.StatusOK {
			t.Fatalf("status %d body %s", code, body)
		}
		var r SpendResponse
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return r
	}
	count := func(t *testing.T) int {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events WHERE idempotency_key LIKE 'held-1:%'`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	if r := spend(t); r.Held != 2 || r.Accepted != 0 || len(r.Errors) != 0 {
		t.Fatalf("first delivery = %+v, want 2 held and no errors", r)
	}
	if r := spend(t); r.Held != 0 || r.Deduplicated != 2 {
		t.Fatalf("resend while held = %+v, want 2 deduplicated", r)
	}

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	customerID, meterID := postgres.NewID("vlx_cus"), postgres.NewID("vlx_mtr")
	if _, err := tx.ExecContext(ctx, `INSERT INTO customers (id, tenant_id, external_id, display_name, email) VALUES ($1, $2, 'cus_not_yet', 'Late', 'late@example.com')`, customerID, tenantID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meters (id, tenant_id, name, key, unit, aggregation) VALUES ($1, $2, 'Tokens', 'tokens', 'tokens', 'sum')`, meterID, tenantID); err != nil {
		t.Fatalf("meter: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	res, err := svc.Release(ctx, tenantID, usage.ReleaseInput{
		ExternalCustomerID: "cus_not_yet", CustomerID: customerID,
		MeterIDs: map[string]string{MeterKeyTokens: meterID},
	}, nil, nil)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if res.Released != 2 {
		t.Fatalf("release = %+v, want 2 released", res)
	}
	if r := spend(t); r.Accepted != 0 || r.Deduplicated != 2 {
		t.Fatalf("resend after release = %+v, want 2 deduplicated", r)
	}
	if n := count(t); n != 2 {
		t.Fatalf("usage_events rows = %d, want 2 (input + output, once)", n)
	}
}

// Review finding (#1151), through the LiteLLM door: the operator discards
// held rows, then creates the customer, then LiteLLM resends the batch. The
// resend is counted as deduplicated and nothing reaches usage_events.
// Before the fix the resend found the customer and billed the rows.
func TestSpend_DiscardedRowsStayUnbilledOnResend_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "LiteLLM Discarded")

	svc := usage.NewService(usage.NewPostgresStore(db))
	h := New(customer.NewPostgresStore(db), pricing.NewService(pricing.NewPostgresStore(db)), svc, svc)
	payload := spendPayload("discard-1")
	payload.User = "cus_discarded"

	spend := func(t *testing.T) SpendResponse {
		t.Helper()
		code, body := postSpendWithCtx(t, h, ctx, tenantID, payload)
		if code != http.StatusOK {
			t.Fatalf("status %d body %s", code, body)
		}
		var r SpendResponse
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return r
	}

	if r := spend(t); r.Held != 2 {
		t.Fatalf("first delivery = %+v, want 2 held", r)
	}
	if n, err := svc.Discard(ctx, tenantID, "cus_discarded", ""); err != nil || n != 2 {
		t.Fatalf("discard = %d (%v), want 2", n, err)
	}

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO customers (id, tenant_id, external_id, display_name, email) VALUES ($1, $2, 'cus_discarded', 'Gone', 'gone@example.com')`, postgres.NewID("vlx_cus"), tenantID); err != nil {
		t.Fatalf("customer: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meters (id, tenant_id, name, key, unit, aggregation) VALUES ($1, $2, 'Tokens', 'tokens', 'tokens', 'sum')`, postgres.NewID("vlx_mtr"), tenantID); err != nil {
		t.Fatalf("meter: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if r := spend(t); r.Accepted != 0 || r.Deduplicated != 2 || len(r.Errors) != 0 {
		t.Fatalf("resend after discard = %+v, want 2 deduplicated", r)
	}
	tx, err = db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM usage_events WHERE idempotency_key LIKE 'discard-1:%'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("usage_events rows = %d, want 0: discarded rows were billed", n)
	}
}
