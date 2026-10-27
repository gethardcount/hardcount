package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// The three amounts of a plan price and a price override (unit_amount_cents,
// tiers[].unit_amount_cents, package_amount_cents) are request
// decimals with documented limits: a JSON number or a string holding one, at
// most 100 characters, 0 or more, under 10^18 cents, at most 12 decimal
// places. A value outside them is a 422 that names the field and nothing is
// stored. A rate the mode does not use must be 0
// (TestPriceShapes_UnusedFieldsRefused). A zero is stored as plain 0.
//
// Mutation-verify: drop the domain.CheckDecimal call in checkRate → the
// size, scale and zero cases go red; parse the rates with shopspring's
// UnmarshalJSON again → the type and exponent cases go red (400).

// rateField is one rate on the wire: frag sets it to raw inside a body that is
// otherwise valid, and pick reads it back from what was stored or answered.
type rateField struct {
	param string
	frag  func(raw string) string
	pick  func(r rates) decimal.Decimal
}

// rates is the three rates of a stored price or override, or of a response.
type rates struct {
	UnitAmountCents    decimal.Decimal     `json:"unit_amount_cents"`
	Tiers              []domain.RatingTier `json:"tiers"`
	PackageAmountCents decimal.Decimal     `json:"package_amount_cents"`
}

var rateFields = []rateField{
	{
		param: "unit_amount_cents",
		frag:  func(raw string) string { return `"mode":"per_unit","unit_amount_cents":` + raw },
		pick:  func(r rates) decimal.Decimal { return r.UnitAmountCents },
	},
	{
		param: "tiers[0].unit_amount_cents",
		frag: func(raw string) string {
			return `"mode":"graduated","tiers":[{"up_to":100,"unit_amount_cents":` + raw + `},{"up_to":null,"unit_amount_cents":"1"}]`
		},
		pick: func(r rates) decimal.Decimal { return r.Tiers[0].UnitAmountCents },
	},
	{
		param: "tiers[1].unit_amount_cents",
		frag: func(raw string) string {
			return `"mode":"graduated","tiers":[{"up_to":100,"unit_amount_cents":"1"},{"up_to":null,"unit_amount_cents":` + raw + `}]`
		},
		pick: func(r rates) decimal.Decimal { return r.Tiers[1].UnitAmountCents },
	},
	{
		param: "package_amount_cents",
		frag: func(raw string) string {
			return `"mode":"package","package_size":100,"package_amount_cents":` + raw
		},
		pick: func(r rates) decimal.Decimal { return r.PackageAmountCents },
	},
}

// rateDoor is one of the two create routes that take rates. send posts a body
// with frag in it to a fresh store and returns the response and the rates
// stored (ok=false when nothing was stored).
type rateDoor struct {
	name string
	send func(t *testing.T, frag string) (rr *httptest.ResponseRecorder, stored func(rateField) decimal.Decimal, ok bool)
}

var rateDoors = []rateDoor{
	{
		name: "plan price",
		send: func(t *testing.T, frag string) (*httptest.ResponseRecorder, func(rateField) decimal.Decimal, bool) {
			store := newMemStore()
			plan, seeded := seedPlanWithPrice(store, "tnt_test")
			h := NewHandler(NewService(store))
			rr := doJSON(t, h.addPrice, http.MethodPost, "/", `{"meter_id":"`+seeded.MeterID+`","dimension_match":{"model":"a"},`+frag+`}`, map[string]string{"id": plan.ID})
			for id, p := range store.prices {
				if id == seeded.ID {
					continue
				}
				return rr, func(f rateField) decimal.Decimal {
					return f.pick(rates{p.UnitAmountCents, p.Tiers, p.PackageAmountCents})
				}, true
			}
			return rr, nil, false
		},
	},
	{
		name: "price override",
		send: func(t *testing.T, frag string) (*httptest.ResponseRecorder, func(rateField) decimal.Decimal, bool) {
			store := newMemStore()
			_, price := seedPlanWithPrice(store, "tnt_test")
			h := NewHandler(NewService(store))
			rr := doJSON(t, h.createOverride, http.MethodPost, "/", `{"customer_id":"vlx_cus_1","price_id":"`+price.ID+`",`+frag+`}`, nil)
			for _, o := range store.overrides {
				return rr, func(f rateField) decimal.Decimal {
					return f.pick(rates{o.UnitAmountCents, o.Tiers, o.PackageAmountCents})
				}, true
			}
			return rr, nil, false
		},
	},
}

