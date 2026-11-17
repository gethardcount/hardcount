package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/logging"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/version"
)

// statusPending marks an idempotency row that is reserved but not yet
// completed. The request that claims the key writes it before running the
// handler. A concurrent request that finds it polls until the owner writes
// the real status_code, which is always above 0. status_code is NOT NULL, so
// this sentinel encodes "in flight" without a separate column.
const statusPending = 0

// idempotencyPollInterval and idempotencyPollTimeout bound how long a losing
// concurrent request waits for the winner to store its response before it
// gives up with 409. The timeout is shorter than typical handler budgets. A
// request still pending after the timeout either died mid-flight, or is a
// slow operation that the client should retry instead of blocking on.
const (
	idempotencyPollInterval = 25 * time.Millisecond
	idempotencyPollTimeout  = 5 * time.Second

	// idempotencyOrphanAfter is the age at which a pending reservation has
	// no live owner: the owner died (OOM, SIGKILL, node loss) or panicked
	// (Recoverer is mounted outside this middleware) between reserving and
	// finalizing. Without this limit, the row would sit at status 0 for its
	// 24 h life, and every retry on any replica would poll 5 s and be told
	// "please retry" (S2 in docs/dev/records/ha-readiness-2026-07-06.md).
	//
	// It is 5 minutes because that is the longest any handler here runs:
	// the export group (router.go), and a refund (POST
	// /v1/invoices/{id}/refund, or POST /v1/credit-notes with auto_issue),
	// whose work runs on its own 5-minute budget past the request timeout.
	// Every other handler gets 30 s. So a retry just past this limit may be
	// told the owner died while a refund is still finishing.
	//
	// Past the limit, a retry is told that the outcome is unknown and how
	// to find out (conflict_idempotency_unresolved). The reservation is
	// never released for a second run, because the handler's transaction
	// may have committed before the owner died.
	idempotencyOrphanAfter = 5 * time.Minute
)

// Idempotency returns middleware that caches responses for POST, PUT and
// PATCH requests that carry an Idempotency-Key header. When a request with
// the same key was processed before, it returns the cached response.
//
// The rules, where the caller is the API key, or the dashboard user, that
// made the request:
//   - Same key, same caller, method, path, query string and body: replay
//     the cached response, with the header Idempotent-Replayed: true. While
//     the first request still runs, wait for its answer.
//   - Same key, any of those different: 422 idempotency_error at once,
//     whether the first request has finished or not. This catches a client
//     bug that reuses a key across operations, such as a retry of
//     POST /invoices with a changed amount under the old key. It also keeps
//     a stored answer, and the state of a running request, from reaching
//     another caller of the account.
//
// ignore names the requests that skip this middleware, and nil means none.
// The server names the requests whose answer carries a secret shown only
// once, which must never be kept for a replay. Those requests run as if no
// header was sent.
func Idempotency(db *postgres.DB, ignore func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return IdempotencyGuard{db: db, ignore: ignore, next: next}
	}
}

// IdempotencyGuard replays the stored answer to a POST, PUT or PATCH that
// repeats an Idempotency-Key from the same caller, and refuses a key reused
// for a different request. A request ignore matches passes through.
// Idempotency makes it; its doc has the rules.
type IdempotencyGuard struct {
	db     *postgres.DB
	ignore func(*http.Request) bool
	next   http.Handler
}

