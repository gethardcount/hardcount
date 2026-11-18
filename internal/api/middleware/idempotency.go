package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/gethardcount/hardcount/internal/platform/request"
	"github.com/gethardcount/hardcount/internal/version"
)

// statusPending is the status_code of a reserved key whose request has not
// stored its answer yet. A stored answer's status is always above it.
const statusPending = 0

const (
	// A request that finds the key held by a running request reads it this
	// often, for this long, before it answers 409.
	idempotencyPollInterval = 25 * time.Millisecond
	idempotencyPollTimeout  = 5 * time.Second

	// idempotencyOrphanAfter is the age past which a pending reservation has
	// no live owner: it died, or panicked (the Recoverer sits outside this
	// middleware), before it stored an answer. Its change may have committed,
	// so the key is never freed for a second run. It is long because a
	// refund's steps outlast the routes' 30 s.
	idempotencyOrphanAfter = 5 * time.Minute

	// keyQueryTimeout bounds each transaction on idempotency_keys.
	keyQueryTimeout = 2 * time.Second
)

// Idempotency returns the middleware that makes a retried write safe. It
// guards a POST, PUT or PATCH with an Idempotency-Key from a tenant, unless
// ignore matches it (nil matches nothing). Every other request passes
// through. The server's ignore names two kinds, which run as if no key was
// sent: a request that matched no route, where nothing can run, and one whose
// answer carries a secret shown once, which must never be kept.
//
// A key is kept per tenant and mode for 24 hours, the expires_at default on
// idempotency_keys. A request is the same
// request as the one that first used the key when its caller (the API key,
// or the dashboard user), method, path, query string and body match:
//
//   - The first request runs, and its answer is stored. The same request
//     again gets that answer replayed, with Idempotent-Replayed: true, after
//     waiting for it while the first still runs.
//   - Any other request with the key gets 422 idempotency_error at once,
//     finished or not. So a stored answer goes only to its own caller, never
//     to another key or user of the account.
//   - A refusal of the caller is not stored: a 401 or a 403 frees the key for
//     the next request with it, as a 409, a 422, a 503 and a refused body
//     (respond.WriteBodyError) do.
//
// Its other answers are in docs/architecture.md, "Where a refusal comes from".
func Idempotency(db *postgres.DB, ignore func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return IdempotencyGuard{db: db, ignore: ignore, next: next}
	}
}

// IdempotencyGuard makes a retried write safe. Idempotency makes it; its doc
// has the rules.
type IdempotencyGuard struct {
	db     *postgres.DB
	ignore func(*http.Request) bool
	next   http.Handler
}

