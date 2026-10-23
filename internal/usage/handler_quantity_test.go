package usage

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The ingest body follows the spec exactly (B5):
//   - quantity is required: a JSON number, or a string holding one;
//   - it may be negative (a correction);
//   - it has at most 12 decimal places (the column's scale), so no stored
//     value is ever rounded;
//   - its magnitude is below 10^26;
//   - `properties` is not a field (its new name is `dimensions`).
//
// Anything else is a 422 that names the field, at every door, and a batch
// names every failing index. Before B5 a missing quantity was stored as 0,
// a bad type was a 400 for the whole body, extra decimals were rounded by
// the column, and `properties` was an alias.
//
// Mutation-verify: drop the required check in parseQuantity → the absent and
// null cases go red; drop the scale check → the 13-decimal cases go red;
// drop the properties refusal → the properties cases go red.

// quantityBody is one event with quantity set to raw (a JSON fragment), or
// without a quantity when raw is empty.
func quantityBody(raw, key string) string {
	q := ""
	if raw != "" {
		q = `"quantity":` + raw + `,`
	}
	return `{"external_customer_id":"acme","event_name":"api_call",` + q +
		`"idempotency_key":"` + key + `","timestamp":"2026-01-02T03:04:05Z"}`
}

// quantityDoors are the three public ingest doors. A batch wraps the event
// in an array.
func quantityDoors(t *testing.T, h *Handler) map[string]func(body string) (int, string) {
	return map[string]func(string) (int, string){
		"single": func(b string) (int, string) {
			rr := usageReq(t, h.ingest, b)
			return rr.Code, rr.Body.String()
		},
		"batch": func(b string) (int, string) {
			rr := usageReq(t, h.batchIngest, "["+b+"]")
			return rr.Code, rr.Body.String()
		},
		"backfill": func(b string) (int, string) {
			rr := usageReq(t, h.Backfill, b)
			return rr.Code, rr.Body.String()
		},
	}
}

func TestUsageIngest_QuantityRefusedAsTheSpecSays(t *testing.T) {
	refused := map[string]string{
		"absent":             "",
		"null":               `null`,
		"bool":               `true`,
		"object":             `{}`,
		"array":              `[1]`,
		"empty string":       `""`,
		"not a number":       `"abc"`,
		"padded string":      `" 5"`,
		"13 decimals number": `0.1234567890123`,
		"13 decimals string": `"0.1234567890123"`,
		"float artifact":     `0.30000000000000004`,
		"10^26":              `"100000000000000000000000000"`,
		// shopspring's NumDigits counts 10^15..10^15+2 as 15 digits (a float
		// log10), so a size check built on it let these through to the column.
		"over 10^26, exponent number": `1.000000000000001e26`,
		"over 10^26, exponent string": `"1.000000000000001e26"`,
		"10^26 as 10^15 × 10^11":      `1000000000000000e11`,
		"huge exponent":               `1e999999999`,
		"tiny exponent":               `"1e-999999999"`,
	}
	for name, raw := range refused {
		store := newMemStore()
		h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
		for door, send := range quantityDoors(t, h) {
			t.Run(door+"/"+name, func(t *testing.T) {
				code, body := send(quantityBody(raw, "evt_q_"+door))
				if code != http.StatusUnprocessableEntity {
					t.Fatalf("status %d, want 422; body %s", code, body)
				}
				if door == "batch" {
					if !strings.Contains(body, `event[0]: quantity`) {
						t.Errorf("batch errors[] does not name event[0]: quantity: %s", body)
					}
				} else if !strings.Contains(body, `"param":"quantity"`) {
					t.Errorf("422 does not name quantity: %s", body)
				}
			})
		}
		if len(store.events) != 0 {
			t.Errorf("%s: %d events stored, want 0", name, len(store.events))
		}
	}
}

