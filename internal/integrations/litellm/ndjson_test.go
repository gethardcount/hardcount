package litellm

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// liteLLMNDJSON is the body LiteLLM's generic_api logger sends with
// log_format ndjson: each record's JSON on its own line, joined by "\n", with
// no final newline ("\n".join(safe_dumps(log) for log in queue)).
func liteLLMNDJSON(t *testing.T, n int, customer string, recordBytes int) []byte {
	t.Helper()
	var records []json.RawMessage
	if err := json.Unmarshal(liteLLMBatch(t, n, customer, recordBytes), &records); err != nil {
		t.Fatalf("split batch: %v", err)
	}
	lines := make([][]byte, len(records))
	for i, r := range records {
		lines[i] = r
	}
	return bytes.Join(lines, []byte("\n"))
}

// TestSpend_NDJSONBatchIsAccepted: a full LiteLLM batch sent as NDJSON — 512
// records, each on its own line, some 4 MB — lands whole, as the same batch
// sent as a JSON array does. Before, a body with more than one record per
// send was refused with a 400 ("invalid JSON body"), which LiteLLM never
// sends again: every ndjson send of two or more calls was lost.
// Mutation-verify: read one object and refuse anything after it again →
// this fails with a 400.
func TestSpend_NDJSONBatchIsAccepted(t *testing.T) {
	const records = 512
	body := liteLLMNDJSON(t, records, "cus_acme_corp", 8400)
	if len(body) <= 4<<20 || bytes.Count(body, []byte("\n")) != records-1 {
		t.Fatalf("body is %d bytes over %d lines, want a 4 MB send of %d lines", len(body), bytes.Count(body, []byte("\n"))+1, records)
	}

	ing := &fakeIngester{}
	rec := postCapped(t, New(fakeCustomers{}, fakeMeters{}, ing, &fakeHolder{}), body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var resp SpendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	if resp.Accepted != 2*records || len(resp.Errors) != 0 || ing.calls != records {
		t.Fatalf("answer %+v after %d ingest calls, want %d accepted from %d calls", resp, ing.calls, 2*records, records)
	}
	seen := map[string]bool{}
	for _, call := range ing.got {
		for _, e := range call.Events {
			seen[e.IdempotencyKey] = true
		}
	}
	if len(seen) != 2*records {
		t.Fatalf("%d distinct idempotency keys written, want %d", len(seen), 2*records)
	}
}

// TestSpend_BadNDJSONLineIs400WithItsLine: a record that is not valid JSON,
// or not a record, refuses the whole send with a 400 naming its line and
// record, and nothing of the send is written. Lines are counted as they are:
// a blank line counts, so line and record can differ.
// Mutation-verify: drop the newlines still buffered from the count in
// recordFailed → the lines are wrong and this fails. Count the offending
// byte from the body's first byte, not the decoder's → the rows with blank
// lines before the body fail.
func TestSpend_BadNDJSONLineIs400WithItsLine(t *testing.T) {
	ok := func(id string) string {
		b, _ := json.Marshal(spendPayload(id))
		return string(b)
	}
	for _, c := range []struct {
		name, body, want string
	}{
		{"a line that is not JSON", ok("a") + "\n" + ok("b") + "\nnot json\n" + ok("d"), "line 3 (record 3)"},
		{"a truncated record on the last line", ok("a") + "\n" + ok("b") + "\n{\"id\":\"c\",", "line 3 (record 3)"},
		{"an id given as a number", ok("a") + "\n{\"id\":5,\"user\":\"cus_acme\"}\n" + ok("c"), "line 2 (record 2)"},
		{"after a blank line", ok("a") + "\n\n" + ok("b") + "\n{\"id\":\"c\" \"x\"}", "line 4 (record 3)"},
		{"an element of an array", "[\n" + ok("a") + ",\n" + ok("b") + ",\n{\"id\":}\n]", "line 4 (record 3)"},
		{"the first element of an array", "[\n{\"id\":}\n]", "line 2 (record 1)"},
		{"an element after a blank line", "[\n" + ok("a") + ",\n\n  {\"id\":}\n]", "line 4 (record 2)"},
		{"an element after a comma on its own line", "[\n" + ok("a") + "\n,\n{\"id\" x}\n]", "line 4 (record 2)"},
		{"an element that is not JSON", "[\n" + ok("a") + ",\nnot json\n]", "line 3 (record 2)"},
		{"blank lines before an array", "\n\n[\n" + ok("a") + ",\nnot json\n]", "line 5 (record 2)"},
		{"blank lines before NDJSON", "\n\n" + ok("a") + "\nnot json", "line 4 (record 2)"},
		{"a record over two lines, bad on its second", ok("a") + "\n{\"id\":\n\"b\" x}", "line 3 (record 2)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ing := &fakeIngester{}
			rec := postCapped(t, New(fakeCustomers{}, fakeMeters{}, ing, &fakeHolder{}), []byte(c.body))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.want) {
				t.Fatalf("answer %d %s, want 400 naming %s", rec.Code, rec.Body.String(), c.want)
			}
			if ing.calls != 0 {
				t.Fatalf("%d calls written from a refused send, want none", ing.calls)
			}
		})
	}
}

// TestSpend_NDJSONOverTheCapIs413: an NDJSON send over MaxBodyBytes gets the
// same 413 as an array does, and nothing of it is written.
func TestSpend_NDJSONOverTheCapIs413(t *testing.T) {
	body := liteLLMNDJSON(t, 512, "cus_acme_corp", 66<<10)
	if len(body) <= MaxBodyBytes {
		t.Fatalf("body is %d bytes, want over the %d-byte cap", len(body), MaxBodyBytes)
	}
	ing := &fakeIngester{}
	rec := postCapped(t, New(fakeCustomers{}, fakeMeters{}, ing, &fakeHolder{}), body)
	if rec.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rec.Body.String(), "batch_too_large") {
		t.Fatalf("answer %d %s, want 413 batch_too_large", rec.Code, rec.Body.String())
	}
	if ing.calls != 0 {
		t.Fatalf("%d calls written from a refused send, want none", ing.calls)
	}
}

// TestSpend_BadLineDeepInALargeSendIsNamed: in a full 4 MB send, the line
// named is the bad one, though the decoder has read past it and refilled its
// buffer many times by then.
// Mutation-verify: count the newlines read without those still in the
// decoder's buffer → the line is wrong and this fails.
func TestSpend_BadLineDeepInALargeSendIsNamed(t *testing.T) {
	lines := bytes.Split(liteLLMNDJSON(t, 512, "cus_acme_corp", 8400), []byte("\n"))
	lines[299] = []byte(`{"id":"broken", "user": }`)
	ing := &fakeIngester{}
	rec := postCapped(t, New(fakeCustomers{}, fakeMeters{}, ing, &fakeHolder{}), bytes.Join(lines, []byte("\n")))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "line 300 (record 300)") {
		t.Fatalf("answer %d %s, want 400 naming line 300 (record 300)", rec.Code, rec.Body.String())
	}
	if ing.calls != 0 {
		t.Fatalf("%d calls written from a refused send, want none", ing.calls)
	}
}
