package litellm

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"unicode/utf8"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// TestSpend_NewCallTypes_RealStore posts captured LiteLLM 1.83.9 proxy
// records of the call types this change starts billing through the real
// handler into real Postgres.
func TestSpend_NewCallTypes_RealStore(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := postgres.WithLivemode(context.Background(), false)
	tenantID := testutil.CreateTestTenant(t, db, "LiteLLM Call Types")
	svc := usage.NewService(usage.NewPostgresStore(db))
	h := New(customer.NewPostgresStore(db), pricing.NewService(pricing.NewPostgresStore(db)), svc, svc)

	spend := func(t *testing.T, recs ...StandardLoggingPayload) SpendResponse {
		t.Helper()
		code, body := postSpendWithCtx(t, h, ctx, tenantID, recs...)
		if code != http.StatusOK {
			t.Fatalf("status %d body %s", code, body)
		}
		var r SpendResponse
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(r.Errors) != 0 {
			t.Fatalf("rows refused: %+v", r.Errors)
		}
		return r
	}
	exec := func(t *testing.T, q string, args ...any) {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	newCustomer := func(t *testing.T, ext string) string {
		t.Helper()
		id := postgres.NewID("vlx_cus")
		exec(t, `INSERT INTO customers (id, tenant_id, external_id, display_name, email) VALUES ($1, $2, $3, $3, $3 || '@example.com')`, id, tenantID, ext)
		return id
	}
	// rows returns the stored keys of the customer's usage.
	rows := func(t *testing.T, customerID string) []string {
		t.Helper()
		tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer postgres.Rollback(tx)
		rs, err := tx.QueryContext(ctx, `SELECT idempotency_key FROM usage_events WHERE customer_id = $1 ORDER BY idempotency_key`, customerID)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		defer func() { _ = rs.Close() }()
		var out []string
		for rs.Next() {
			var k string
			if err := rs.Scan(&k); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, k)
		}
		return out
	}

	meterID := postgres.NewID("vlx_mtr")
	exec(t, `INSERT INTO meters (id, tenant_id, name, event_name, unit, aggregation) VALUES ($1, $2, 'Tokens', 'tokens', 'tokens', 'sum')`, meterID, tenantID)

	t.Run("a /v1/messages call bills once", func(t *testing.T) {
		rec := proxyRecord(t, "messages-split")
		cus := newCustomer(t, rec.EndUser)
		if r := spend(t, rec); r.Accepted != 5 {
			t.Fatalf("first delivery %+v, want 5 accepted", r)
		}
		if r := spend(t, rec); r.Accepted != 0 || r.Deduplicated != 5 {
			t.Fatalf("resend %+v, want 5 deduplicated", r)
		}
		if n := len(rows(t, cus)); n != 5 {
			t.Fatalf("%d rows, want 5", n)
		}
	})

	t.Run("a replay from LiteLLM's spend log fills only the gap", func(t *testing.T) {
		// docs/ops/runbook/litellm-spend-gap.md: after an outage the operator
		// replays LiteLLM_SpendLogs, whose times keep only milliseconds
		// (TIMESTAMP(3)). The call already stored must deduplicate, and the
		// one lost in the outage must bill.
		stored := proxyRecord(t, "messages-split")
		stored.ID, stored.EndUser = "replay-stored", "cus_replay"
		lost := stored
		lost.ID = "replay-lost"
		cus := newCustomer(t, stored.EndUser)
		if r := spend(t, stored); r.Accepted != 5 {
			t.Fatalf("live delivery %+v, want 5 accepted", r)
		}
		fromLog := func(p StandardLoggingPayload) StandardLoggingPayload {
			p.StartTime = math.Round(p.StartTime*1000) / 1000
			p.EndTime = math.Round(p.EndTime*1000) / 1000
			return p
		}
		if r := spend(t, fromLog(stored), fromLog(lost)); r.Accepted != 5 || r.Deduplicated != 5 {
			t.Fatalf("replay %+v, want 5 accepted (the lost call) and 5 deduplicated (the stored one)", r)
		}
		if n := len(rows(t, cus)); n != 10 {
			t.Fatalf("%d rows, want 10 (two calls of 5)", n)
		}
	})

	t.Run("two calls sharing a client-chosen id store one (decision D6: never bill twice)", func(t *testing.T) {
		// The same as LiteLLM's own spend log, whose primary key is the id.
		recs := proxyRecords(t, "messages-same-call-id")
		a, b := newCustomer(t, recs[0].EndUser), newCustomer(t, recs[1].EndUser)
		if r := spend(t, recs...); r.Accepted != 3 || r.Deduplicated != 3 {
			t.Fatalf("delivery %+v, want 3 accepted and 3 deduplicated", r)
		}
		if na, nb := len(rows(t, a)), len(rows(t, b)); na != 3 || nb != 0 {
			t.Fatalf("rows %d and %d, want 3 and 0", na, nb)
		}
	})

	t.Run("two copies of one call bill once", func(t *testing.T) {
		recs := proxyRecords(t, "responses-bridged-logging-hook")
		cus := newCustomer(t, recs[0].EndUser)
		if r := spend(t, recs...); r.Accepted != 3 || r.Deduplicated != 3 {
			t.Fatalf("delivery %+v, want 3 accepted and 3 deduplicated", r)
		}
		if n := len(rows(t, cus)); n != 3 {
			t.Fatalf("%d rows, want 3", n)
		}
	})

	t.Run("a /v1/responses call with a long id is held, released and deduplicated", func(t *testing.T) {
		rec := proxyRecord(t, "responses")
		if r := spend(t, rec); r.Held != 3 {
			t.Fatalf("delivery for an unknown customer %+v, want 3 held", r)
		}
		cus := newCustomer(t, rec.EndUser)
		res, err := svc.Release(ctx, tenantID, usage.ReleaseInput{
			ExternalCustomerID: rec.EndUser, CustomerID: cus,
			MeterIDs: map[string]string{EventNameTokens: meterID},
		})
		if err != nil || res.Released != 3 {
			t.Fatalf("release %+v err %v, want 3 released", res, err)
		}
		if r := spend(t, rec); r.Accepted != 0 || r.Deduplicated != 3 {
			t.Fatalf("resend after release %+v, want 3 deduplicated", r)
		}
		keys := rows(t, cus)
		if len(keys) != 3 {
			t.Fatalf("%d rows, want 3", len(keys))
		}
		for _, k := range keys {
			if utf8.RuneCountInString(k) > usage.MaxIdempotencyKeyLen {
				t.Errorf("stored key is %d characters, over %d", utf8.RuneCountInString(k), usage.MaxIdempotencyKeyLen)
			}
		}
	})
}