func TestUsageIngest_QuantityAcceptedAsTheSpecSays(t *testing.T) {
	accepted := map[string]string{
		"integer":                     `5`,
		"integer string":              `"5"`,
		"decimal string":              `"5.5"`,
		"decimal number":              `2.25`,
		"zero":                        `0`,
		"negative correction":         `-3`,
		"negative decimal string":     `"-2.5"`,
		"12 decimals":                 `"0.123456789012"`,
		"trailing zeros past 12":      `"1.5000000000000"`,
		"trailing zeros to 27 places": `"0.000000000001000000000000000"`,
		"just under 10^26, exponent":  `9.99999999999999e25`,
		"exponent":                    `1e3`,
		"largest integer under 1e26":  `"99999999999999999999999999"`,
	}
	for name, raw := range accepted {
		want, err := decimal.NewFromString(strings.Trim(raw, `"`))
		if err != nil {
			t.Fatalf("%s: bad fixture: %v", name, err)
		}
		for _, door := range []string{"single", "backfill"} {
			t.Run(door+"/"+name, func(t *testing.T) {
				store := newMemStore()
				h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
				code, body := quantityDoors(t, h)[door](quantityBody(raw, "evt_ok"))
				if code != http.StatusCreated {
					t.Fatalf("status %d, want 201; body %s", code, body)
				}
				var got struct {
					Quantity decimal.Decimal `json:"quantity"`
				}
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatalf("decode: %v; body %s", err, body)
				}
				if !got.Quantity.Equal(want) {
					t.Errorf("stored quantity %s, want %s", got.Quantity, want)
				}
			})
		}
	}
}

// One bad quantity in a batch refuses the whole batch and errors[] names
// each bad index and only those.
func TestUsageBatch_BadQuantityNamesEachIndex(t *testing.T) {
	store := newMemStore()
	h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
	body := "[" + strings.Join([]string{
		quantityBody(`1`, "evt_b0"),
		quantityBody(`true`, "evt_b1"),
		quantityBody(`"2.5"`, "evt_b2"),
		quantityBody(``, "evt_b3"),
	}, ",") + "]"
	rr := usageReq(t, h.batchIngest, body)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Errors) != 2 || !strings.HasPrefix(resp.Errors[0], "event[1]: quantity") || !strings.HasPrefix(resp.Errors[1], "event[3]: quantity") {
		t.Errorf("errors = %q, want exactly event[1] and event[3] naming quantity", resp.Errors)
	}
	if len(store.events) != 0 {
		t.Errorf("%d events stored, want 0 (all-or-nothing)", len(store.events))
	}
}

// `properties` was the old name of `dimensions`. An event that still sends
// it is refused: storing it without its dimensions would price it by the
// wrong rule, silently.
func TestUsageIngest_PropertiesIsRefused(t *testing.T) {
	for _, extra := range []string{
		`"properties":{"model":"gpt-4o"}`,
		`"properties":{"model":"gpt-4o"},"dimensions":{"model":"gpt-4o"}`,
	} {
		store := newMemStore()
		h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
		event := strings.Replace(quantityBody(`1`, "evt_p"), `{`, `{`+extra+`,`, 1)
		for door, send := range quantityDoors(t, h) {
			t.Run(door+"/"+extra, func(t *testing.T) {
				code, body := send(event)
				if code != http.StatusUnprocessableEntity || !strings.Contains(body, "properties") || !strings.Contains(body, "dimensions") {
					t.Fatalf("status %d body %s, want 422 naming properties and dimensions", code, body)
				}
			})
		}
		if len(store.events) != 0 {
			t.Errorf("%d events stored, want 0", len(store.events))
		}
	}
}

// A null or empty properties carries no dimensions, so nothing would be
// priced by the wrong rule: the event is taken as if properties were absent.
// Clients generated from the old spec may send either.
func TestUsageIngest_EmptyPropertiesIsAccepted(t *testing.T) {
	for _, extra := range []string{
		`"properties":null`,
		`"properties":{}`,
		`"properties": { } `,
		`"properties":{},"dimensions":{"model":"gpt-4o"}`,
	} {
		for _, door := range []string{"single", "backfill", "batch"} {
			t.Run(door+"/"+extra, func(t *testing.T) {
				store := newMemStore()
				h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
				event := strings.Replace(quantityBody(`1`, "evt_pe"), `{`, `{`+extra+`,`, 1)
				code, body := quantityDoors(t, h)[door](event)
				if code != http.StatusCreated {
					t.Fatalf("status %d, want 201; body %s", code, body)
				}
				if len(store.events) != 1 {
					t.Fatalf("%d events stored, want 1", len(store.events))
				}
				if strings.Contains(extra, "dimensions") {
					for _, e := range store.events {
						if e.Dimensions["model"] != "gpt-4o" {
							t.Errorf("dimensions = %v, want model=gpt-4o", e.Dimensions)
						}
					}
				}
			})
		}
	}
}

