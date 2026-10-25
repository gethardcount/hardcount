package usage_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// A provider cost follows the unit rule of every *_cents field (ADR-130 §3):
// a rate is unit_cost_cents, US cents per token, and an event's cost goes out
// as provider_cost_cents, a decimal string of cents. The stored stamp stays
// micro-dollars, converted exactly: micros = unit_cost_cents × quantity ×
// 10^4, and cents = micros ÷ 10^4. The old words are refused by name.
func TestProviderCost_InCents(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newProviderCostAuditFixture(t)

	// $3.00 per 1M tokens is 0.0003 cents a token.
	rec, _ := f.serve(t, nil, http.MethodPut, "/", `{"provider":"anthropic","model":"claude-sonnet-4.5","token_type":"input","unit_cost_cents":"0.0003"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("save rate: status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var rate map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &rate); err != nil {
		t.Fatalf("decode rate: %v", err)
	}
	if got := string(rate["unit_cost_cents"]); got != `"0.0003"` {
		t.Errorf("rate unit_cost_cents %s, want \"0.0003\"", got)
	}
	if _, ok := rate["cost_per_token"]; ok {
		t.Errorf("the rate still carries cost_per_token: %s", rec.Body.String())
	}

	// The old word is refused by name, alone or beside the new one, and
	// nothing is written.
	for _, body := range []string{
		`{"provider":"anthropic","model":"claude-sonnet-4.5","token_type":"output","cost_per_token":"0.000015"}`,
		`{"provider":"anthropic","model":"claude-sonnet-4.5","token_type":"output","cost_per_token":"0.000015","unit_cost_cents":"0.000015"}`,
	} {
		rec, _ = f.serve(t, nil, http.MethodPut, "/", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"param":"cost_per_token"`) {
			t.Errorf("%s: status %d body %s, want 422 naming cost_per_token", body, rec.Code, rec.Body.String())
		}
	}
	if n := f.rateCount(t); n != 1 {
		t.Errorf("%d rates stored, want 1", n)
	}

	cust, err := customer.NewPostgresStore(f.db).Create(f.ctx, f.tenantID, domain.Customer{ExternalID: "cus_cents", DisplayName: "Cents"})
	if err != nil {
		t.Fatalf("customer: %v", err)
	}
	meter, err := pricing.NewPostgresStore(f.db).CreateMeter(f.ctx, f.tenantID, domain.Meter{
		EventName: "tokens", Name: "Tokens", Aggregation: "sum", Unit: "tokens",
	})
	if err != nil {
		t.Fatalf("meter: %v", err)
	}
	ts := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	ev, err := usage.NewService(f.store).Ingest(f.ctx, f.tenantID, usage.IngestInput{
		CustomerID: cust.ID, MeterID: meter.ID, Quantity: decimal.NewFromInt(12345),
		Dimensions: map[string]any{"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "input"},
		Timestamp:  &ts, IdempotencyKey: "cents-1",
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// 12345 tokens × 0.0003 cents = 3.7035 cents = 37035 micro-dollars.
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var micros int64
	err = tx.QueryRowContext(f.ctx, `SELECT provider_cost_micros FROM usage_events WHERE idempotency_key = 'cents-1'`).Scan(&micros)
	postgres.Rollback(tx)
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	if micros != 37035 {
		t.Errorf("stored stamp %d micro-dollars, want 37035", micros)
	}

	out, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(out, &wire); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if got := string(wire["provider_cost_cents"]); got != `"3.7035"` {
		t.Errorf("event provider_cost_cents %s, want \"3.7035\"; event %s", got, out)
	}
	if _, ok := wire["provider_cost_micros"]; ok {
		t.Errorf("the event still carries provider_cost_micros: %s", out)
	}
}