func (g IdempotencyGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writes := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	tenantID := auth.TenantID(r.Context())
	if !writes || (g.ignore != nil && g.ignore(r)) || key == "" {
		g.next.ServeHTTP(w, r)
		return
	}

	// Authentication runs first and leaves a caller and its tenant. Anything
	// else means the router is wired wrong; the answer would be kept for no
	// caller.
	caller, ok := requestCaller(r.Context())
	if !ok || tenantID == "" {
		slog.ErrorContext(r.Context(), "idempotency: no caller on the request context; authentication must run before this middleware",
			"path", request.Of(r.Context()).Route().Pattern)
		respond.InternalError(w, r)
		return
	}

	livemode, err := postgres.Livemode(r.Context())
	if err != nil {
		respond.FromError(w, r, err, "idempotency")
		return
	}

	// The body is read before the key is reserved, so a body that cannot be
	// read, such as one over the router's cap (413), stores nothing.
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

	claimed, err := reserveKey(r.Context(), g.db, tenantID, key,
		r.Method, r.URL.Path, fingerprint)
	if err != nil {
		// Running the handler unreserved would let the client's retry run
		// the action a second time. A refused request is safe to retry.
		idempotencyCacheErrors.WithLabelValues("reserve").Inc()
		slog.Log(r.Context(), logging.ErrorLevel(r.Context(), err), "idempotency reserve failed; refusing the request (503) rather than running it unprotected",
			"path", request.Of(r.Context()).Route().Pattern, "error", err)
		w.Header().Set("Retry-After", "1")
		respond.Error(w, r, http.StatusServiceUnavailable, "idempotency_unavailable", "could not record the idempotency key — retry the request with the same key")
		return
	}
	if !claimed {
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

	// The answer is stored on a background context, so a client that hangs
	// up after the change committed still leaves it for the retry. A tenant
	// transaction needs the request's mode, and the log lines its id.
	bgCtx := context.WithValue(postgres.WithLivemode(context.Background(), livemode), chimw.RequestIDKey, chimw.GetReqID(r.Context()))

	// A stored 500 keeps a retry from running again a change that may have
	// committed. A 401 or a 403 refuses the caller before any change
	// (errs.Forbidden), so another caller's request with the key may run. A
	// 409 or a 422 is not the request's outcome: a retry may get past the
	// conflict, and the client fixes the body. A handler answers 503 only when
	// a second run is safe (errs.Unavailable). A refused body was answered
	// before anything changed. Any other 400 is stored: the handler may have
	// written before it answered.
	if bodyRefused() ||
		recorder.statusCode == http.StatusUnauthorized ||
		recorder.statusCode == http.StatusForbidden ||
		recorder.statusCode == http.StatusConflict ||
		recorder.statusCode == http.StatusUnprocessableEntity ||
		recorder.statusCode == http.StatusServiceUnavailable {
		releaseKey(bgCtx, g.db, tenantID, key, recorder.statusCode)
		return
	}
	finalizeKey(bgCtx, g.db, tenantID, key,
		recorder.statusCode, recorder.body.Bytes())
}

// fingerprintRequest hashes what makes two requests the same request: the
// method, the path with its query string (uri), the caller (requestCaller)
// and the body. A zero byte between the parts keeps a byte moved across a
// boundary from hashing equal: uri "/v1/a" with caller "bc" is not uri
// "/v1/ab" with caller "c".
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

// requestCaller names who is calling, as Caller.Actor names it: the API key's
// id, or the dashboard user's id for a session. It is the user and not the
// session, so a person who signs in again and retries is the same caller. ok
// is false when there is no caller.
func requestCaller(ctx context.Context) (caller string, ok bool) {
	switch actorType, id := auth.CallerOf(ctx).Actor(); actorType {
	case "api_key":
		return "key:" + id, true
	case "user":
		return "user:" + id, true
	}
	return "", false
}

// Every statement on idempotency_keys runs on a tenant transaction, whose
// row-level security confines it to the request's tenant and mode, so the
// statements below name only the tenant and the key.

type keyRow struct {
	StatusCode  int
	Body        []byte
	Fingerprint []byte
	ReservedAt  time.Time
}

// reserveKey claims key and reports whether this request won. Its one
// statement orders the requests that send the key at once: exactly one
// inserts a pending row and runs. An expired key is absent: its row, not yet
// deleted by the IdempotencyCleaner, is taken over as a new pending
// reservation, so a retry during the new run waits for the new answer. Of two requests on one
// expired row, the second waits on the first's row lock, then finds the row
// unexpired. The insert trigger sets livemode from the transaction's mode.
func reserveKey(ctx context.Context, db *postgres.DB, tenantID, key, method, path string, fingerprint []byte) (claimed bool, err error) {
	_, err = inKeyTx(ctx, db, tenantID, "insert", func(ctx context.Context, tx *sql.Tx) error {
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
			return err
		}
		n, err := res.RowsAffected()
		claimed = n > 0
		return err
	})
	return claimed && err == nil, err
}

// finalizeKey stores the answer on this request's pending row, which no other
// request can take while it runs. A failure leaves the key pending, so every
// retry with it gets 409 for the rest of its 24 hours and never this answer:
// each one is counted and logged.
func finalizeKey(ctx context.Context, db *postgres.DB, tenantID, key string, statusCode int, body []byte) {
	stage, err := inKeyTx(ctx, db, tenantID, "update", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE idempotency_keys SET status_code = $3, response_body = $4
			WHERE tenant_id = $1 AND key = $2`,
			tenantID, key, statusCode, body)
		return err
	})
	if err != nil {
		idempotencyCacheErrors.WithLabelValues("finalize").Inc()
		slog.ErrorContext(ctx, "idempotency finalize failed; response not stored, key stays pending and retries with it get 409",
			"stage", stage, "tenant_id", tenantID, "status_code", statusCode, "error", err)
	}
}

// releaseKey deletes this request's pending row, freeing the key. A failure
// leaves the key pending, as in finalizeKey, and is counted and logged with
// answerStatus, the status the request answered.
func releaseKey(ctx context.Context, db *postgres.DB, tenantID, key string, answerStatus int) {
	stage, err := inKeyTx(ctx, db, tenantID, "delete", func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM idempotency_keys WHERE tenant_id = $1 AND key = $2 AND status_code = $3`,
			tenantID, key, statusPending)
		return err
	})
	if err != nil {
		idempotencyCacheErrors.WithLabelValues("release").Inc()
		slog.ErrorContext(ctx, "idempotency release failed; key stays pending and retries with it get 409 instead of running again",
			"stage", stage, "tenant_id", tenantID, "status_code", answerStatus, "error", err)
	}
}

