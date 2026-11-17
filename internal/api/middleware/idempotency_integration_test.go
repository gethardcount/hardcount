package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/version"
)

func invokeWithKey(t *testing.T, h http.Handler, tenantID, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return invokeWithKeyAs(t, h, tenantID, "key_test", key, body)
}

// invokeWithKeyAs sends POST /v1/invoices with key, from the API key keyID,
// in test mode, as authentication leaves the context.
func invokeWithKeyAs(t *testing.T, h http.Handler, tenantID, keyID, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/invoices", strings.NewReader(body))
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("Content-Type", "application/json")
	ctx := auth.WithCaller(req.Context(), auth.Caller{TenantID: tenantID, KeyID: keyID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(postgres.WithLivemode(ctx, false)))
	return rec
}

// seedKey stores a row for key as a test-mode request would, made createdAgo
// before now and expiring expiresIn after it (both Postgres intervals).
func seedKey(t *testing.T, db *postgres.DB, tenantID, key string, fingerprint []byte, status int, body, createdAgo, expiresIn string) {
	t.Helper()
	ctx := postgres.WithLivemode(context.Background(), false)
	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, tenant_id, http_method, http_path, request_fingerprint, status_code, response_body, created_at, expires_at)
		 VALUES ($1, $2, 'POST', '/v1/invoices', $3, $4, $5, now() - $6::interval, now() + $7::interval)`,
		key, tenantID, fingerprint, status, []byte(body), createdAgo, expiresIn); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// seedExpiredKey stores a row for key made 25 hours ago and expired an hour
// ago, which the cleanup job has not deleted yet. Its fingerprint is another
// body's, so a replay of it would be a 422 rather than the new request's 201.
func seedExpiredKey(t *testing.T, db *postgres.DB, tenantID, key string, status int) {
	t.Helper()
	old := fingerprintRequest("POST", "/v1/invoices", "key:key_test", []byte(`{"amount":5}`))
	seedKey(t, db, tenantID, key, old, status, `{"id":"old"}`, "25 hours", "-1 hour")
}

// createHandler stands in for a create route: it decodes the body as every
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

// pollWatch closes polling the first time a request reads a key's row: the
// request that lost the reserve is then waiting on the one that won.
type pollWatch struct {
	once    sync.Once
	polling chan struct{}
}

func (p *pollWatch) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(strings.TrimSpace(d.SQL), "SELECT status_code") {
		p.once.Do(func() { close(p.polling) })
	}
	return ctx
}
func (*pollWatch) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// watchedDB is a store on the test database whose polling closes when a
// request first waits on a key another request holds. Call it after
// SetupTestDB.
func watchedDB(t *testing.T) (*postgres.DB, <-chan struct{}) {
	t.Helper()
	w := &pollWatch{polling: make(chan struct{})}
	return testutil.OpenTracedDB(t, w), w.polling
}

// raceOneKey sends two requests with one key at once. The handler that runs
// first holds its claim until the other request waits on the key, so the two
// always meet. It returns both answers and how many times the handler ran.
func raceOneKey(t *testing.T, tenantID, key string) ([2]*httptest.ResponseRecorder, int64) {
	t.Helper()
	db, polling := watchedDB(t)
	var runs atomic.Int64
	handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if runs.Add(1) == 1 {
			select {
			case <-polling:
			case <-time.After(10 * time.Second):
				t.Error("the second request never waited on the key")
			}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"new"}`))
	}))
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		recs  [2]*httptest.ResponseRecorder
	)
	for i := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recs[i] = invokeWithKey(t, handler, tenantID, key, `{"amount":100}`)
		}()
	}
	close(start)
	wg.Wait()
	return recs, runs.Load()
}

// checkRanOnce fails unless the handler ran once: one answer fresh, the other
// the new answer replayed or a 409, never a second run or an older answer.
func checkRanOnce(t *testing.T, recs [2]*httptest.ResponseRecorder, runs int64) {
	t.Helper()
	if runs != 1 {
		t.Fatalf("handler ran %d times, want exactly 1", runs)
	}
	fresh := 0
	for i, rec := range recs {
		replayed := rec.Header().Get("Idempotent-Replayed") == "true"
		switch {
		case rec.Code == http.StatusCreated && !replayed:
			fresh++
		case rec.Code == http.StatusCreated && replayed:
			if !strings.Contains(rec.Body.String(), `"new"`) {
				t.Errorf("request %d replayed %s, want the new request's answer", i, rec.Body.String())
			}
		case rec.Code == http.StatusConflict:
		default:
			t.Errorf("request %d: got %d (replayed %v), want 201 or 409", i, rec.Code, replayed)
		}
	}
	if fresh != 1 {
		t.Fatalf("%d requests ran fresh, want 1", fresh)
	}
}

