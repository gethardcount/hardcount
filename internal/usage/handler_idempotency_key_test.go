package usage

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// #980: an event without an idempotency_key could not be told apart from its
// own retry, so a client that missed the response and re-sent stored and
// billed the usage twice. Every public ingest door now refuses such an event
// with 422 before it looks anything up or writes anything.
//
// Mutation-verify: delete the idempotency_key check in resolve → every case
// here goes red; replace strings.TrimSpace(key) == "" with key == "" → the
// whitespace cases go red.

const keyedEvent = `{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":"evt_1"}`

func TestUsageIngest_MissingIdempotencyKeyIs422(t *testing.T) {
	bodies := map[string]string{
		"absent":     `{"external_customer_id":"acme","event_name":"api_call","quantity":1}`,
		"empty":      `{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":""}`,
		"whitespace": `{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":"  \t "}`,
	}
	for name, body := range bodies {
		for _, door := range []string{"single", "backfill"} {
			t.Run(door+"/"+name, func(t *testing.T) {
				store := newMemStore()
				cust := &stubCustomers{}
				h := NewHandler(NewService(store), cust, &stubMeters{})
				fn := h.ingest
				if door == "backfill" {
					fn = h.Backfill
				}
				rr := usageReq(t, fn, body)
				if rr.Code != http.StatusUnprocessableEntity {
					t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
				}
				var resp struct {
					Error struct {
						Param string `json:"param"`
					} `json:"error"`
				}
				_ = json.Unmarshal(rr.Body.Bytes(), &resp)
				if resp.Error.Param != "idempotency_key" {
					t.Errorf("param = %q, want idempotency_key; body %s", resp.Error.Param, rr.Body.String())
				}
				if len(store.events) != 0 {
					t.Errorf("%d events stored, want 0", len(store.events))
				}
				if cust.calls != 0 {
					t.Errorf("customer lookups = %d, want 0 (the key is checked before any lookup)", cust.calls)
				}
			})
		}
	}
}

// A batch is all-or-nothing: one keyless event among keyed ones refuses the
// whole batch, and errors[] names every keyless index and only those.
func TestUsageBatch_KeylessEventRefusesWholeBatch(t *testing.T) {
	store := newMemStore()
	h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})

	body := `[` +
		`{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":"evt_a"},` +
		`{"external_customer_id":"acme","event_name":"api_call","quantity":1},` +
		`{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":"evt_c"},` +
		`{"external_customer_id":"acme","event_name":"api_call","quantity":1,"idempotency_key":"   "}` +
		`]`
	rr := usageReq(t, h.batchIngest, body)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d, want 422; body %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body %s", err, rr.Body.String())
	}
	if resp.Error.Code != "batch_rejected" {
		t.Errorf("code = %q, want batch_rejected", resp.Error.Code)
	}
	if len(resp.Errors) != 2 ||
		!strings.HasPrefix(resp.Errors[0], "event[1]: idempotency_key") ||
		!strings.HasPrefix(resp.Errors[1], "event[3]: idempotency_key") {
		t.Errorf("errors = %q, want exactly event[1] and event[3] naming idempotency_key", resp.Errors)
	}
	if len(store.events) != 0 {
		t.Errorf("%d events stored, want 0 (all-or-nothing)", len(store.events))
	}
}

// The key check runs before the lookups, so a keyless event is refused as
// 422 even while the lookup store is down. A 503 would tell the client to
// retry an event that can never be accepted.
func TestUsageIngest_KeylessIs422EvenWhenLookupStoreIsDown(t *testing.T) {
	keyless := `{"external_customer_id":"acme","event_name":"api_call","quantity":1}`
	h := NewHandler(NewService(newMemStore()), &stubCustomers{err: errLookupInfra}, &stubMeters{err: errLookupInfra})
	doors := map[string]struct {
		fn   func(http.ResponseWriter, *http.Request)
		body string
	}{
		"single":   {h.ingest, keyless},
		"batch":    {h.batchIngest, "[" + keyless + "]"},
		"backfill": {h.Backfill, keyless},
	}
	for door, d := range doors {
		t.Run(door, func(t *testing.T) {
			rr := usageReq(t, d.fn, d.body)
			if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "idempotency_key") {
				t.Fatalf("status %d body %s, want 422 naming idempotency_key", rr.Code, rr.Body.String())
			}
		})
	}
}

// Control: the same event with a key is accepted, so the 422s above come
// from the missing key and nothing else.
func TestUsageIngest_KeyedEventAccepted(t *testing.T) {
	store := newMemStore()
	h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
	if rr := usageReq(t, h.ingest, keyedEvent); rr.Code != http.StatusCreated {
		t.Fatalf("single: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	if rr := usageReq(t, h.batchIngest, `[`+strings.Replace(keyedEvent, "evt_1", "evt_2", 1)+`]`); rr.Code != http.StatusCreated {
		t.Fatalf("batch: status %d, want 201; body %s", rr.Code, rr.Body.String())
	}
	if len(store.events) != 2 {
		t.Errorf("%d events stored, want 2", len(store.events))
	}
}

// A key past the limit would fail the UNIQUE index with a 5xx, which tells an
// ingest client to retry forever. It is a 422 instead; the limit itself, and a
// multi-byte key at the limit, are accepted.
func TestUsageIngest_IdempotencyKeyLengthLimit(t *testing.T) {
	keyed := func(k string) string {
		return strings.Replace(keyedEvent, `"evt_1"`, `"`+k+`"`, 1)
	}
	for name, tc := range map[string]struct {
		key  string
		want int
	}{
		"255 ascii":     {strings.Repeat("k", MaxIdempotencyKeyLen), http.StatusCreated},
		"255 multibyte": {strings.Repeat("é", MaxIdempotencyKeyLen), http.StatusCreated},
		"256":           {strings.Repeat("k", MaxIdempotencyKeyLen+1), http.StatusUnprocessableEntity},
		"3000":          {strings.Repeat("k", 3000), http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			store := newMemStore()
			h := NewHandler(NewService(store), &stubCustomers{}, &stubMeters{})
			rr := usageReq(t, h.ingest, keyed(tc.key))
			if rr.Code != tc.want {
				t.Fatalf("status %d, want %d; body %s", rr.Code, tc.want, rr.Body.String())
			}
			if tc.want == http.StatusUnprocessableEntity && !strings.Contains(rr.Body.String(), `"param":"idempotency_key"`) {
				t.Errorf("422 does not name idempotency_key: %s", rr.Body.String())
			}
		})
	}
}
