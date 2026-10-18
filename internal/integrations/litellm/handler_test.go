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

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/usage"
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

func (f fakeMeters) GetMeterByKey(_ context.Context, _, key string) (domain.Meter, error) {
	if f.err != nil {
		return domain.Meter{}, f.err
	}
	return domain.Meter{ID: "met_" + key}, nil
}

type fakeIngester struct {
	errs  []error // consumed in order; nil = success
	calls int
}

func (f *fakeIngester) IngestResolvingHeld(_ context.Context, _ string, _ usage.IngestInput) (domain.UsageEvent, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return domain.UsageEvent{}, f.errs[i]
	}
	return domain.UsageEvent{ID: "evt"}, nil
}

type fakeHolder struct {
	err   error
	calls int
	got   []usage.HoldInput
}

func (f *fakeHolder) Hold(_ context.Context, _ string, in usage.HoldInput) (domain.HeldUsageEvent, error) {
	f.calls++
	f.got = append(f.got, in)
	if f.err != nil {
		return domain.HeldUsageEvent{}, f.err
	}
	return domain.HeldUsageEvent{ID: "held"}, nil
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
	return postSpendWithCtx(t, h, context.Background(), "vlx_ten_test", payloads...)
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
		"already held":       {New(fakeCustomers{err: errs.ErrNotFound}, fakeMeters{}, &fakeIngester{}, &fakeHolder{err: errs.ErrDuplicateKey}), false, 2, 0},
		"held row invalid":   {New(fakeCustomers{err: errs.ErrNotFound}, fakeMeters{}, &fakeIngester{}, &fakeHolder{err: errs.Invalid("quantity", "too large")}), true, 0, 0},
		"duplicate row":      {New(fakeCustomers{}, fakeMeters{}, &fakeIngester{errs: []error{errs.ErrDuplicateKey, errs.ErrDuplicateKey}}, &fakeHolder{}), false, 2, 0},
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
	if hold.calls != 2 {
		t.Fatalf("holder called %d times, want 2 (input + output)", hold.calls)
	}
	keys := map[string]bool{}
	for _, in := range hold.got {
		keys[in.IdempotencyKey] = true
		if in.Reason != domain.HeldReasonCustomerNotFound || in.ExternalCustomerID != "cus_acme" || in.MeterKey != MeterKeyTokens {
			t.Errorf("held row = %+v", in)
		}
		if in.EventAt == nil {
			t.Errorf("held row lost the payload time")
		}
	}
	if !keys["call-9:input"] || !keys["call-9:output"] {
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

// A batch whose first row commits and whose second hits the store failure
// is still refused whole: the committed row replays as deduplicated.
func TestSpend_MixedBatchIsRefusedWhole(t *testing.T) {
	ing := &fakeIngester{errs: []error{nil, nil, errInfra}}
	h := New(fakeCustomers{}, fakeMeters{}, ing, &fakeHolder{})
	code, _ := postSpend(t, h, spendPayload("a"), spendPayload("b"))
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 for a batch that could not be fully recorded", code)
	}
}