// Every answer is stored under its key and replayed, a 400 written after the
// handler read its body included, except a 409, a 422 and a 503, which free
// the key for a second run. A replay keeps the status, the body and
// Hardcount-Version, and sets Idempotent-Replayed.
func TestIdempotency_StoresEveryAnswerExcept409_422And503(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Statuses")
	for _, tc := range []struct {
		status int
		stored bool
	}{
		{http.StatusBadRequest, true},
		{http.StatusUnauthorized, true},
		{http.StatusNotFound, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusGatewayTimeout, true},
		{http.StatusConflict, false},
		{http.StatusUnprocessableEntity, false},
		{http.StatusServiceUnavailable, false},
	} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			var runs atomic.Int64
			handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var in struct {
					Amount int64 `json:"amount"`
				}
				if !respond.DecodeStrict(w, r, &in) {
					return
				}
				runs.Add(1)
				respond.Error(w, r, tc.status, "test_status", "answered "+strconv.Itoa(tc.status))
			}))
			key := "idem-status-" + strconv.Itoa(tc.status)
			first := invokeWithKey(t, handler, tenantID, key, `{"amount":100}`)
			again := invokeWithKey(t, handler, tenantID, key, `{"amount":100}`)

			wantRuns := int64(2)
			if tc.stored {
				wantRuns = 1
			}
			if first.Code != tc.status || again.Code != tc.status || runs.Load() != wantRuns {
				t.Fatalf("answers %d then %d with %d handler runs, want %d twice with %d", first.Code, again.Code, runs.Load(), tc.status, wantRuns)
			}
			if first.Header().Get("Idempotent-Replayed") != "" {
				t.Error("the first answer is marked replayed")
			}
			if replayed := again.Header().Get("Idempotent-Replayed") == "true"; replayed != tc.stored {
				t.Fatalf("second answer replayed = %v, want %v", replayed, tc.stored)
			}
			if tc.stored && (again.Body.String() != first.Body.String() || again.Header().Get("Hardcount-Version") != version.Version) {
				t.Errorf("replay: body %s, Hardcount-Version %q; want %s and %q",
					again.Body.String(), again.Header().Get("Hardcount-Version"), first.Body.String(), version.Version)
			}
		})
	}
}

// Two requests with one key at the same time run the handler once: the other
// replays the answer or gets 409, never a second run of the side effect.
func TestIdempotency_ConcurrentSameKey_RunsOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Concurrent")
	recs, runs := raceOneKey(t, tenantID, "idem-concurrent")
	checkRanOnce(t, recs, runs)
}