func (g IdempotencyGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" && r.Method != "PUT" && r.Method != "PATCH" {
		g.next.ServeHTTP(w, r)
		return
	}
	if g.ignore != nil && g.ignore(r) {
		g.next.ServeHTTP(w, r)
		return
	}

	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		g.next.ServeHTTP(w, r)
		return
	}

	tenantID := auth.TenantID(r.Context())
	if tenantID == "" {
		g.next.ServeHTTP(w, r)
		return
	}

	// The caller is part of the fingerprint, so a stored answer is
	// returned only to the caller that made the request. That
	// matters because this middleware runs before each route's
	// permission check: a replay does not pass that check again.
	// Authentication runs first and puts exactly one caller on the
	// context. A request without one means the router is wired
	// wrong, so it is refused rather than fingerprinted with no
	// caller.
	caller, ok := requestCaller(r.Context())
	if !ok {
		slog.ErrorContext(r.Context(), "idempotency: no caller on the request context; authentication must run before this middleware",
			"path", r.URL.Path)
		respond.InternalError(w, r)
		return
	}

	// Capture livemode from the request ctx, so the cache write
	// after the response carries the same mode. That write runs on
	// context.Background, not r.Context, so it survives a client
	// disconnect. Without this, the BeginTx for the cache insert
	// defaults to live mode. A test-mode response would then be
	// stored with livemode=true, which corrupts later replays for
	// the test partition.
	livemode, err := postgres.Livemode(r.Context())
	if err != nil {
		respond.FromError(w, r, err, "idempotency")
		return
	}

	// Read the whole body to hash it for the fingerprint, then give
	// the next handler a fresh reader. The router caps the body
	// (limitRequestBody), so a body over the cap is answered here
	// with the 413 the handler's decoder gives without a key. The
	// key is not reserved yet, so nothing is stored for it.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		if !respond.BodyTooLarge(w, r, err) {
			respond.BadRequest(w, r, "failed to read body")
		}
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	fingerprint := fingerprintRequest(r.Method, r.URL.RequestURI(), caller, bodyBytes)

	// Reserve the key before running the handler. reserveKey's one
	// conditional INSERT ... ON CONFLICT DO UPDATE, which resets only
	// an expired row, is the serialization point. Of the concurrent
	// requests with the same (tenant, livemode, key), exactly one
	// claims the row and runs: it inserts a new row or resets an
	// expired one, and RowsAffected = 1 names it. Every other one
	// finds the row taken. Without the reservation, two
	// simultaneous retries could both find no row and both run the
	// side effect: a double credit grant or a double charge.
	claimed, err := reserveKey(r.Context(), g.db, tenantID, key,
		r.Method, r.URL.Path, fingerprint)
	if err != nil {
		// A database error on the reserve fails closed: refuse with
		// a retryable 503 and run nothing. Running the handler
		// without a reservation breaks the key's one promise just
		// when retries happen. The action commits, the client sees
		// an error and retries, and the retry finds no reservation
		// and runs the action again: a second credit grant, refund
		// or charge. A refused request is safe to retry, and Stripe
		// answers a failed idempotency check the same way.
		idempotencyCacheErrors.WithLabelValues("reserve").Inc()
		slog.Log(r.Context(), logging.ErrorLevel(r.Context(), err), "idempotency reserve failed; refusing the request (503) rather than running it unprotected",
			"path", r.URL.Path, "error", err)
		w.Header().Set("Retry-After", "1")
		respond.Error(w, r, http.StatusServiceUnavailable, "idempotency_unavailable", "could not record the idempotency key — retry the request with the same key")
		return
	}
	if !claimed {
		// Another request owns this key. It has either completed, or
		// it is still in flight. Resolve it as a serial retry would:
		// replay the stored response, answer 422 on a fingerprint
		// mismatch, or 409 if it never resolves.
		replayExistingKey(w, r, g.db, tenantID, key, fingerprint)
		return
	}

	recorder := &responseRecorder{
		ResponseWriter: w,
		statusCode:     http.StatusOK,
		body:           &bytes.Buffer{},
	}
	ctx, bodyRefused := respond.TrackBodyRefusal(r.Context())
	g.next.ServeHTTP(recorder, r.WithContext(ctx))

	// Finalize the reservation on context.Background, with the
	// captured livemode, so the write survives a client disconnect.
	// When the connection drops after the side effect committed, the
	// response must still be stored, or the retry runs it again.
	// It carries the request's id too, so the lines finalize and release
	// write join the request's other lines.
	bgCtx := context.WithValue(postgres.WithLivemode(context.Background(), livemode), chimw.RequestIDKey, chimw.GetReqID(r.Context()))

	// Cache every status except 409, 422 and 503, and every refused
	// body. Above all, a retry after a transient 500 must not run
	// the side effects again. The first call may have committed
	// writes the client cannot see, such as a Stripe charge that
	// succeeded while the response timed out. If the 500 were not
	// cached, a retry under the same key would charge twice.
	//
	// 409 Conflict and 422 Unprocessable Entity are not cached,
	// because neither is the real first response. A 409 comes from
	// contention on shared state, and a retry after it clears may
	// succeed. A 422 usually comes from input validation. The client
	// usually fixes the body, and the fingerprint check would then
	// flag the fixed body as a reused key. Storing either would
	// trap the caller, so the pending reservation is deleted
	// instead, which frees the key for a clean retry.
	//
	// 503 Service Unavailable is not cached either. A handler
	// answers 503 only when a second run is safe: something the
	// request needed failed, and nothing it committed would be done
	// twice (errs.Unavailable). Storing it would replay a failure
	// that a retry may get past. A handler that may have made its
	// change must not answer 503.
	//
	// A refused body is not cached either: the 400 for a body that
	// is not valid JSON, or the 422 for a key the request type does
	// not declare (respond.WriteBodyError). The handler answered
	// before it changed anything, and the client fixes the body, so
	// a stored refusal would only trap the corrected request. Any
	// other 400 is still cached: a handler past its decode may have
	// written something before it answered.
	if bodyRefused() ||
		recorder.statusCode == http.StatusConflict ||
		recorder.statusCode == http.StatusUnprocessableEntity ||
		recorder.statusCode == http.StatusServiceUnavailable {
		releaseKey(bgCtx, g.db, tenantID, key, recorder.statusCode)
		return
	}
	finalizeKey(bgCtx, g.db, tenantID, key,
		recorder.statusCode, recorder.body.Bytes())
}

