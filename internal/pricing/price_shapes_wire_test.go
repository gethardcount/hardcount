package pricing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The two routes that take a price, POST /v1/rating-rules and POST
// /v1/price-overrides, take ADR-130 §3's words: mode per_unit with
// unit_amount_cents, graduated tiers whose last up_to is null, and package
// with a decimal package_amount_cents. The old words are refused by name. A
// tier table is checked when it is saved, so one the API accepts always rates.
// A package amount takes the limits of every request decimal (#1378's
// domain.ParseDecimal and domain.CheckRate).
//
// Mutation-verify: let the last tier carry a bound (domain.CheckTiers) →
// TestPriceShapes_TierTableCheckedWhenSaved goes red.

// shapeDoor posts one price body to one of the two routes, on a fresh store.
type shapeDoor struct {
	name string
	send func(t *testing.T, price string) *httptest.ResponseRecorder
}

var shapeDoors = []shapeDoor{
	{
		name: "rating rule",
		send: func(t *testing.T, price string) *httptest.ResponseRecorder {
			h := NewHandler(NewService(newMemStore()))
			return doJSON(t, h.createRatingRule, http.MethodPost, "/", `{"rule_key":"r","name":"r","currency":"USD",`+price+`}`, nil)
		},
	},
	{
		name: "price override",
		send: func(t *testing.T, price string) *httptest.ResponseRecorder {
			h := NewHandler(NewService(newMemStore()))
			rr := doJSON(t, h.createRatingRule, http.MethodPost, "/", `{"rule_key":"r","name":"r","currency":"USD","mode":"per_unit","unit_amount_cents":"1"}`, nil)
			if rr.Code != http.StatusCreated {
				t.Fatalf("seed rule: status %d, body %s", rr.Code, rr.Body.String())
			}
			var rule struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &rule); err != nil {
				t.Fatalf("decode rule: %v", err)
			}
			return doJSON(t, h.createOverride, http.MethodPost, "/", `{"customer_id":"vlx_cus_1","rating_rule_version_id":"`+rule.ID+`",`+price+`}`, nil)
		},
	},
}

// shapeOut is the price part of a rule or an override on the wire, with each
// tier's up_to kept raw so a null is told apart from a missing key.
type shapeOut struct {
	Mode               string                       `json:"mode"`
	UnitAmountCents    string                       `json:"unit_amount_cents"`
	Tiers              []map[string]json.RawMessage `json:"tiers"`
	PackageSize        int64                        `json:"package_size"`
	PackageAmountCents string                       `json:"package_amount_cents"`
}

func TestPriceShapes_RoutesTakeTheNewWords(t *testing.T) {
	for _, door := range shapeDoors {
		for _, tc := range []struct {
			name, price string
			check       func(t *testing.T, got shapeOut)
		}{
			{
				name:  "per_unit",
				price: `"mode":"per_unit","unit_amount_cents":"0.0035"`,
				check: func(t *testing.T, got shapeOut) {
					if got.Mode != "per_unit" || got.UnitAmountCents != "0.0035" {
						t.Errorf("got mode %q unit_amount_cents %q, want per_unit 0.0035", got.Mode, got.UnitAmountCents)
					}
				},
			},
			{
				name:  "graduated, the last tier open",
				price: `"mode":"graduated","tiers":[{"up_to":100,"unit_amount_cents":"5"},{"up_to":null,"unit_amount_cents":"1.25"}]`,
				check: func(t *testing.T, got shapeOut) {
					if got.Mode != "graduated" || len(got.Tiers) != 2 {
						t.Fatalf("got mode %q with %d tiers, want graduated with 2", got.Mode, len(got.Tiers))
					}
					for i, want := range []struct{ upTo, unit string }{{"100", `"5"`}, {"null", `"1.25"`}} {
						if up, ok := got.Tiers[i]["up_to"]; !ok || string(up) != want.upTo {
							t.Errorf("tier %d up_to %s (present %v), want %s", i+1, up, ok, want.upTo)
						}
						if string(got.Tiers[i]["unit_amount_cents"]) != want.unit {
							t.Errorf("tier %d unit_amount_cents %s, want %s", i+1, got.Tiers[i]["unit_amount_cents"], want.unit)
						}
					}
				},
			},
			{
				name:  "package at a decimal amount",
				price: `"mode":"package","package_size":1000,"package_amount_cents":"0.2"`,
				check: func(t *testing.T, got shapeOut) {
					if got.Mode != "package" || got.PackageSize != 1000 || got.PackageAmountCents != "0.2" {
						t.Errorf("got mode %q size %d amount %q, want package 1000 0.2", got.Mode, got.PackageSize, got.PackageAmountCents)
					}
				},
			},
			{
				name:  "package, the amount sent as a JSON number",
				price: `"mode":"package","package_size":100,"package_amount_cents":120.5`,
				check: func(t *testing.T, got shapeOut) {
					if got.PackageAmountCents != "120.5" {
						t.Errorf("got package_amount_cents %q, want 120.5", got.PackageAmountCents)
					}
				},
			},
		} {
			t.Run(door.name+"/"+tc.name, func(t *testing.T) {
				rr := door.send(t, tc.price)
				if rr.Code != http.StatusCreated {
					t.Fatalf("status %d, want 201; body %s", rr.Code, rr.Body.String())
				}
				var got shapeOut
				if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
					t.Fatalf("decode: %v", err)
				}
				tc.check(t, got)
				for _, old := range []string{"flat_amount_cents", "graduated_tiers", "overage_unit_amount_cents"} {
					if strings.Contains(rr.Body.String(), old) {
						t.Errorf("the answer holds the old word %s: %s", old, rr.Body.String())
					}
				}
			})
		}
	}
}