// A reservation whose owner died mid-request (pending, older than any
// route's time budget) answers conflict_idempotency_unresolved at once,
// instead of a 5-second poll and a "please retry", and runs nothing: its side
// effect may have committed.
func TestIdempotency_OrphanedReservation_AnswersUnresolved(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Orphan")
	fp := fingerprintRequest("POST", "/v1/invoices", "key:key_test", []byte(`{"amount":100}`))
	seedKey(t, db, tenantID, "idem-orphan", fp, statusPending, "", "10 minutes", "24 hours")

	var calls atomic.Int64
	handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	start := time.Now()
	rec := invokeWithKey(t, handler, tenantID, "idem-orphan", `{"amount":100}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "conflict_idempotency_unresolved") {
		t.Fatalf("orphaned reservation: %d %s, want 409 conflict_idempotency_unresolved", rec.Code, rec.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times on an orphaned reservation", calls.Load())
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the answer must be immediate, not the 5 s poll: took %s", took)
	}
}

// Another caller that sends a key whose request is still running gets 422 at
// once. It does not wait on the first caller's request, so it cannot learn
// when that request finished or how. The caller that made the request still
// waits for its answer and gets it replayed.
func TestIdempotency_AnotherCallerDoesNotWaitOnAPendingKey(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Pending Other Caller")

	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"first"}`))
	}))

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- invokeWithKeyAs(t, handler, tenantID, "key_a", "idem-pending", `{"amount":100}`) }()
	<-entered

	start := time.Now()
	other := invokeWithKeyAs(t, handler, tenantID, "key_b", "idem-pending", `{"amount":100}`)
	if took := time.Since(start); took > time.Second {
		t.Errorf("another caller waited %s on the pending key, want an answer at once", took)
	}
	if other.Code != http.StatusUnprocessableEntity || !strings.Contains(other.Body.String(), "idempotency_error") {
		t.Errorf("another caller: %d %s, want 422 idempotency_error", other.Code, other.Body.String())
	}

	const hold = 300 * time.Millisecond
	retry := make(chan *httptest.ResponseRecorder, 1)
	start = time.Now()
	go func() { retry <- invokeWithKeyAs(t, handler, tenantID, "key_a", "idem-pending", `{"amount":100}`) }()
	time.Sleep(hold)
	close(release)
	got := <-retry
	if took := time.Since(start); took < hold {
		t.Errorf("the same caller's retry answered after %s, before the first request finished", took)
	}
	if got.Code != http.StatusCreated || got.Header().Get("Idempotent-Replayed") != "true" || got.Body.String() != `{"id":"first"}` {
		t.Errorf("the same caller's retry: %d %q %s, want the replayed 201", got.Code, got.Header().Get("Idempotent-Replayed"), got.Body.String())
	}
	if rec := <-first; rec.Code != http.StatusCreated {
		t.Errorf("first request: %d, want 201", rec.Code)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the handler ran %d times, want 1", n)
	}
}

// A stored row with no fingerprint, or an empty one, matches no request: it
// answers 422 and never replays its body.
func TestIdempotency_RowWithoutFingerprintIsAMismatch(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency No Fingerprint")
	handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the handler ran for a key that is already stored")
		w.WriteHeader(http.StatusCreated)
	}))
	for key, fingerprint := range map[string][]byte{"idem-null-fp": nil, "idem-empty-fp": {}} {
		seedKey(t, db, tenantID, key, fingerprint, http.StatusCreated, `{"id":"stored"}`, "0 seconds", "24 hours")
		rec := invokeWithKey(t, handler, tenantID, key, `{"amount":100}`)
		if rec.Code != http.StatusUnprocessableEntity || strings.Contains(rec.Body.String(), "stored") {
			t.Errorf("%s: %d %s, want 422 without the stored body", key, rec.Code, rec.Body.String())
		}
	}
}

// A body the handler refused as unreadable frees the key: the corrected
// request with the same key runs, and a retry of it replays its 201.
func TestIdempotency_UnreadableBodyIsNotStored(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Unreadable Body")
	var runs atomic.Int64
	handler := Idempotency(db, nil)(createHandler(&runs))

	if first := invokeWithKey(t, handler, tenantID, "idem-malformed", `{`); first.Code != http.StatusBadRequest || !strings.Contains(first.Body.String(), "invalid JSON body") {
		t.Fatalf("malformed body: %d %s, want 400 invalid JSON body", first.Code, first.Body.String())
	}
	if fixed := invokeWithKey(t, handler, tenantID, "idem-malformed", `{"amount":100}`); fixed.Code != http.StatusCreated || fixed.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("corrected request with the same key: %d (replayed %q), want a fresh 201", fixed.Code, fixed.Header().Get("Idempotent-Replayed"))
	}
	if retry := invokeWithKey(t, handler, tenantID, "idem-malformed", `{"amount":100}`); retry.Code != http.StatusCreated || retry.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("retry of the corrected request: %d (replayed %q), want the stored 201", retry.Code, retry.Header().Get("Idempotent-Replayed"))
	}
	// The body is part of what a key stands for: the same key with another
	// amount is refused, never answered with the stored 201.
	changed := invokeWithKey(t, handler, tenantID, "idem-malformed", `{"amount":200}`)
	if changed.Code != http.StatusUnprocessableEntity || !strings.Contains(changed.Body.String(), "idempotency_error") || strings.Contains(changed.Body.String(), `"amount":100`) {
		t.Fatalf("same key, another body: %d %s, want 422 idempotency_error with none of the stored answer", changed.Code, changed.Body.String())
	}
	if got := runs.Load(); got != 1 {
		t.Fatalf("handler ran %d times past the decode, want 1", got)
	}
}

