package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request/requesttest"
)

func TestIdempotency_NoCaching_WithoutKey(t *testing.T) {
	callCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"new"}`))
	})

	// No idempotency key — should execute every time
	req := httptest.NewRequest("POST", "/v1/customers", strings.NewReader(`{}`))
	ctx := auth.WithTenantID(req.Context(), "t1")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if callCount != 1 {
		t.Errorf("expected 1 call, got %d", callCount)
	}
}

// TestFingerprintRequest holds what makes two requests the same request: the
// method, the path with its query string, the caller and the body. Equal
// inputs hash equal, and changing any one of them changes the hash.
func TestFingerprintRequest(t *testing.T) {
	type in struct {
		method, uri, caller string
		body                []byte
	}
	fp := func(i in) []byte { return fingerprintRequest(i.method, i.uri, i.caller, i.body) }
	base := in{"POST", "/v1/invoices?expand=lines", "key:key_a", []byte(`{"amount":100}`)}

	if !bytes.Equal(fp(base), fp(in{"POST", "/v1/invoices?expand=lines", "key:key_a", []byte(`{"amount":100}`)})) {
		t.Error("equal inputs must give equal fingerprints")
	}
	for _, c := range []struct {
		name string
		in   in
	}{
		{"another method", in{"PUT", base.uri, base.caller, base.body}},
		{"another path", in{base.method, "/v1/customers?expand=lines", base.caller, base.body}},
		{"another query string", in{base.method, "/v1/invoices?expand=customer", base.caller, base.body}},
		{"no query string", in{base.method, "/v1/invoices", base.caller, base.body}},
		{"another API key", in{base.method, base.uri, "key:key_b", base.body}},
		{"a dashboard user", in{base.method, base.uri, "user:usr_a", base.body}},
		{"another body", in{base.method, base.uri, base.caller, []byte(`{"amount":200}`)}},
	} {
		if bytes.Equal(fp(base), fp(c.in)) {
			t.Errorf("%s: the fingerprint must differ from the base request's", c.name)
		}
	}

	// A zero byte separates each part from the next, so moving one byte
	// across a boundary changes the hash.
	for _, c := range []struct {
		boundary string
		a, b     in
	}{
		{"method and uri", in{"POS", "T/v1/a", "key:k", []byte("b")}, in{"POST", "/v1/a", "key:k", []byte("b")}},
		{"uri and caller", in{"POST", "/v1/a", "key:k", []byte("b")}, in{"POST", "/v1/ak", "ey:k", []byte("b")}},
		{"caller and body", in{"POST", "/v1/a", "key:k", []byte("b")}, in{"POST", "/v1/a", "key:kb", nil}},
	} {
		if bytes.Equal(fp(c.a), fp(c.b)) {
			t.Errorf("%s: moving a byte across the boundary must change the fingerprint", c.boundary)
		}
	}
}

// TestRequestCaller holds who the fingerprint names as the caller: the API
// key's id for a key, and the dashboard user's id for a session, so a person
// who signs in again is the same caller. A context with neither, or with both,
// names no caller.
func TestRequestCaller(t *testing.T) {
	bg := context.Background()
	for _, c := range []struct {
		name   string
		ctx    context.Context
		want   string
		wantOK bool
	}{
		{"API key", auth.WithCaller(bg, auth.Caller{KeyID: "key_a"}), "key:key_a", true},
		{"dashboard session", auth.WithCaller(bg, auth.Caller{UserID: "usr_a"}), "user:usr_a", true},
		{"no caller", bg, "", false},
		{"both", auth.WithCaller(bg, auth.Caller{UserID: "usr_a", KeyID: "key_a"}), "", false},
	} {
		got, ok := requestCaller(c.ctx)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: requestCaller = %q, %v; want %q, %v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

// A request that reaches the middleware with a tenant but no caller means
// authentication did not run first. It is refused with 500 and runs nothing,
// rather than being stored under a fingerprint that names no caller.
func TestIdempotency_NoCallerIsRefused(t *testing.T) {
	var calls int
	handler := Idempotency(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest("POST", "/v1/customers", strings.NewReader(`{}`))
	req.Header.Set("Idempotency-Key", "idem-no-caller")
	req = req.WithContext(postgres.WithLivemode(auth.WithTenantID(req.Context(), "t1"), false))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError || calls != 0 {
		t.Errorf("status %d with the handler run %d time(s), want 500 and 0", rec.Code, calls)
	}
}

func TestIdempotency_GET_NotCached(t *testing.T) {
	callCount := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
	})

	// GET with idempotency key — should NOT be cached
	req := httptest.NewRequest("GET", "/v1/customers", nil)
	req.Header.Set("Idempotency-Key", "idem-get-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	handler.ServeHTTP(rec, req)

	if callCount != 2 {
		t.Errorf("GET should not be cached, expected 2 calls, got %d", callCount)
	}
}

func TestRateLimiter_NilDB_FailsOpen(t *testing.T) {
	// With nil db the rate limiter should fail open — every request is allowed.
	rl := NewRateLimiter(nil, "test", 1, time.Minute, FailOpen)
	handler := requesttest.Root(rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/v1/customers", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: fail-open should allow, got %d", i, rec.Code)
		}
	}

	// Headers should still be set even in fail-open mode
	req := httptest.NewRequest("GET", "/v1/customers", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("X-RateLimit-Limit") != "1" {
		t.Errorf("X-RateLimit-Limit: got %q, want 1", rec.Header().Get("X-RateLimit-Limit"))
	}
	if rec.Header().Get("X-RateLimit-Remaining") == "" {
		t.Error("X-RateLimit-Remaining header should be set")
	}
}

func TestRateLimiter_NilDB_FailsClosed_WhenConfigured(t *testing.T) {
	rl := NewRateLimiter(nil, "test", 1, time.Minute, FailClosed)
	handler := requesttest.Root(rl.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest("GET", "/v1/customers", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("fail-closed should return 429 when Redis missing, got %d", rec.Code)
	}
}

// The body is read before the key is reserved. A body over the router's cap
// gets the decoder's 413, and a body that fails to read for another reason
// keeps its 400. Neither runs the handler or touches the store: the store is
// nil here, so a reserve would panic.
func TestIdempotency_BodyReadErrorAnsweredBeforeTheReserve(t *testing.T) {
	for _, c := range []struct {
		name     string
		body     func(http.ResponseWriter) io.ReadCloser
		wantCode int
		wantErr  string
	}{
		{"over the cap", func(w http.ResponseWriter) io.ReadCloser {
			return http.MaxBytesReader(w, io.NopCloser(strings.NewReader(`{"a":"bcdefgh"}`)), 8)
		}, http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"another read error", func(http.ResponseWriter) io.ReadCloser {
			return io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))
		}, http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var calls int
			handler := Idempotency(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
			}))
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "/v1/customers", nil)
			req.Body = c.body(rec)
			req.Header.Set("Idempotency-Key", "idem-body-read")
			ctx := auth.WithCaller(req.Context(), auth.Caller{TenantID: "t1", KeyID: "key_a"})
			handler.ServeHTTP(rec, req.WithContext(postgres.WithLivemode(ctx, false)))

			if rec.Code != c.wantCode || !strings.Contains(rec.Body.String(), `"code":"`+c.wantErr+`"`) || calls != 0 {
				t.Errorf("answer %d %s with the handler run %d time(s), want %d %s and 0", rec.Code, rec.Body, calls, c.wantCode, c.wantErr)
			}
		})
	}
}