// fingerprintRequest returns a stable hash of what makes two requests the
// same request: the method, the path with its query string (uri), the caller
// (requestCaller) and the body. A retry whose hash differs from the stored
// one reused the key for another request and gets 422 idempotency_error.
// A zero byte separates the parts, so moving a byte across a boundary
// changes the hash: uri "/v1/a" with caller "bc" is not uri "/v1/ab" with
// caller "c".
func fingerprintRequest(method, uri, caller string, body []byte) []byte {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(uri))
	h.Write([]byte{0})
	h.Write([]byte(caller))
	h.Write([]byte{0})
	h.Write(body)
	return h.Sum(nil)
}

// requestCaller names who is calling, from what authentication put on ctx:
// the API key's id, or the dashboard user's id for a session. It is the user
// and not the session, so a person who signs in again and retries is the
// same caller. ok is false unless exactly one of the two is set.
func requestCaller(ctx context.Context) (caller string, ok bool) {
	c := auth.CallerOf(ctx)
	switch {
	case c.KeyID != "" && c.UserID == "":
		return "key:" + c.KeyID, true
	case c.IsSession():
		return "user:" + c.UserID, true
	}
	return "", false
}

type cachedResponse struct {
	StatusCode  int
	Body        []byte
	Fingerprint []byte
	CreatedAt   time.Time // when the key was reserved — a pending row's age
}

// reserveKey claims the idempotency key atomically. It inserts a pending row
// (status_code = statusPending), and reports whether this request won the
// claim. RowsAffected 1 means claimed, so run the handler. 0 means another
// request already owns the key, so replay its outcome. This one statement is
// the serialization point that stops two concurrent requests from both
// running the side effect.
//
// An expired key is absent. When the key's row has passed expires_at, the
// conflict branch resets that row to a new pending reservation, so the
// request runs as new without waiting for CleanExpired to delete it. The
// reset sets status_code back to statusPending, so a retry that arrives
// during the new run waits for the new answer, not the expired one. Of two
// requests that find the same expired row, the second waits on the first's
// row lock, then sees the reset row as unexpired and does not claim it.
//
// The INSERT leaves out livemode on purpose. The BEFORE INSERT trigger
// (set_livemode_from_session) sets it from the transaction's session, so the
// row lands in the mode partition the request runs under. The conflict
// resolves against the (tenant_id, livemode, key) primary key.
func reserveKey(ctx context.Context, db *postgres.DB, tenantID, key, method, path string, fingerprint []byte) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return false, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx,
		`INSERT INTO idempotency_keys (key, tenant_id, http_method, http_path, request_fingerprint, status_code, response_body)
		VALUES ($1, $2, $3, $4, $5, $6, ''::bytea)
		ON CONFLICT (tenant_id, livemode, key) DO UPDATE SET
			http_method = EXCLUDED.http_method,
			http_path = EXCLUDED.http_path,
			request_fingerprint = EXCLUDED.request_fingerprint,
			status_code = EXCLUDED.status_code,
			response_body = EXCLUDED.response_body,
			created_at = EXCLUDED.created_at,
			expires_at = EXCLUDED.expires_at
		WHERE idempotency_keys.expires_at <= now()`,
		key, tenantID, method, path, fingerprint, statusPending,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Conflict with an unexpired row: another request holds the key.
		// Nothing to commit.
		return false, nil
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// finalizeKey writes the captured response onto the pending reservation. The
// owning request always overwrites its own pending row, so an unconditional
// UPDATE on (tenant_id, key) is correct.
func finalizeKey(ctx context.Context, db *postgres.DB, tenantID, key string, statusCode int, body []byte) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	// A finalize failure leaves the row pending with no response stored. A
	// retry does not run the handler again. It finds the pending row and
	// gets 409 conflict_idempotency, then conflict_idempotency_unresolved
	// once the reservation is older than idempotencyOrphanAfter, for the rest
	// of the key's 24-hour life. The client never sees this response
	// replayed, so every failure path is counted and logged.
	fail := func(stage string, err error) {
		idempotencyCacheErrors.WithLabelValues("finalize").Inc()
		slog.ErrorContext(ctx, "idempotency finalize failed; response not stored, key stays pending and retries with it get 409",
			"stage", stage, "tenant_id", tenantID, "status_code", statusCode, "error", err)
	}

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		fail("begin", err)
		return
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`UPDATE idempotency_keys SET status_code = $3, response_body = $4
		WHERE tenant_id = $1 AND key = $2`,
		tenantID, key, statusCode, body,
	); err != nil {
		fail("update", err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail("commit", err)
	}
}

