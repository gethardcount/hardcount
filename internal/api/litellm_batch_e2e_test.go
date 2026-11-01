package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// TestE2E_LiteLLM_FullDefaultBatchLands sends a full LiteLLM batch over the
// real router and database, the way the proxy's generic_api logger sends one:
// a JSON array of 512 records (DEFAULT_BATCH_SIZE), each the captured proxy
// record padded to about 8 KB as a real 1.83.9 record is, some 4 MB in all,
// with an ingest key. Every record lands: two usage events a call, 1,024 rows.
//
// Before the fix the router capped every body at 1 MB, so this batch was
// answered 400 and LiteLLM, which never resends a 4xx, dropped it whole.
// Mutation-verify: drop the LiteLLM branch in limitRequestBody → 413 and no
// rows.
func TestE2E_LiteLLM_FullDefaultBatchLands(t *testing.T) {
	db := testutil.SetupTestDB(t)
	t.Setenv("HARDCOUNT_BOOTSTRAP_TOKEN", "litellm-batch-bootstrap")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	defer ts.Close()

	const email, password = "owner@litellm-batch.test", "litellm-batch-pw-12"
	boot := flowBootstrap(t, ts, "litellm-batch-bootstrap", email, password)
	tenantID := flowStr(boot, "tenant", "id")
	owner := login(t, ts, email, password)
	asOwner := func(method, path string, body any) flowResp {
		t.Helper()
		return flowDo(t, ts, method, path, flowReq{cookie: owner, body: body})
	}
	asOwner("POST", "/v1/customers", map[string]any{"external_id": "acme-batch", "display_name": "Acme"}).want(t, 201, "create customer")
	asOwner("POST", "/v1/meters", map[string]any{"event_name": litellm.EventNameTokens, "name": "Tokens", "unit": "tokens", "aggregation": "sum"}).want(t, 201, "create tokens meter")
	key := flowStr(asOwner("POST", "/v1/api-keys", map[string]any{"name": "LiteLLM proxy", "key_type": "ingest"}).want(t, 201, "create an ingest key").body, "raw_key")

	const calls = 512
	body := e2eLiteLLMBatch(t, calls, "acme-batch", 8400)
	if len(body) <= 4<<20 {
		t.Fatalf("batch is %d bytes, want over 4 MB (a full default batch)", len(body))
	}
	spend := flowDo(t, ts, "POST", "/v1/integrations/litellm/spend", flowReq{bearer: key, body: body}).want(t, 200, "a full LiteLLM batch")
	if got := flowInt(spend.body, "accepted"); got != 2*calls {
		t.Fatalf("answer %s, want accepted %d (input and output for each call)", spend.raw, 2*calls)
	}

	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var rows, distinctCalls int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT split_part(idempotency_key, ':', 1))
		FROM usage_events WHERE idempotency_key LIKE 'chatcmpl-batch-%'`).Scan(&rows, &distinctCalls); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 2*calls || distinctCalls != calls {
		t.Fatalf("usage_events holds %d rows from %d calls, want %d rows from all %d calls", rows, distinctCalls, 2*calls, calls)
	}
}

// e2eLiteLLMBatch is the JSON array LiteLLM sends: n copies of the captured
// proxy record (internal/integrations/litellm/testdata), each with its own id,
// the customer, an end time a minute ago, and its messages padded so the
// record is about recordBytes long.
func e2eLiteLLMBatch(t *testing.T, n int, customer string, recordBytes int) []byte {
	t.Helper()
	raw, err := os.ReadFile("../integrations/litellm/testdata/litellm-1.83.9-generic-api.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture []map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture) != 1 {
		t.Fatalf("decode fixture: %v (%d records)", err, len(fixture))
	}
	base, err := json.Marshal(fixture[0])
	if err != nil {
		t.Fatalf("encode fixture record: %v", err)
	}
	pad := strings.Repeat("x", max(0, recordBytes-len(base)))
	end := float64(time.Now().Add(-time.Minute).Unix())
	records := make([]map[string]any, n)
	for i := range records {
		rec := make(map[string]any, len(fixture[0]))
		for k, v := range fixture[0] {
			rec[k] = v
		}
		rec["id"] = fmt.Sprintf("chatcmpl-batch-%04d", i)
		rec["end_user"] = customer
		rec["startTime"], rec["endTime"] = end-1, end
		rec["messages"] = []map[string]any{{"role": "user", "content": pad}}
		records[i] = rec
	}
	body, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("encode batch: %v", err)
	}
	return body
}
