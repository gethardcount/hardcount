package usage_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// cost_per_token is a request decimal with documented limits: required, a
// JSON number or a string holding one, at most 100 characters, 0 or more,
// under 10^8 dollars, at most 12 decimal places (the column is
// NUMERIC(20,12)). A value outside them is a 422 naming cost_per_token and
// nothing is written. Before, Postgres rounded a 13th decimal place away and
// answered a value of 10^8 or more with an error (a 500), and a missing
// cost was stored as 0.
//
// Mutation-verify: drop the domain.CheckDecimal call in the upsert handler →
// the size and scale cases go red; drop the required check → the absent case
// goes red.

func costBody(raw string) string {
	cost := ""
	if raw != "" {
		cost = `,"cost_per_token":` + raw
	}
	return `{"provider":"openai","model":"gpt-4o","token_type":"input"` + cost + `}`
}

func TestProviderCostRate_ValidEdgesStoredAsSent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newProviderCostAuditFixture(t)
	for _, tc := range []struct{ raw, want string }{
		{`"0"`, "0"},
		{`"0.000000000001"`, "0.000000000001"},
		{`"99999999.999999999999"`, "99999999.999999999999"},
		{`"0.0000015"`, "0.0000015"},
		{`0.0000025`, "0.0000025"},
		{`"1.500000000000000"`, "1.5"},
		{`"1e-6"`, "0.000001"},
		{`"0e99999"`, "0"},
	} {
		rec, _ := f.serve(t, nil, http.MethodPut, "/", costBody(tc.raw))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200; body %s", tc.raw, rec.Code, rec.Body.String())
		}
		var out struct {
			CostPerToken string `json:"cost_per_token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.CostPerToken != tc.want {
			t.Errorf("%s: stored %s, want %s", tc.raw, out.CostPerToken, tc.want)
		}
		// The row in Postgres holds the same value.
		var exact bool
		tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		err = tx.QueryRowContext(f.ctx, `SELECT cost_per_token = $1::numeric FROM provider_cost_rates`, tc.want).Scan(&exact)
		postgres.Rollback(tx)
		if err != nil {
			t.Fatalf("read row: %v", err)
		}
		if !exact {
			t.Errorf("%s: the stored row is not %s", tc.raw, tc.want)
		}
	}
}

func TestProviderCostRate_OutOfRangeRefusedNamingTheField(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newProviderCostAuditFixture(t)
	for _, raw := range []string{
		``, // absent
		`null`,
		`"100000000"`, // 10^8
		`"1e8"`,
		`"0.0000000000001"`, // 13 places
		`"1.0000000000001"`,
		`"-0.1"`,
		`"abc"`,
		`""`,
		`true`,
		`"0.` + strings.Repeat("0", 120) + `1"`, // over 100 characters
	} {
		rec, _ := f.serve(t, nil, http.MethodPut, "/", costBody(raw))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422; body %s", raw, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), `"param":"cost_per_token"`) {
			t.Errorf("%s: 422 does not name cost_per_token: %s", raw, rec.Body.String())
		}
	}
	if n := f.rateCount(t); n != 0 {
		t.Errorf("%d rates stored, want 0", n)
	}
}

func TestProviderCostRate_ExponentOutOfRangeIsNamed(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newProviderCostAuditFixture(t)
	for _, raw := range []string{`1e9999999999`, `"1e-9999999999"`, `0e9999999999`} {
		rec, _ := f.serve(t, nil, http.MethodPut, "/", costBody(raw))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "exponent is out of range") {
			t.Errorf("%s: status %d body %s, want 422 naming the exponent", raw, rec.Code, rec.Body.String())
		}
	}
}

func TestProviderCostRate_LargeExponentsAnsweredAtOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: skipped in -short mode")
	}
	f := newProviderCostAuditFixture(t)
	for _, tc := range []struct {
		raw   string
		want  int
		bound time.Duration
	}{
		{`1e99999999`, http.StatusUnprocessableEntity, 200 * time.Millisecond},
		{`"1e-99999999"`, http.StatusUnprocessableEntity, 200 * time.Millisecond},
		{`1e2147483647`, http.StatusUnprocessableEntity, 200 * time.Millisecond},
		// Accepted, so it reaches Postgres: the bound allows the round trip.
		{`0e99999999`, http.StatusOK, time.Second},
	} {
		done := make(chan int, 1)
		go func() {
			rec, _ := f.serve(t, nil, http.MethodPut, "/", costBody(tc.raw))
			done <- rec.Code
		}()
		select {
		case code := <-done:
			if code != tc.want {
				t.Errorf("%s: status %d, want %d", tc.raw, code, tc.want)
			}
		case <-time.After(tc.bound):
			t.Fatalf("%s: not answered within %v", tc.raw, tc.bound)
		}
	}
}