// releaseKey deletes the pending reservation, so a key whose answer is not
// cached (409, 422, 503 or a refused body) can be retried cleanly. When
// the delete fails, the pending row stays. Every retry with the key then gets
// 409 instead of running again (conflict_idempotency_unresolved once the
// reservation is older than idempotencyOrphanAfter). So each failure is
// counted and logged, like a finalize failure.
func releaseKey(ctx context.Context, db *postgres.DB, tenantID, key string, statusCode int) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	fail := func(stage string, err error) {
		idempotencyCacheErrors.WithLabelValues("release").Inc()
		slog.ErrorContext(ctx, "idempotency release failed; key stays pending and retries with it get 409 instead of running again",
			"stage", stage, "tenant_id", tenantID, "status_code", statusCode, "error", err)
	}

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		fail("begin", err)
		return
	}
	defer postgres.Rollback(tx)

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM idempotency_keys WHERE tenant_id = $1 AND key = $2 AND status_code = $3`,
		tenantID, key, statusPending,
	); err != nil {
		fail("delete", err)
		return
	}
	if err := tx.Commit(); err != nil {
		fail("commit", err)
	}
}

// readKey loads the current state of a reserved or completed idempotency row.
// pending reports whether the owner has not finalized it yet (status_code ==
// statusPending). found is false when no row exists, because the owner
// released it. It is called only after reserveKey found the row unexpired,
// so it applies no expiry rule of its own.
func readKey(ctx context.Context, db *postgres.DB, tenantID, key string) (c cachedResponse, found, pending bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return cachedResponse{}, false, false, err
	}
	defer postgres.Rollback(tx)

	err = tx.QueryRowContext(ctx,
		`SELECT status_code, response_body, request_fingerprint, created_at FROM idempotency_keys
		WHERE tenant_id = $1 AND key = $2`,
		tenantID, key,
	).Scan(&c.StatusCode, &c.Body, &c.Fingerprint, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cachedResponse{}, false, false, nil
	}
	if err != nil {
		return cachedResponse{}, false, false, err
	}
	if err := tx.Commit(); err != nil {
		return cachedResponse{}, false, false, err
	}
	return c, true, c.StatusCode == statusPending, nil
}

// replayExistingKey resolves a request that lost the reserve race. It
// answers 422 at once when the row's fingerprint is not this request's,
// replays the stored response once the owner finalizes, and polls briefly
// while the owner is still in flight. If the owner does not finalize within the poll window,
// because it died or the operation is too slow to block on, it returns 409
// conflict_idempotency, which tells the client to retry.
func replayExistingKey(w http.ResponseWriter, r *http.Request, db *postgres.DB, tenantID, key string, fingerprint []byte) {
	ctx := r.Context()
	deadline := time.Now().Add(idempotencyPollTimeout)
	for {
		c, found, pending, err := readKey(ctx, db, tenantID, key)
		if err != nil {
			// On a database error while resolving the conflict, do not fail
			// open: the side effect may already be running. Answer a 409 that
			// the client retries, instead of running the operation again.
			respond.Error(w, r, http.StatusConflict, "conflict_idempotency",
				"A request with this idempotency key is being processed. Please retry.")
			return
		}
		// A row whose fingerprint is not this request's belongs to another
		// caller or another request: answer 422 before anything else. The
		// reservation stores the fingerprint, so this holds while the first
		// request still runs too, and another caller never waits on it or
		// learns how it ended. A row with no fingerprint matches nothing: it
		// cannot be shown to be this caller's, and the comparison is false
		// for it, since a request's fingerprint is never empty.
		if found && subtle.ConstantTimeCompare(c.Fingerprint, fingerprint) != 1 {
			respond.Error(w, r, http.StatusUnprocessableEntity, "idempotency_error",
				"This Idempotency-Key was first used by another caller or with a different request. Use a new key for a new request.")
			return
		}
		if found && !pending {
			// A replay runs no handler and makes no mutation. It hands back
			// the first request's response, and that request wrote the audit
			// row. Mark this request accounted for, so the audit-coverage
			// detector does not read this 2xx as a mutation that lost its
			// row. The detector is mounted at the root and wraps this
			// middleware, so without the mark every idempotent retry of a
			// successful mutation would be reported as uncovered.
			//
			// Writing a second row here would be worse than the false alarm:
			// it would record a mutation that never happened, in an
			// append-only log.
			audit.MarkSkip(ctx)

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Hardcount-Version", version.Version)
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(c.StatusCode)
			_, _ = w.Write(c.Body)
			return
		}
		// Not found means the owner released the key after its handler
		// answered 409, 422 or 503, or refused the body. The key is free
		// again, but this request already lost the race. Treat it as a
		// conflict, so the client retries into a clean slot instead of
		// running concurrently.
		if !found {
			respond.Error(w, r, http.StatusConflict, "conflict_idempotency",
				"A concurrent request with this idempotency key did not complete. Please retry.")
			return
		}
		// Still pending. A reservation older than any route's time budget
		// has no live owner: the owner died or panicked. Say so at once,
		// because the outcome is unknown and polling cannot resolve it.
		if age := time.Since(c.CreatedAt); age > idempotencyOrphanAfter {
			respond.Error(w, r, http.StatusConflict, "conflict_idempotency_unresolved",
				fmt.Sprintf("A request with this idempotency key started %s ago and never recorded an outcome — the replica handling it may have died mid-request. Whether its side effect committed is unknown: fetch the resource to check, then retry with a NEW idempotency key only if it does not exist.", age.Round(time.Minute)))
			return
		}
		// Still pending. Wait for the owner to finalize, bounded by the poll
		// timeout and the request context.
		if time.Now().After(deadline) {
			respond.Error(w, r, http.StatusConflict, "conflict_idempotency",
				"A request with this idempotency key is still being processed. Please retry.")
			return
		}
		select {
		case <-ctx.Done():
			respond.Error(w, r, http.StatusConflict, "conflict_idempotency",
				"A request with this idempotency key is being processed. Please retry.")
			return
		case <-time.After(idempotencyPollInterval):
		}
	}
}

// responseRecorder captures the response for caching while writing to the client.
type responseRecorder struct {
	http.ResponseWriter
	statusCode int
	body       *bytes.Buffer
	answered   bool // the status is set; a later WriteHeader changes nothing on the wire, so nothing here either
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.answered {
		r.statusCode, r.answered = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.answered = true
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

// CleanExpired deletes expired idempotency keys across all tenants and
// returns the number of rows deleted. A background scheduler calls it, not a
// request, so it works across tenants by design. It uses TxBypass to skip the
// tenant_isolation policy.
func CleanExpired(ctx context.Context, db *postgres.DB) (int, error) {
	tx, err := db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return 0, err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at < now()`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

// IdempotencyCleaner adapts CleanExpired to the scheduler's cleaner interface,
// Cleanup(ctx) (int, error), the shape payment.TokenService also has. That
// keeps the scheduler wiring the same for every cleanup task, and it means the
// scheduler package does not need to import middleware.
type IdempotencyCleaner struct {
	db *postgres.DB
}

// NewIdempotencyCleaner returns an IdempotencyCleaner for the keys in db.
func NewIdempotencyCleaner(db *postgres.DB) *IdempotencyCleaner {
	return &IdempotencyCleaner{db: db}
}

// Cleanup runs CleanExpired and returns the number of rows it deleted.
func (c *IdempotencyCleaner) Cleanup(ctx context.Context) (int, error) {
	return CleanExpired(ctx, c.db)
}
