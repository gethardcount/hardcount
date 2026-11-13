package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// createHandler stands in for a create route. It decodes the body as every
// Hardcount handler does, counts the runs that got past the decode, and
// answers 201.
func createHandler(runs *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Amount int64 `json:"amount"`
		}
		if !respond.DecodeStrict(w, r, &in) {
			return
		}
		runs.Add(1)
		respond.JSON(w, r, http.StatusCreated, map[string]any{"id": "new", "amount": in.Amount})
	})
}

// A body that could not be read is refused before the handler does
// anything, so its 400 is not stored under the key. The corrected request
// with the same key runs, and a retry of that request replays its 201.
// Mutation check: drop the mark in respond.WriteBodyError, and the
// corrected request gets 422 idempotency_error.
func TestIdempotency_UnreadableBodyIsNotStored(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Unreadable Body")

	cases := []struct{ key, body string }{
		{"idem-malformed", `{`},
		{"idem-wrong-type", `{"amount":"ten"}`},
		{"idem-empty", ``},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			var runs atomic.Int64
			handler := Idempotency(db, nil)(createHandler(&runs))

			first := invokeWithKey(t, handler, tenantID, tc.key, tc.body)
			if first.Code != http.StatusBadRequest || !strings.Contains(first.Body.String(), "invalid JSON body") {
				t.Fatalf("body %q: got %d %s, want 400 invalid JSON body", tc.body, first.Code, first.Body.String())
			}

			fixed := invokeWithKey(t, handler, tenantID, tc.key, `{"amount":100}`)
			if fixed.Code != http.StatusCreated || fixed.Header().Get("Idempotent-Replayed") != "" {
				t.Fatalf("corrected request with the same key: got %d (replayed %q) %s, want a fresh 201",
					fixed.Code, fixed.Header().Get("Idempotent-Replayed"), fixed.Body.String())
			}

			retry := invokeWithKey(t, handler, tenantID, tc.key, `{"amount":100}`)
			if retry.Code != http.StatusCreated || retry.Header().Get("Idempotent-Replayed") != "true" {
				t.Fatalf("retry of the corrected request: got %d (replayed %q), want the stored 201 replayed",
					retry.Code, retry.Header().Get("Idempotent-Replayed"))
			}
			if got := runs.Load(); got != 1 {
				t.Fatalf("handler ran %d times past the decode, want 1", got)
			}
		})
	}
}

// A 400 the handler answers after it read the body is still stored. Only a
// refused body frees the key: past the decode, a handler may have written
// something before its 400, so a second run is not known to be safe.
func TestIdempotency_BadRequestAfterTheDecodeIsStored(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Handler 400")

	var runs atomic.Int64
	handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Amount int64 `json:"amount"`
		}
		if !respond.DecodeStrict(w, r, &in) {
			return
		}
		runs.Add(1)
		respond.BadRequest(w, r, "amount is not accepted here")
	}))

	_ = invokeWithKey(t, handler, tenantID, "idem-handler-400", `{"amount":100}`)
	again := invokeWithKey(t, handler, tenantID, "idem-handler-400", `{"amount":100}`)
	if again.Code != http.StatusBadRequest || again.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("second request: got %d (replayed %q), want the stored 400 replayed",
			again.Code, again.Header().Get("Idempotent-Replayed"))
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want 1", got)
	}
}

