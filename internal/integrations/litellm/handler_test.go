package litellm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/usage"
)

// --- fakes for the three narrow interfaces ---

type fakeCustomers struct{ err error }

func (f fakeCustomers) GetByExternalID(_ context.Context, _, ext string) (domain.Customer, error) {
	if f.err != nil {
		return domain.Customer{}, f.err
	}
	return domain.Customer{ID: "cus_int_" + ext}, nil
}

type fakeMeters struct{ err error }

func (f fakeMeters) GetMeterByEventName(_ context.Context, _, key string) (domain.Meter, error) {
	if f.err != nil {
		return domain.Meter{}, f.err
	}
	return domain.Meter{ID: "met_" + key}, nil
}

// fakeIngester answers IngestCall from errs (consumed one per call; nil is
// success). A success counts every event as inserted, or as deduplicated
// when dup is set.
type fakeIngester struct {
	errs  []error
	dup   bool
	calls int
	got   []usage.CallInput
}

func (f *fakeIngester) IngestCall(_ context.Context, _ string, in usage.CallInput) (usage.CallResult, error) {
	i := f.calls
	f.calls++
	f.got = append(f.got, in)
	if i < len(f.errs) && f.errs[i] != nil {
		return usage.CallResult{}, f.errs[i]
	}
	if f.dup {
		return usage.CallResult{Deduplicated: len(in.Events)}, nil
	}
	return usage.CallResult{Inserted: len(in.Events)}, nil
}

type fakeHolder struct {
	err   error
	dup   bool
	calls int
	got   []usage.HoldCallInput
}

func (f *fakeHolder) HoldCall(_ context.Context, _ string, in usage.HoldCallInput) (usage.CallResult, error) {
	f.calls++
	f.got = append(f.got, in)
	if f.err != nil {
		return usage.CallResult{}, f.err
	}
	if f.dup {
		return usage.CallResult{Deduplicated: len(in.Events)}, nil
	}
	return usage.CallResult{Inserted: len(in.Events)}, nil
}

func spendPayload(id string) StandardLoggingPayload {
	return StandardLoggingPayload{
		ID: id, CallType: "completion", Model: "claude-3-5-sonnet-20241022",
		CustomLLMProvider: "anthropic", User: "cus_acme",
		Usage:     &Usage{PromptTokens: 1200, CompletionTokens: 350, TotalTokens: 1550},
		StartTime: 1700000000.1, EndTime: 1700000003.456,
	}
}

func postSpend(t *testing.T, h *Handler, payloads ...StandardLoggingPayload) (int, string) {
	t.Helper()
	return postSpendWithCtx(t, h, context.Background(), "hc_ten_test", payloads...)
}