// Control: dimensions is the field, and it is stored.
func TestUsageIngest_DimensionsStored(t *testing.T) {
	store := newMemStore()
	h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
	event := strings.Replace(quantityBody(`1`, "evt_d"), `{`, `{"dimensions":{"model":"gpt-4o"},`, 1)
	rr := usageReq(t, h.ingest, event)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	for _, e := range store.events {
		if e.Dimensions["model"] != "gpt-4o" {
			t.Errorf("dimensions = %v, want model=gpt-4o", e.Dimensions)
		}
	}
}

// A zero is stored as plain 0 (exponent 0), whatever exponent it was written
// with. shopspring keeps 0e9999999 as coefficient 0 and exponent 9999999. The
// size and scale checks pass a zero, so before B5 review round 2 any zero was
// stored as written.
//
// Mutation-verify: drop the zero normalisation in domain.CheckDecimal → every
// case here goes red.
func TestUsageIngest_ZeroIsStoredAsPlainZero(t *testing.T) {
	for _, raw := range []string{`0e9999999`, `"0e9999999"`, `-0e9999999`, `"0e-9999999"`, `0.000`} {
		for _, door := range []string{"single", "batch", "backfill"} {
			t.Run(door+"/"+raw, func(t *testing.T) {
				store := newMemStore()
				h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
				code, body := quantityDoors(t, h)[door](quantityBody(raw, "evt_zero"))
				if code != http.StatusCreated {
					t.Fatalf("status %d, want 201; body %s", code, body)
				}
				if len(store.events) != 1 {
					t.Fatalf("%d events stored, want 1", len(store.events))
				}
				for _, e := range store.events {
					if !e.Quantity.IsZero() || e.Quantity.Exponent() != 0 {
						t.Errorf("stored quantity has exponent %d, want plain 0", e.Quantity.Exponent())
					}
				}
			})
		}
	}
	// A held row takes the same check, so it is stored as plain 0 too.
	ev, err := heldEvent(HoldInput{
		ExternalCustomerID: "acme", EventName: "api_call", IdempotencyKey: "evt_held_zero",
		Quantity: decimal.RequireFromString("0e99999999"), Reason: domain.HeldReasonCustomerNotFound,
	})
	if err != nil {
		t.Fatalf("held zero refused: %v", err)
	}
	if !ev.Quantity.IsZero() || ev.Quantity.Exponent() != 0 {
		t.Errorf("held quantity has exponent %d, want plain 0", ev.Quantity.Exponent())
	}
}

// A JSON number whose exponent does not fit shopspring's int32 is a number,
// so the 422 says the exponent is out of range, not that it is not a number.
func TestUsageIngest_ExponentOutOfRangeIsNamed(t *testing.T) {
	for _, raw := range []string{`1e9999999999`, `"1e-9999999999"`, `0e9999999999`, `1e99999999999999999999`} {
		store := newMemStore()
		h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
		for door, send := range quantityDoors(t, h) {
			t.Run(door+"/"+raw, func(t *testing.T) {
				code, body := send(quantityBody(raw, "evt_exp_"+door))
				if code != http.StatusUnprocessableEntity {
					t.Fatalf("status %d, want 422; body %s", code, body)
				}
				if !strings.Contains(body, "exponent is out of range") || strings.Contains(body, "must be a JSON number") {
					t.Errorf("body %s, want the exponent named", body)
				}
			})
		}
		if len(store.events) != 0 {
			t.Errorf("%s: %d events stored, want 0", raw, len(store.events))
		}
	}
	// Not a JSON number: the old text stays.
	h := NewHandler(NewService(newMemStore()), &stubCustomers{}, &stubMeters{})
	for _, raw := range []string{`"abc"`, `" 5"`, `"1e"`} {
		if rr := usageReq(t, h.ingest, quantityBody(raw, "evt_nan")); !strings.Contains(rr.Body.String(), "must be a JSON number") {
			t.Errorf("%s: body %s, want the JSON-number text", raw, rr.Body.String())
		}
	}
}