func TestPriceShapes_OldWordsRefused(t *testing.T) {
	for _, door := range shapeDoors {
		for _, tc := range []struct{ price, param string }{
			{`"mode":"per_unit","flat_amount_cents":"1"`, "flat_amount_cents"},
			{`"mode":"graduated","graduated_tiers":[{"up_to":0,"unit_amount_cents":"1"}]`, "graduated_tiers"},
			{`"mode":"package","package_size":100,"package_amount_cents":"1","overage_unit_amount_cents":"1"`, "overage_unit_amount_cents"},
			{`"mode":"flat","unit_amount_cents":"1"`, "mode"},
		} {
			rr := door.send(t, tc.price)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s %s: status %d, want 422; body %s", door.name, tc.price, rr.Code, rr.Body.String())
				continue
			}
			if param, _ := errorParam(t, rr); param != tc.param {
				t.Errorf("%s %s: 422 names %q, want %q", door.name, tc.price, param, tc.param)
			}
		}
	}
}

func TestPriceShapes_TierTableCheckedWhenSaved(t *testing.T) {
	for _, door := range shapeDoors {
		for _, tc := range []struct{ name, tiers string }{
			{"a bounded last tier", `[{"up_to":100,"unit_amount_cents":"5"},{"up_to":1000,"unit_amount_cents":"1"}]`},
			{"one bounded tier", `[{"up_to":100,"unit_amount_cents":"5"}]`},
			{"the last tier at up_to 0", `[{"up_to":100,"unit_amount_cents":"5"},{"up_to":0,"unit_amount_cents":"1"}]`},
			{"an open tier before the last", `[{"up_to":null,"unit_amount_cents":"5"},{"up_to":100,"unit_amount_cents":"1"}]`},
			{"bounds that do not increase", `[{"up_to":100,"unit_amount_cents":"5"},{"up_to":100,"unit_amount_cents":"2"},{"up_to":null,"unit_amount_cents":"1"}]`},
			{"a first bound of 0", `[{"up_to":0,"unit_amount_cents":"5"},{"up_to":null,"unit_amount_cents":"1"}]`},
			{"no tiers", `[]`},
		} {
			rr := door.send(t, `"mode":"graduated","tiers":`+tc.tiers)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s, %s: status %d, want 422; body %s", door.name, tc.name, rr.Code, rr.Body.String())
				continue
			}
			// The refusal is about the table, not an unknown field.
			if param, msg := errorParam(t, rr); param != "tiers" || strings.HasPrefix(msg, "unknown field") {
				t.Errorf("%s, %s: 422 names %q (%s), want the tier table refused", door.name, tc.name, param, msg)
			}
		}
	}
}

func TestPriceShapes_PackageAmountTakesTheDecimalLimits(t *testing.T) {
	for _, door := range shapeDoors {
		for _, raw := range []string{
			`"0.0000000000001"`, // 13 decimal places
			`"-1"`,
			`"1000000000000000000"`, // 10^18
			`"abc"`,
			`true`,
		} {
			rr := door.send(t, `"mode":"package","package_size":100,"package_amount_cents":`+raw)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s %s: status %d, want 422; body %s", door.name, raw, rr.Code, rr.Body.String())
				continue
			}
			if param, _ := errorParam(t, rr); param != "package_amount_cents" {
				t.Errorf("%s %s: 422 names %q, want package_amount_cents", door.name, raw, param)
			}
		}
	}
}