// seedExpiredKey stores a row for key as a test-mode request would, made 25
// hours ago and expired an hour ago. The cleanup job has not deleted it yet.
func seedExpiredKey(t *testing.T, db *postgres.DB, tenantID, key string, status int) {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	// The fingerprint is another body's, so a replay of this row would be a
	// 422 idempotency_error rather than the new request's 201.
	old := fingerprintRequest("POST", "/v1/invoices", []byte(`{"amount":5}`))
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, tenant_id, http_method, http_path, request_fingerprint, status_code, response_body, created_at, expires_at)
		 VALUES ($1, $2, 'POST', '/v1/invoices', $3, $4, '{"id":"old"}'::bytea, now() - interval '25 hours', now() - interval '1 hour')`,
		key, tenantID, old, status); err != nil {
		t.Fatalf("seed expired key: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// keyLife reads how old key's row is and how long it has left.
func keyLife(t *testing.T, db *postgres.DB, tenantID, key string) (age, left time.Duration) {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	var ageSec, leftSec float64
	if err := tx.QueryRowContext(ctx,
		`SELECT extract(epoch FROM now() - created_at), extract(epoch FROM expires_at - now())
		 FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`, tenantID, key,
	).Scan(&ageSec, &leftSec); err != nil {
		t.Fatalf("read key %s: %v", key, err)
	}
	return time.Duration(ageSec * float64(time.Second)), time.Duration(leftSec * float64(time.Second))
}

// An expired key is absent. A request that reuses it after its 24 hours
// runs as new, though the cleanup job has not deleted the row yet, and its
// answer is stored under the key for a fresh 24 hours.
// Mutation check: drop the DO UPDATE branch of reserveKey's ON CONFLICT,
// and the request gets 409 conflict_idempotency until the cleanup runs.
func TestIdempotency_ExpiredKeyRunsAsNew(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Expired Key")

	cases := []struct {
		key    string
		status int
	}{
		{"idem-expired-answer", http.StatusCreated},
		{"idem-expired-pending", statusPending},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			seedExpiredKey(t, db, tenantID, tc.key, tc.status)
			var runs atomic.Int64
			handler := Idempotency(db, nil)(createHandler(&runs))

			rec := invokeWithKey(t, handler, tenantID, tc.key, `{"amount":100}`)
			if rec.Code != http.StatusCreated || rec.Header().Get("Idempotent-Replayed") != "" ||
				!strings.Contains(rec.Body.String(), `"new"`) {
				t.Fatalf("reused expired key: got %d (replayed %q) %s, want a fresh 201",
					rec.Code, rec.Header().Get("Idempotent-Replayed"), rec.Body.String())
			}

			retry := invokeWithKey(t, handler, tenantID, tc.key, `{"amount":100}`)
			if retry.Code != http.StatusCreated || retry.Header().Get("Idempotent-Replayed") != "true" {
				t.Fatalf("retry: got %d (replayed %q), want the new 201 replayed",
					retry.Code, retry.Header().Get("Idempotent-Replayed"))
			}
			if got := runs.Load(); got != 1 {
				t.Fatalf("handler ran %d times, want 1", got)
			}

			age, left := keyLife(t, db, tenantID, tc.key)
			if age > time.Minute || left < 23*time.Hour {
				t.Fatalf("key row is %s old with %s left, want a new row with about 24h left", age, left)
			}
		})
	}
}

// Two requests that reuse one expired key at the same time: taking over the
// expired row is the serialization point, so the handler runs once. The
// other request replays its answer or gets 409, never a second run.
// Mutation check: drop the WHERE on the DO UPDATE, and both requests take
// the row and run.
func TestIdempotency_ExpiredKey_ConcurrentReuseRunsOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Expired Concurrent")
	seedExpiredKey(t, db, tenantID, "idem-expired-race", http.StatusCreated)

	var runs atomic.Int64
	handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runs.Add(1)
		// Hold the key long enough for the other request to find it taken.
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"new"}`))
	}))

	const n = 2
	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		codes   [n]int
		replays [n]string
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := invokeWithKey(t, handler, tenantID, "idem-expired-race", `{"amount":100}`)
			codes[i] = rec.Code
			replays[i] = rec.Header().Get("Idempotent-Replayed")
		}(i)
	}
	close(start)
	wg.Wait()

	if got := runs.Load(); got != 1 {
		t.Fatalf("handler ran %d times, want exactly 1 (codes %v, replayed %v)", got, codes, replays)
	}
	fresh := 0
	for i := 0; i < n; i++ {
		switch {
		case codes[i] == http.StatusCreated && replays[i] == "":
			fresh++
		case codes[i] == http.StatusCreated && replays[i] == "true", codes[i] == http.StatusConflict:
		default:
			t.Errorf("request %d: got %d (replayed %q), want 201 or 409", i, codes[i], replays[i])
		}
	}
	if fresh != 1 {
		t.Fatalf("%d requests ran fresh, want 1 (codes %v, replayed %v)", fresh, codes, replays)
	}
}
