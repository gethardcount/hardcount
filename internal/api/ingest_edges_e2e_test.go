package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// TestIngest_EdgeQuantitiesAndEmptyProperties sends, through the full router
// to real Postgres, the B5 review-round-1 cases at all three doors:
//
//   - a quantity of 10^26 or more written so that shopspring's float digit
//     count misses it (coefficient 10^15..10^15+2). The column
//     (NUMERIC(38,12)) cannot hold it, so a size check that let it through
//     answered 500 on single and backfill, and a batch 422 carrying the
//     Postgres error text. It must be a clean 422 naming quantity;
//   - 10^-12 written to 27 places. Trailing zeros do not count, so it fits
//     and is stored as 0.000000000001;
//   - "properties": null and {}, which carry no dimensions. They are taken
//     as if absent; dimensions sent beside {} are stored.
func TestIngest_EdgeQuantitiesAndEmptyProperties(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_arrears", false)
	ts := lagP0.Add(24 * time.Hour)
	at := lagP0.Add(48 * time.Hour)
	n := 0
	ev := func(qty any, extra map[string]any) map[string]any {
		n++
		e := map[string]any{
			"external_customer_id": r.custExt, "event_name": "calls", "quantity": qty,
			"timestamp": ts.Format(time.RFC3339), "idempotency_key": fmt.Sprintf("edge-%d", n),
		}
		for k, v := range extra {
			e[k] = v
		}
		return e
	}
	send := func(door string, e map[string]any) (int, string) {
		switch door {
		case "batch":
			code, raw := r.do(http.MethodPost, "/v1/usage-events/batch", at, []map[string]any{e})
			return code, string(raw)
		case "backfill":
			code, raw := r.do(http.MethodPost, "/v1/usage-events/backfill", at, e)
			return code, string(raw)
		default:
			code, raw := r.do(http.MethodPost, "/v1/usage-events", at, e)
			return code, string(raw)
		}
	}
	doors := []string{"single", "backfill", "batch"}

	for name, qty := range map[string]any{
		"over 10^26, exponent number": json.Number("1.000000000000001e26"),
		"over 10^26, exponent string": "1.000000000000001e26",
		"10^26 as 10^15 × 10^11":      json.Number("1000000000000000e11"),
		"10^26 written out":           "100000000000000000000000000",
	} {
		for _, door := range doors {
			code, body := send(door, ev(qty, nil))
			if code != http.StatusUnprocessableEntity {
				t.Errorf("%s/%s: status %d, want 422; body %s", door, name, code, body)
				continue
			}
			want := `"param":"quantity"`
			if door == "batch" {
				want = `event[0]: quantity`
			}
			if !strings.Contains(body, want) || strings.Contains(body, "SQLSTATE") {
				t.Errorf("%s/%s: body %s, want %s and no SQL error text", door, name, body, want)
			}
		}
	}

	accepted := []map[string]any{}
	for _, door := range doors {
		for _, e := range []map[string]any{
			ev("0.000000000001000000000000000", nil),
			ev(json.Number("9.99999999999999e25"), nil),
			ev(1, map[string]any{"properties": nil}),
			ev(1, map[string]any{"properties": map[string]any{}}),
			ev(1, map[string]any{"properties": map[string]any{}, "dimensions": map[string]any{"model": "m1"}}),
		} {
			if code, body := send(door, e); code != http.StatusCreated {
				t.Errorf("%s: %v: status %d, want 201; body %s", door, e, code, body)
				continue
			}
			accepted = append(accepted, e)
		}
	}

	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var events, tiny, big, withDims int
	if err := tx.QueryRowContext(ctx, `SELECT count(*),
			count(*) FILTER (WHERE quantity = 0.000000000001),
			count(*) FILTER (WHERE quantity = 99999999999999900000000000),
			count(*) FILTER (WHERE properties @> '{"model":"m1"}')
		FROM usage_events WHERE tenant_id = $1`, r.tenantID).Scan(&events, &tiny, &big, &withDims); err != nil {
		t.Fatalf("read events: %v", err)
	}
	if events != len(accepted) || events != 15 || tiny != 3 || big != 3 || withDims != 3 {
		t.Errorf("stored %d events (%d at 1e-12, %d at 9.99999999999999e25, %d with dimensions), want 15 (3, 3, 3)",
			events, tiny, big, withDims)
	}
	var stored decimal.Decimal
	if err := tx.QueryRowContext(ctx, `SELECT quantity FROM usage_events WHERE tenant_id = $1 AND quantity < 1 LIMIT 1`, r.tenantID).Scan(&stored); err != nil {
		t.Fatalf("read tiny: %v", err)
	}
	if !stored.Equal(decimal.New(1, -12)) {
		t.Errorf("1e-12 stored as %s", stored)
	}
}