func errorParam(t *testing.T, rr *httptest.ResponseRecorder) (param, message string) {
	t.Helper()
	var env struct {
		Error struct {
			Param   string `json:"param"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error body: %v; body %s", err, rr.Body.String())
	}
	return env.Error.Param, env.Error.Message
}

func TestRates_ValidEdgesStoredAsSent(t *testing.T) {
	// raw → the value stored, written the way the API answers it.
	accepted := []struct{ raw, want string }{
		{`"0"`, "0"},
		{`"0.000000000001"`, "0.000000000001"},
		{`"999999999999999999.999999999999"`, "999999999999999999.999999999999"},
		{`"123456789012345678"`, "123456789012345678"},
		{`"0.0003"`, "0.0003"},
		{`2.25`, "2.25"},
		{`7`, "7"},
		// Not written in the canonical form: the value is kept.
		{`"1.500000000000000"`, "1.5"},
		{`1e3`, "1000"},
		{`"9.99999999999999999999999999999e17"`, "999999999999999999.999999999999"},
	}
	for _, door := range rateDoors {
		for _, f := range rateFields {
			for _, tc := range accepted {
				t.Run(door.name+"/"+f.param+"/"+tc.raw, func(t *testing.T) {
					rr, stored, ok := door.send(t, f.frag(tc.raw))
					if rr.Code != http.StatusCreated || !ok {
						t.Fatalf("status %d, want 201; body %s", rr.Code, rr.Body.String())
					}
					if got := stored(f).String(); got != tc.want {
						t.Errorf("stored %s, want %s", got, tc.want)
					}
					var answered rates
					if err := json.Unmarshal(rr.Body.Bytes(), &answered); err != nil {
						t.Fatalf("decode response: %v", err)
					}
					if got := f.pick(answered).String(); got != tc.want {
						t.Errorf("answered %s, want %s", got, tc.want)
					}
				})
			}
		}
	}
}

func TestRates_OutOfRangeRefusedNamingTheField(t *testing.T) {
	refused := []string{
		`"1000000000000000000"`, // 10^18
		`"1e18"`,
		`1e18`,
		`"1000000000000000000.5"`,
		`"0.0000000000001"`, // 13 places
		`"1.0000000000001"`,
		`0.1234567890123`,
		`"-1"`,
		`"-0.000000000001"`,
		`-5`,
		`"abc"`,
		`""`,
		`" 5"`,
		`true`,
		`{}`,
		`[1]`,
		`"1.` + strings.Repeat("0", 120) + `"`, // over 100 characters
	}
	for _, door := range rateDoors {
		for _, f := range rateFields {
			for _, raw := range refused {
				t.Run(door.name+"/"+f.param+"/"+raw, func(t *testing.T) {
					rr, _, ok := door.send(t, f.frag(raw))
					if rr.Code != http.StatusUnprocessableEntity {
						t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
					}
					if p, _ := errorParam(t, rr); p != f.param {
						t.Errorf("param %q, want %q; body %s", p, f.param, rr.Body.String())
					}
					if ok {
						t.Error("a refused rate was stored")
					}
				})
			}
		}
	}
}

func TestRates_ZeroStoredAsPlainZero(t *testing.T) {
	for _, raw := range []string{`"0e99999"`, `0e99999`, `"-0"`, `"0.000"`, `"-0e-99999"`} {
		for _, door := range rateDoors {
			for _, f := range rateFields {
				t.Run(door.name+"/"+f.param+"/"+raw, func(t *testing.T) {
					rr, stored, ok := door.send(t, f.frag(raw))
					if rr.Code != http.StatusCreated || !ok {
						t.Fatalf("status %d, want 201; body %s", rr.Code, rr.Body.String())
					}
					if d := stored(f); !d.IsZero() || d.Exponent() != 0 {
						t.Errorf("stored zero has exponent %d, want plain 0", d.Exponent())
					}
				})
			}
		}
	}
}

func TestRates_ExponentOutOfRangeIsNamed(t *testing.T) {
	for _, raw := range []string{`1e9999999999`, `"1e-9999999999"`, `0e9999999999`, `"1e99999999999999999999"`} {
		for _, door := range rateDoors {
			for _, f := range rateFields {
				t.Run(door.name+"/"+f.param+"/"+raw, func(t *testing.T) {
					rr, _, ok := door.send(t, f.frag(raw))
					if rr.Code != http.StatusUnprocessableEntity {
						t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
					}
					p, msg := errorParam(t, rr)
					if p != f.param || !strings.Contains(msg, "exponent is out of range") {
						t.Errorf("param %q message %q, want %q and the exponent named", p, msg, f.param)
					}
					if ok {
						t.Error("a refused rate was stored")
					}
				})
			}
		}
	}
}

// Exponents far outside the limits are refused; a zero with any exponent is
// stored.
func TestRates_ExtremeExponents(t *testing.T) {
	cases := []struct {
		raw  string
		want int
	}{
		{`1e99999999`, http.StatusUnprocessableEntity},
		{`"1e-99999999"`, http.StatusUnprocessableEntity},
		{`"-1e99999999"`, http.StatusUnprocessableEntity},
		{`0e99999999`, http.StatusCreated},
		{`"0e-99999999"`, http.StatusCreated},
		{`1e2147483647`, http.StatusUnprocessableEntity},
		{`"1e-2147483648"`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		for _, door := range rateDoors {
			for _, f := range rateFields {
				rr, _, _ := door.send(t, f.frag(tc.raw))
				if rr.Code != tc.want {
					t.Errorf("%s %s %s: status %d, want %d", door.name, f.param, tc.raw, rr.Code, tc.want)
				}
			}
		}
	}
}

// The recipe installer saves a plan and its prices through
// CreatePlanWithPricesTx, not the public route, so that entrypoint takes the
// same limits, and nothing is written when one is out.
//
// Mutation-verify: drop the priceAdds call in CreatePlanWithPricesTx → red.
func TestCreatePlanWithPricesTx_RatesTakeTheLimits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shape domain.PriceShape
	}{
		{"per_unit 13 places", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0.0000000000001")}},
		{"tier 10^18", domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{{UpTo: nil, UnitAmountCents: decimal.RequireFromString("1e18")}}}},
		{"negative package amount", domain.PriceShape{Mode: domain.PricingPackage, PackageSize: 10, PackageAmountCents: decimal.NewFromInt(-1)}},
	} {
		store := newMemStore()
		if _, _, err := NewService(store).CreatePlanWithPricesTx(context.Background(), nil, "tnt_test", recipePlan(), []domain.PlanPrice{{MeterID: "m", PriceShape: tc.shape}}); err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
		if len(store.plans) != 0 || len(store.prices) != 0 {
			t.Errorf("%s: %d plans and %d prices stored, want none", tc.name, len(store.plans), len(store.prices))
		}
	}
	// A zero is stored as plain 0 here too.
	_, got, err := NewService(newMemStore()).CreatePlanWithPricesTx(context.Background(), nil, "tnt_test", recipePlan(), []domain.PlanPrice{{
		MeterID: "m", PriceShape: domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: decimal.RequireFromString("0e99999")},
	}})
	if err != nil || len(got) != 1 || got[0].UnitAmountCents.Exponent() != 0 {
		t.Errorf("zero: got %+v, err %v; want plain 0", got, err)
	}
}

func recipePlan() domain.Plan {
	return domain.Plan{Code: "r", Name: "r", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive}
}

// The recipe installer's save entrypoint takes the public route's shape
// check too, so a price it saves always rates and carries only its mode's
// fields. The recipe parser checks the same before an install, but the
// save path does not rely on it.
//
// Mutation-verify: drop the checkShape call in priceAdd → red.
func TestCreatePlanWithPricesTx_ShapeChecked(t *testing.T) {
	upTo := func(n int64) *int64 { return &n }
	one := decimal.NewFromInt(1)
	open := []domain.RatingTier{{UpTo: upTo(100), UnitAmountCents: one}, {UpTo: nil, UnitAmountCents: one}}
	for _, tc := range []struct {
		name, param string
		shape       domain.PriceShape
	}{
		{"a bounded last tier", "prices[0].tiers", domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{{UpTo: upTo(100), UnitAmountCents: one}, {UpTo: upTo(1000), UnitAmountCents: one}}}},
		{"an open tier before the last", "prices[0].tiers", domain.PriceShape{Mode: domain.PricingGraduated, Tiers: []domain.RatingTier{{UpTo: nil, UnitAmountCents: one}, {UpTo: upTo(100), UnitAmountCents: one}}}},
		{"graduated with no tiers", "prices[0].tiers", domain.PriceShape{Mode: domain.PricingGraduated}},
		{"a package of size 0", "prices[0].package_size", domain.PriceShape{Mode: domain.PricingPackage, PackageAmountCents: one}},
		{"per_unit with tiers", "prices[0].tiers", domain.PriceShape{Mode: domain.PricingPerUnit, UnitAmountCents: one, Tiers: open}},
		{"package with a unit amount", "prices[0].unit_amount_cents", domain.PriceShape{Mode: domain.PricingPackage, PackageSize: 10, PackageAmountCents: one, UnitAmountCents: one}},
		{"an unknown mode", "prices[0].mode", domain.PriceShape{Mode: "flat", UnitAmountCents: one}},
	} {
		store := newMemStore()
		_, _, err := NewService(store).CreatePlanWithPricesTx(context.Background(), nil, "tnt_test", recipePlan(), []domain.PlanPrice{{MeterID: "m", PriceShape: tc.shape}})
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
		} else if field := errs.Field(err); field != tc.param {
			t.Errorf("%s: refused naming %q (%v), want %q", tc.name, field, err, tc.param)
		}
		if len(store.plans) != 0 || len(store.prices) != 0 {
			t.Errorf("%s: something was stored", tc.name)
		}
	}
}