// An expired key is absent: a request that reuses it runs as new though the
// cleanup job has not deleted the row, whether the row held an answer or a
// pending reservation, and its answer is stored for a fresh 24 hours.
func TestIdempotency_ExpiredKeyRunsAsNew(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Expired Key")
	for _, tc := range []struct {
		key    string
		status int
	}{
		{"idem-expired-answer", http.StatusCreated},
		{"idem-expired-pending", statusPending},
	} {
		t.Run(tc.key, func(t *testing.T) {
			seedExpiredKey(t, db, tenantID, tc.key, tc.status)
			var runs atomic.Int64
			handler := Idempotency(db, nil)(createHandler(&runs))

			rec := invokeWithKey(t, handler, tenantID, tc.key, `{"amount":100}`)
			if rec.Code != http.StatusCreated || rec.Header().Get("Idempotent-Replayed") != "" || !strings.Contains(rec.Body.String(), `"new"`) {
				t.Fatalf("reused expired key: %d (replayed %q) %s, want a fresh 201", rec.Code, rec.Header().Get("Idempotent-Replayed"), rec.Body.String())
			}
			if retry := invokeWithKey(t, handler, tenantID, tc.key, `{"amount":100}`); retry.Code != http.StatusCreated || retry.Header().Get("Idempotent-Replayed") != "true" {
				t.Fatalf("retry: %d (replayed %q), want the new 201 replayed", retry.Code, retry.Header().Get("Idempotent-Replayed"))
			}
			if got := runs.Load(); got != 1 {
				t.Fatalf("handler ran %d times, want 1", got)
			}

			ctx := postgres.WithLivemode(context.Background(), false)
			tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
			if err != nil {
				t.Fatal(err)
			}
			defer postgres.Rollback(tx)
			var ageSec, leftSec float64
			if err := tx.QueryRowContext(ctx, `SELECT extract(epoch FROM now() - created_at), extract(epoch FROM expires_at - now())
				FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`, tenantID, tc.key).Scan(&ageSec, &leftSec); err != nil {
				t.Fatalf("read key: %v", err)
			}
			if ageSec > 60 || leftSec < 23*3600 {
				t.Fatalf("key row is %.0fs old with %.0fs left, want a new row with about 24h left", ageSec, leftSec)
			}
		})
	}
}

// Two requests that reuse one expired key at once: taking over the expired
// row is the serialization point, so the handler runs once, and the other
// request never gets the expired row's answer.
func TestIdempotency_ExpiredKey_ConcurrentReuseRunsOnce(t *testing.T) {
	db := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, db, "Idempotency Expired Concurrent")
	seedExpiredKey(t, db, tenantID, "idem-expired-race", http.StatusCreated)
	recs, runs := raceOneKey(t, tenantID, "idem-expired-race")
	checkRanOnce(t, recs, runs)
}

// A retry sent while the request that took over an expired key still runs
// waits for that request's answer: taking over the row marks it pending
// again, so the retry never gets the expired row's {"id":"old"}.
func TestIdempotency_ExpiredKey_RetryWhileRunningWaitsForTheNewAnswer(t *testing.T) {
	setup := testutil.SetupTestDB(t)
	tenantID := testutil.CreateTestTenant(t, setup, "Idempotency Expired Retry")
	const key = "idem-expired-wait"
	seedExpiredKey(t, setup, tenantID, key, http.StatusCreated)

	db, polling := watchedDB(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var runs atomic.Int64
	handler := Idempotency(db, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if runs.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"new"}`))
	}))

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- invokeWithKey(t, handler, tenantID, key, `{"amount":100}`) }()
	<-entered
	retry := make(chan *httptest.ResponseRecorder, 1)
	go func() { retry <- invokeWithKey(t, handler, tenantID, key, `{"amount":100}`) }()
	<-polling
	close(release)

	if got := <-retry; got.Code != http.StatusCreated || got.Header().Get("Idempotent-Replayed") != "true" || got.Body.String() != `{"id":"new"}` {
		t.Fatalf("retry while the request ran: %d (replayed %q) %s, want the new 201 replayed", got.Code, got.Header().Get("Idempotent-Replayed"), got.Body.String())
	}
	<-first
}
