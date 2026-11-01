package litellm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
)

// liteLLMBatch builds the body LiteLLM's generic_api logger sends: a JSON
// array of n records. Each is the real proxy record in testdata, given its
// own id and the customer, with its messages padded so the record is about
// recordBytes long (a real record carries the call's messages and reply; the
// captured one was a ten-token call).
func liteLLMBatch(t *testing.T, n int, customer string, recordBytes int) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/litellm-1.83.9-generic-api.json")
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
	records := make([]map[string]any, n)
	for i := range records {
		rec := make(map[string]any, len(fixture[0]))
		for k, v := range fixture[0] {
			rec[k] = v
		}
		rec["id"] = fmt.Sprintf("chatcmpl-batch-%04d", i)
		rec["end_user"] = customer
		rec["messages"] = []map[string]any{{"role": "user", "content": pad}}
		records[i] = rec
	}
	body, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("encode batch: %v", err)
	}
	return body
}

// postCapped posts body to the handler with the request body capped at
// MaxBodyBytes, as the router caps this route (api limitRequestBody).
func postCapped(t *testing.T, h *Handler, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/spend", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithTenantID(req.Context(), "hc_ten_test"))
	rec := httptest.NewRecorder()
	req.Body = http.MaxBytesReader(rec, req.Body, MaxBodyBytes)
	h.Routes().ServeHTTP(rec, req)
	return rec
}

// TestSpend_FullDefaultLiteLLMBatchIsAccepted: a full LiteLLM batch — 512
// records (DEFAULT_BATCH_SIZE) of about 8 KB, some 4 MB — is read whole and
// every record is written. Under the old 1 MB cap for every route the body
// failed to read and the answer was a 400, which LiteLLM never resends: the
// whole batch was lost.
// Mutation-verify: set MaxBodyBytes back to 1<<20 → 413, nothing written.
func TestSpend_FullDefaultLiteLLMBatchIsAccepted(t *testing.T) {
	const records = 512
	body := liteLLMBatch(t, records, "cus_acme_corp", 8400)
	if len(body) <= 4<<20 {
		t.Fatalf("batch is %d bytes, want over 4 MB (a full default batch)", len(body))
	}

	ing := &fakeIngester{}
	hold := &fakeHolder{}
	rec := postCapped(t, New(fakeCustomers{}, fakeMeters{}, ing, hold), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp SpendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if resp.Accepted != 2*records || len(resp.Errors) != 0 || resp.Skipped != 0 || resp.Held != 0 {
		t.Fatalf("answer %+v, want %d accepted (input and output per call) and no errors", resp, 2*records)
	}
	if ing.calls != records || hold.calls != 0 {
		t.Fatalf("ingested %d calls and held %d, want %d ingested", ing.calls, hold.calls, records)
	}
	seen := map[string]bool{}
	for _, call := range ing.got {
		for _, e := range call.Events {
			seen[e.IdempotencyKey] = true
		}
	}
	if len(seen) != 2*records {
		t.Fatalf("%d distinct idempotency keys written, want %d: a record was dropped or merged", len(seen), 2*records)
	}
}

// TestSpend_BodyOverTheCapIs413: a body over MaxBodyBytes is refused with a
// 413 that says the batch was not saved and how to fix it, and no record of
// it is written — not even the ones read before the cap.
// Mutation-verify: drop the MaxBytesError arm in spend → 400 invalid JSON.
func TestSpend_BodyOverTheCapIs413(t *testing.T) {
	body := liteLLMBatch(t, 512, "cus_acme_corp", 66<<10)
	if len(body) <= MaxBodyBytes {
		t.Fatalf("batch is %d bytes, want over the %d-byte cap", len(body), MaxBodyBytes)
	}

	ing := &fakeIngester{}
	hold := &fakeHolder{}
	rec := postCapped(t, New(fakeCustomers{}, fakeMeters{}, ing, hold), body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413; body %s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if env.Error.Code != "batch_too_large" || !strings.Contains(env.Error.Message, "32 MB") || !strings.Contains(env.Error.Message, "not saved") {
		t.Fatalf("error %+v, want code batch_too_large naming the 32 MB limit and that nothing was saved", env.Error)
	}
	if ing.calls != 0 || hold.calls != 0 {
		t.Fatalf("ingested %d calls and held %d from a refused body, want none", ing.calls, hold.calls)
	}
}

// TestDecodeBody_Shapes pins the three body shapes the streaming decoder
// reads, and what it refuses.
func TestDecodeBody_Shapes(t *testing.T) {
	decode := func(body string) ([]StandardLoggingPayload, error) {
		req := httptest.NewRequest(http.MethodPost, "/spend", strings.NewReader(body))
		return decodeBody(req)
	}
	ids := func(ps []StandardLoggingPayload) string {
		out := make([]string, len(ps))
		for i, p := range ps {
			out[i] = p.ID
		}
		return strings.Join(out, ",")
	}

	for _, c := range []struct {
		name, body, want string
	}{
		{"bare array (LiteLLM's json_array)", `[{"id":"a"},{"id":"b"}]`, "a,b"},
		{"bare array after whitespace", " \n\t[{\"id\":\"a\"}]", "a"},
		{"empty array", `[]`, ""},
		{"single record", `{"id":"a","model":"m"}`, "a"},
		{"events wrapper", `{"events":[{"id":"a"},{"id":"b"}]}`, "a,b"},
		{"empty events is a single record", `{"id":"a","events":[]}`, "a"},
		{"NDJSON (LiteLLM's ndjson)", "{\"id\":\"a\"}\n{\"id\":\"b\"}\n{\"id\":\"c\"}", "a,b,c"},
		{"NDJSON with CRLF, a blank line and a final newline", "{\"id\":\"a\"}\r\n\r\n{\"id\":\"b\"}\r\n", "a,b"},
		{"objects back to back", `{"id":"a"}{"id":"b"}`, "a,b"},
		{"a record over two lines", "{\"id\":\n\"a\"}\n{\"id\":\"b\"}", "a,b"},
		{"an events wrapper, then a record", `{"events":[{"id":"a"},{"id":"b"}]} {"id":"c"}`, "a,b,c"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := decode(c.body)
			if err != nil {
				t.Fatalf("decode %q: %v", c.body, err)
			}
			if ids(got) != c.want {
				t.Fatalf("decode %q = ids %q, want %q", c.body, ids(got), c.want)
			}
		})
	}

	for _, c := range []struct {
		name, body string
	}{
		{"empty body", ``},
		{"whitespace only", " \n"},
		{"truncated array", `[{"id":"a"},{"id":`},
		{"not an object in the array", `[{"id":"a"},1]`},
		{"trailing data after the array", `[{"id":"a"}] [{"id":"b"}]`},
		{"a stray } after the objects", `{"id":"a"} }`},
		{"a stray ] after the objects", "{\"id\":\"a\"}\n]"},
		{"an array after an object", `{"id":"a"} [{"id":"b"}]`},
		{"not JSON", `id=a`},
	} {
		t.Run("refuses "+c.name, func(t *testing.T) {
			if got, err := decode(c.body); err == nil {
				t.Fatalf("decode %q = %d records, want an error", c.body, len(got))
			}
		})
	}

	if _, err := decode(``); !errors.Is(err, errEmptyBody) {
		t.Fatalf("empty body: err %v, want errEmptyBody", err)
	}
}