// readKey loads key's row. found is false when its owner released it. It
// runs only after reserveKey found the row unexpired, so it applies no expiry
// rule of its own.
func readKey(ctx context.Context, db *postgres.DB, tenantID, key string) (c keyRow, found bool, err error) {
	_, err = inKeyTx(ctx, db, tenantID, "select", func(ctx context.Context, tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT status_code, response_body, request_fingerprint, created_at FROM idempotency_keys
			WHERE tenant_id = $1 AND key = $2`,
			tenantID, key,
		).Scan(&c.StatusCode, &c.Body, &c.Fingerprint, &c.ReservedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		found = err == nil
		return err
	})
	if err != nil {
		return keyRow{}, false, err
	}
	return c, found, nil
}

// inKeyTx runs statement on a tenant transaction bounded by keyQueryTimeout,
// and commits it. On a failure it names the step that failed: "begin", step
// or "commit".
func inKeyTx(ctx context.Context, db *postgres.DB, tenantID, step string, statement func(context.Context, *sql.Tx) error) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, keyQueryTimeout)
	defer cancel()

	tx, err := db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return "begin", err
	}
	defer postgres.Rollback(tx)

	if err := statement(ctx, tx); err != nil {
		return step, err
	}
	if err := tx.Commit(); err != nil {
		return "commit", err
	}
	return "", nil
}

// replayExistingKey answers a request whose key another request holds: 422
// when the key's fingerprint is not this request's, the stored answer once
// there is one, and otherwise 409, such as when there is still none after
// idempotencyPollTimeout.
func replayExistingKey(w http.ResponseWriter, r *http.Request, db *postgres.DB, tenantID, key string, fingerprint []byte) {
	ctx := r.Context()
	deadline := time.Now().Add(idempotencyPollTimeout)
	for {
		c, found, err := readKey(ctx, db, tenantID, key)
		if err != nil {
			// Not running the request: the first one may be running it now.
			respond.Error(w, r, http.StatusConflict, "conflict_idempotency",
				"A request with this idempotency key is being processed. Please retry.")
			return
		}
		// Checked first, while the first request still runs too, so another
		// caller never waits on it or learns how it ended. A row written
		// without a fingerprint, which this code never does, matches none.
		if found && !bytes.Equal(c.Fingerprint, fingerprint) {
			respond.Error(w, r, http.StatusUnprocessableEntity, "idempotency_error",
				"This Idempotency-Key was first used by another caller or with a different request. Use a new key for a new request.")
			return
		}
		if found && c.StatusCode != statusPending {
			// A replay changes nothing, and the first request wrote the audit
			// row. Without the mark, the audit detector, which wraps this
			// middleware, would report every replayed 2xx; a second row would
			// record a change that never happened.
			audit.MarkSkip(ctx)

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Hardcount-Version", version.Version)
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(c.StatusCode)
			_, _ = w.Write(c.Body)
			return
		}
		// The owner freed the key. This request still lost the race, so it
		// retries rather than run beside whatever took the key next.
		if !found {
			respond.Error(w, r, http.StatusConflict, "conflict_idempotency",
				"A concurrent request with this idempotency key did not complete. Please retry.")
			return
		}
		// No polling can tell what an orphaned reservation's owner did.
		if age := time.Since(c.ReservedAt); age > idempotencyOrphanAfter {
			respond.Error(w, r, http.StatusConflict, "conflict_idempotency_unresolved",
				fmt.Sprintf("A request with this idempotency key started %s ago and never recorded an outcome — the replica handling it may have died mid-request. Whether its side effect committed is unknown: fetch the resource to check, then retry with a NEW idempotency key only if it does not exist.", age.Round(time.Minute)))
			return
		}
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

// responseRecorder keeps a copy of the answer it writes to the client.
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

// IdempotencyCleaner deletes expired keys, in the shape of the scheduler's
// cleaners (billing.IdempotencyCleaner), so the scheduler does not import
// this package.
type IdempotencyCleaner struct {
	db *postgres.DB
}

// NewIdempotencyCleaner returns an IdempotencyCleaner for the keys in db.
func NewIdempotencyCleaner(db *postgres.DB) *IdempotencyCleaner {
	return &IdempotencyCleaner{db: db}
}

// Cleanup deletes the expired keys of every tenant and returns how many it
// deleted. The scheduler calls it outside any request, so it bypasses the
// tenant_isolation policy (TxBypass).
func (c *IdempotencyCleaner) Cleanup(ctx context.Context) (int, error) {
	tx, err := c.db.BeginTx(ctx, postgres.TxBypass, "")
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
