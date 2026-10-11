package middleware

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// When the key cannot be reserved (a database error), the request is refused
// with a retryable 503 and the handler does NOT run. The old fail-open ran it
// unprotected: the action committed, the client retried, and the retry found
// no reservation and ran the action a second time.
func TestIdempotency_ReserveErrorRefusesAndRunsNothing(t *testing.T) {
	pool, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = pool.Close() // every BeginTx now fails: "sql: database is closed"
	db := postgres.NewDB(pool, time.Second)

	var calls atomic.Int64
	handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/credits/grant", strings.NewReader(`{"amount_cents":5000}`))
	req.Header.Set("Idempotency-Key", "grant-abc123")
	req = req.WithContext(postgres.WithLivemode(auth.WithTenantID(req.Context(), "t1"), false))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 503 must tell the client when to retry")
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("handler ran %d time(s); with no reservation it must not run at all", n)
	}
}