func postSpendWithCtx(t *testing.T, h *Handler, ctx context.Context, tenantID string, payloads ...StandardLoggingPayload) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"events": payloads})
	req := httptest.NewRequest(http.MethodPost, "/spend", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(auth.WithTenantID(ctx, tenantID))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

var errInfra = errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

// TestSpend_StoreUnavailableIs503 pins HA-10 (2026-08-30): a usage store
// that cannot be reached is a NON-verdict — the batch is refused with 503
// so LiteLLM's retry re-sends it. Before, this was a 200 with the failure
// filed as a per-row error, which LiteLLM read as delivered and discarded.
// Fail-fast: the second payload of the batch is never attempted. The raw
// driver error never reaches the body (ADR-026).
// Mutation-verify: drop the errIngestUnavailable arm in spend → 200.
func TestSpend_StoreUnavailableIs503(t *testing.T) {
	ing := &fakeIngester{errs: []error{errInfra, nil}}
	h := New(fakeCustomers{}, fakeMeters{}, ing, &fakeHolder{})
	code, body := postSpend(t, h, spendPayload("a"), spendPayload("b"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503; body %s", code, body)
	}
	if !strings.Contains(body, "ingest_unavailable") {
		t.Fatalf("body should carry code ingest_unavailable: %s", body)
	}
	if strings.Contains(body, "5432") || strings.Contains(body, "connection refused") {
		t.Fatalf("driver error leaked to the caller: %s", body)
	}
	if ing.calls != 1 {
		t.Fatalf("fail-fast: ingester called %d times for a 2-payload batch, want 1", ing.calls)
	}
}

// The lookup stores return errs.ErrNotFound only on sql.ErrNoRows; any
// other error is infrastructure and must not masquerade as "not found".
func TestSpend_LookupInfraErrorsAre503NotMisconfig(t *testing.T) {
	for name, h := range map[string]*Handler{
		"customer": New(fakeCustomers{err: errInfra}, fakeMeters{}, &fakeIngester{}, &fakeHolder{}),
		"meter":    New(fakeCustomers{}, fakeMeters{err: errInfra}, &fakeIngester{}, &fakeHolder{}),
	} {
		code, body := postSpend(t, h, spendPayload("a"))
		if code != http.StatusServiceUnavailable {
			t.Errorf("%s infra error: status %d, want 503; body %s", name, code, body)
		}
		if strings.Contains(body, "not found") {
			t.Errorf("%s infra error masqueraded as misconfiguration: %s", name, body)
		}
	}
}

// Negative controls: verdicts stay 200 — an unmapped customer or meter
// (now held, #1044), a duplicate row, and a payload that fails validation
// must never trigger a retry loop.
func TestSpend_VerdictsStay200(t *testing.T) {
	cases := map[string]struct {
		h        *Handler
		wantErr  bool
		wantDup  int
		wantHeld int
	}{
		"customer not found": {New(fakeCustomers{err: errs.ErrNotFound}, fakeMeters{}, &fakeIngester{}, &fakeHolder{}), false, 0, 2},
		"meter not found":    {New(fakeCustomers{}, fakeMeters{err: errs.ErrNotFound}, &fakeIngester{}, &fakeHolder{}), false, 0, 2},
		"already held":       {New(fakeCustomers{err: errs.ErrNotFound}, fakeMeters{}, &fakeIngester{}, &fakeHolder{dup: true}), false, 2, 0},
		"held row invalid":   {New(fakeCustomers{err: errs.ErrNotFound}, fakeMeters{}, &fakeIngester{}, &fakeHolder{err: errs.Invalid("quantity", "too large")}), true, 0, 0},
		"duplicate row":      {New(fakeCustomers{}, fakeMeters{}, &fakeIngester{dup: true}, &fakeHolder{}), false, 2, 0},
		"validation":         {New(fakeCustomers{}, fakeMeters{}, &fakeIngester{errs: []error{errs.Invalid("quantity", "negative")}}, &fakeHolder{}), true, 0, 0},
	}
	for name, tc := range cases {
		code, body := postSpend(t, tc.h, spendPayload("a"))
		if code != http.StatusOK {
			t.Errorf("%s: status %d, want 200 (a verdict must not make LiteLLM retry); body %s", name, code, body)
			continue
		}
		var resp SpendResponse
		_ = json.Unmarshal([]byte(body), &resp)
		if tc.wantErr != (len(resp.Errors) > 0) {
			t.Errorf("%s: errors=%v, want errors present=%v", name, resp.Errors, tc.wantErr)
		}
		if resp.Deduplicated != tc.wantDup {
			t.Errorf("%s: deduplicated=%d want %d", name, resp.Deduplicated, tc.wantDup)
		}
		if resp.Held != tc.wantHeld {
			t.Errorf("%s: held=%d want %d", name, resp.Held, tc.wantHeld)
		}
	}
}

// #1044: a row for a customer that does not exist is held with the key,
// reason and time it would have been ingested with — never just dropped.
// Mutation-verify: restore the old "customer %q not found" return in
// persist → held=0 and this fails.
func TestSpend_UnknownCustomerIsHeldWithItsKey(t *testing.T) {
	hold := &fakeHolder{}
	h := New(fakeCustomers{err: errs.ErrNotFound}, fakeMeters{}, &fakeIngester{}, hold)
	code, body := postSpend(t, h, spendPayload("call-9"))
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	if hold.calls != 1 {
		t.Fatalf("holder called %d times, want 1 (one call, held whole)", hold.calls)
	}
	in := hold.got[0]
	if in.Reason != domain.HeldReasonCustomerNotFound || in.ExternalCustomerID != "cus_acme" || in.EventName != EventNameTokens {
		t.Errorf("held call = %+v", in)
	}
	keys := map[string]bool{}
	for _, ev := range in.Events {
		keys[ev.IdempotencyKey] = true
		if ev.Timestamp == nil {
			t.Errorf("held row lost the payload time")
		}
	}
	if !keys["call-9:input"] || !keys["call-9:output"] || len(keys) != 2 {
		t.Fatalf("held keys %v, want the mapper's call-9:input and call-9:output", keys)
	}
}

// A hold that cannot be written is a NON-verdict: 503 so LiteLLM resends,
// never a 200 that reads as delivered (the bug #1044 closes, by another
// route).
func TestSpend_HoldStoreFailureIs503(t *testing.T) {
	h := New(fakeCustomers{err: errs.ErrNotFound}, fakeMeters{}, &fakeIngester{}, &fakeHolder{err: errInfra})
	code, body := postSpend(t, h, spendPayload("a"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503; body %s", code, body)
	}
	if strings.Contains(body, "5432") {
		t.Fatalf("driver error leaked: %s", body)
	}
}

// A payload without an id stays an error, never held: its rows would have
// no key, and a resend would bill them twice.
func TestSpend_MissingIDIsNotHeld(t *testing.T) {
	hold := &fakeHolder{}
	h := New(fakeCustomers{err: errs.ErrNotFound}, fakeMeters{}, &fakeIngester{}, hold)
	p := spendPayload("")
	code, body := postSpend(t, h, p)
	if code != http.StatusOK || hold.calls != 0 || !strings.Contains(body, "payload.id is required") {
		t.Fatalf("status %d holder calls %d body %s", code, hold.calls, body)
	}
}

// A batch whose first call commits and whose second hits the store failure
// is still refused whole: the committed call replays as deduplicated.
func TestSpend_MixedBatchIsRefusedWhole(t *testing.T) {
	ing := &fakeIngester{errs: []error{nil, errInfra}}
	h := New(fakeCustomers{}, fakeMeters{}, ing, &fakeHolder{})
	code, _ := postSpend(t, h, spendPayload("a"), spendPayload("b"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 for a batch that could not be fully recorded", code)
	}
}

// A LiteLLM response-cache hit and a failed call bill nothing, whatever
// tokens they carry. Both are skipped calls, never errors, and the hit is
// also counted in cache_hits so an operator can reconcile against LiteLLM's
// own spend. A failed call with no user is skipped too: the failure check
// runs before the user check.
// Mutation-verify: drop the cache_hits count in spend → cache_hits 0; move
// the failure check after the user check in MapPayload → an error row.
func TestSpend_CacheHitAndFailureAreSkipped(t *testing.T) {
	ing, hold := &fakeIngester{}, &fakeHolder{}
	h := New(fakeCustomers{}, fakeMeters{}, ing, hold)
	yes := true
	hit := spendPayload("hit-1")
	hit.CacheHit = &yes
	failed := spendPayload("fail-1")
	failed.Status = "failure"
	anonFailed := spendPayload("fail-2")
	anonFailed.Status = "failure"
	anonFailed.User = ""
	anonFailed.Usage = &Usage{}

	code, body := postSpend(t, h, hit, failed, anonFailed, spendPayload("ok-1"))
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	var resp SpendResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Skipped != 3 || resp.CacheHits != 1 || resp.Accepted != 2 || len(resp.Errors) != 0 {
		t.Fatalf("response = %+v, want skipped 3, cache_hits 1, accepted 2 (the one real call), no errors", resp)
	}
	if ing.calls != 1 || hold.calls != 0 {
		t.Fatalf("ingester calls %d, holder calls %d, want 1 and 0", ing.calls, hold.calls)
	}
	if !strings.Contains(body, `"cache_hits":1`) {
		t.Fatalf("body lacks cache_hits: %s", body)
	}
}
