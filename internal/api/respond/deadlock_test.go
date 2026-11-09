package respond

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gethardcount/hardcount/internal/errs"
)

// A request that lost a database deadlock answers 409 concurrent_change,
// which the idempotency layer does not store, so the same request can be sent
// again with the same key. The body says nothing of the cause, and the raw
// error goes to the log at WARN with the route. Every other error passes
// through unchanged.
func TestDeadlockAsConflict(t *testing.T) {
	deadlock := fmt.Errorf("cancel proration relief: %w", &pgconn.PgError{
		Code:    "40P01",
		Message: "deadlock detected",
		Detail:  "Process 101 waits for ShareLock on transaction 7; blocked by process 202.",
	})

	// serve answers POST /v1/things/{id} with err through DeadlockAsConflict
	// and FromError, as a handler does.
	serve := func(err error) *httptest.ResponseRecorder {
		router := chi.NewRouter()
		router.Post("/v1/things/{id}", func(w http.ResponseWriter, r *http.Request) {
			FromError(w, r, DeadlockAsConflict(r, err), "thing")
		})
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/things/thg_1", nil))
		return rec
	}

	t.Run("deadlock", func(t *testing.T) {
		buf := captureLogs(t)
		rec := serve(deadlock)

		if rec.Code != http.StatusConflict {
			t.Fatalf("status: got %d, want 409", rec.Code)
		}
		body := decodeError(t, rec)
		if body.Type != "invalid_request_error" || body.Code != CodeConcurrentChange {
			t.Errorf("body: got %+v, want invalid_request_error %s", body, CodeConcurrentChange)
		}
		var entry map[string]any
		if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
			t.Fatalf("want exactly one JSON log line, got %q: %v", buf.String(), err)
		}
		if entry["level"] != "WARN" {
			t.Errorf("level: got %v, want WARN", entry["level"])
		}
		if got, _ := entry["error"].(string); !strings.Contains(got, "40P01") || !strings.Contains(got, "cancel proration relief") {
			t.Errorf("log attr error: got %q, want the raw error with its SQLSTATE and context", got)
		}
		if got, _ := entry["detail"].(string); !strings.Contains(got, "blocked by process 202") {
			t.Errorf("log attr detail: got %q, want the deadlock's detail (who waited for whom)", got)
		}
		if entry["route"] != "/v1/things/{id}" {
			t.Errorf("log attr route: got %v, want /v1/things/{id}", entry["route"])
		}
	})

	t.Run("no_sql_text", func(t *testing.T) {
		captureLogs(t)
		rec := serve(deadlock)
		body := decodeError(t, rec)
		if body.Message != concurrentChangeMessage {
			t.Errorf("message: got %q, want %q", body.Message, concurrentChangeMessage)
		}
		lower := strings.ToLower(body.Message)
		for _, leak := range []string{"sqlstate", "deadlock", "40p01", "process", "relief", "idempotency"} {
			if strings.Contains(lower, leak) {
				t.Errorf("message %q carries %q: the dashboard shows it to an operator", body.Message, leak)
			}
		}
	})

	t.Run("nil", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		if got := DeadlockAsConflict(req, nil); got != nil {
			t.Errorf("nil error: got %v, want nil", got)
		}
	})

	t.Run("other_error", func(t *testing.T) {
		for name, err := range map[string]error{
			// Every application transaction runs at READ COMMITTED, so a
			// serialization failure is not a lock-order loss and stays a 500.
			"serialization failure 40001": fmt.Errorf("x: %w", &pgconn.PgError{Code: "40001"}),
			"unique violation 23505":      fmt.Errorf("x: %w", &pgconn.PgError{Code: "23505"}),
			"invalid state":               errs.InvalidState("can only issue draft credit notes"),
			"plain error":                 errors.New("connection reset by peer"),
		} {
			t.Run(name, func(t *testing.T) {
				buf := captureLogs(t)
				req := httptest.NewRequest(http.MethodPost, "/", nil)
				if got := DeadlockAsConflict(req, err); got != err {
					t.Errorf("got %v, want the same error unchanged", got)
				}
				if buf.Len() != 0 {
					t.Errorf("logged for an error that is not a deadlock: %s", buf.String())
				}
			})
		}
	})
}
