package middleware

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/platform/request/requesttest"
)

// closedDB is a store whose every transaction fails, as during an outage.
func closedDB(t *testing.T) *postgres.DB {
	t.Helper()
	pool, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = pool.Close()
	return postgres.NewDB(pool, time.Second)
}

// keyedPost is a POST /v1/invoices with an Idempotency-Key, from caller, in
// test mode.
func keyedPost(caller auth.Caller, body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/invoices", body)
	req.Header.Set("Idempotency-Key", "idem-unit")
	return req.WithContext(postgres.WithLivemode(auth.WithCaller(req.Context(), caller), false))
}

// The fingerprint names what makes two requests the same request: the
// method, the path with its query string, the caller and the body. Equal
// inputs hash equal, changing any one changes the hash, and a zero byte
// between the parts keeps a byte moved across a boundary from hashing equal.
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

// A request with no caller, or a caller with no tenant, means authentication
// did not run first. It is refused with 500 and runs nothing, rather than
// stored for no caller or passed through unguarded.
func TestIdempotency_NoCallerIsRefused(t *testing.T) {
	for _, c := range []struct {
		name   string
		caller auth.Caller
	}{
		{"no caller", auth.Caller{TenantID: "t1"}},
		{"a caller with no tenant", auth.Caller{KeyID: "key_a"}},
	} {
		var calls int
		handler := requesttest.Root(Idempotency(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusCreated)
		})))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, keyedPost(c.caller, strings.NewReader(`{}`)))
		if rec.Code != http.StatusInternalServerError || calls != 0 {
			t.Errorf("%s: status %d with the handler run %d time(s), want 500 and 0", c.name, rec.Code, calls)
		}
	}
}

// When the key cannot be reserved, the request is refused with a retryable
// 503 and the handler does not run: running it unprotected would let the
// client's retry run the action a second time.
func TestIdempotency_ReserveErrorRefusesAndRunsNothing(t *testing.T) {
	var calls atomic.Int64
	handler := requesttest.Root(Idempotency(closedDB(t), nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, keyedPost(auth.Caller{TenantID: "t1", KeyID: "key_test"}, strings.NewReader(`{"amount_cents":5000}`)))

	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d, Retry-After %q; want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("handler ran %d time(s); with no reservation it must not run", n)
	}
}

// The body is read before the key is reserved, so a body that fails to read
// answers 400 without running the handler or touching the store: the store is
// nil here, so a reserve would panic.
func TestIdempotency_BodyReadErrorAnsweredBeforeTheReserve(t *testing.T) {
	var calls int
	handler := Idempotency(nil, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, keyedPost(auth.Caller{TenantID: "t1", KeyID: "key_a"}, io.NopCloser(iotest.ErrReader(io.ErrUnexpectedEOF))))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) || calls != 0 {
		t.Errorf("answer %d %s with the handler run %d time(s), want 400 invalid_request and 0", rec.Code, rec.Body, calls)
	}
}

// recordingHandler keeps every slog record so a test can read what was logged.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

// A failed release or finalize leaves the key pending, so every retry with it
// gets 409 for the rest of the key's life. Each is counted in
// hardcount_idempotency_cache_errors_total and logged at ERROR.
func TestIdempotency_StoreWriteErrorsAreCountedAndLogged(t *testing.T) {
	db := closedDB(t)
	for _, tc := range []struct {
		stage string
		write func(ctx context.Context)
	}{
		{"release", func(ctx context.Context) { releaseKey(ctx, db, "t1", "grant-abc123", http.StatusConflict) }},
		{"finalize", func(ctx context.Context) { finalizeKey(ctx, db, "t1", "grant-abc123", http.StatusOK, []byte(`{}`)) }},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			rec := &recordingHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(rec))
			t.Cleanup(func() { slog.SetDefault(prev) })

			before := promtest.ToFloat64(idempotencyCacheErrors.WithLabelValues(tc.stage))
			tc.write(postgres.WithLivemode(context.Background(), false))
			if got := promtest.ToFloat64(idempotencyCacheErrors.WithLabelValues(tc.stage)) - before; got != 1 {
				t.Errorf("hardcount_idempotency_cache_errors_total{stage=%q} rose by %v, want 1", tc.stage, got)
			}

			var logged *slog.Record
			for i := range rec.records {
				if rec.records[i].Level == slog.LevelError {
					logged = &rec.records[i]
					break
				}
			}
			if logged == nil {
				t.Fatalf("no ERROR log for a failed %s", tc.stage)
			}
			attrs := map[string]string{}
			logged.Attrs(func(a slog.Attr) bool {
				attrs[a.Key] = a.Value.String()
				return true
			})
			if attrs["tenant_id"] != "t1" || attrs["stage"] != "begin" || attrs["error"] == "" {
				t.Errorf("log attrs = %v, want tenant_id=t1, stage=begin and the error", attrs)
			}
		})
	}
}
