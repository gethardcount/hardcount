package testclock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// fullQueue is the real channel queue with its one slot taken, so the next
// Enqueue returns ErrQueueFull.
func fullQueue(t *testing.T) *chanCatchupQueue {
	t.Helper()
	q := NewCatchupQueue(1)
	if err := q.Enqueue(CatchupJob{TenantID: "t1", ClockID: "busy"}); err != nil {
		t.Fatalf("fill queue: %v", err)
	}
	return q
}

// postClock sends a POST to the test-clock routes as a test-mode caller of
// tenant t1 and returns the status and the error code.
func postClock(t *testing.T, h *Handler, path, body string) (int, string, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := postgres.WithLivemode(auth.WithTenantID(req.Context(), "t1"), false)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req.WithContext(ctx))
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return rec.Code, env.Error.Code, env.Error.Message
}

// TestAdvance_QueueFullIs503: when the catch-up queue is full, advance and
// retry-advance answer 503 catchup_queue_full, as catchup.go and ADR-015
// promise, and the message names the way on: retry-advance. The clock is
// parked in internal_failure, which is what makes retry-advance work. Before,
// the wrapped ErrQueueFull fell through FromError to a 500.
// Mutation-verify: drop the ErrQueueFull arm in writeAdvanceError → 500.
func TestAdvance_QueueFullIs503(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	t.Run("advance", func(t *testing.T) {
		store := newMockStore()
		store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady, FrozenTime: start}
		svc := NewService(store)
		svc.SetCatchupQueue(fullQueue(t))

		code, errCode, msg := postClock(t, NewHandler(svc), "/c1/advance", `{"frozen_time":"2026-02-01T00:00:00Z"}`)
		if code != http.StatusServiceUnavailable || errCode != "catchup_queue_full" {
			t.Fatalf("answer %d %q, want 503 catchup_queue_full", code, errCode)
		}
		if !strings.Contains(msg, "/v1/test-clocks/c1/retry-advance") {
			t.Fatalf("message %q does not name retry-advance for the clock", msg)
		}
		if st := store.clocks["c1"].Status; st != domain.TestClockStatusInternalFailed {
			t.Fatalf("clock status %q, want internal_failure (the state retry-advance resumes)", st)
		}
	})

	t.Run("retry-advance", func(t *testing.T) {
		store := newMockStore()
		store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusInternalFailed, FrozenTime: start}
		svc := NewService(store)
		svc.SetCatchupQueue(fullQueue(t))

		code, errCode, _ := postClock(t, NewHandler(svc), "/c1/retry-advance", ``)
		if code != http.StatusServiceUnavailable || errCode != "catchup_queue_full" {
			t.Fatalf("answer %d %q, want 503 catchup_queue_full", code, errCode)
		}
		if st := store.clocks["c1"].Status; st != domain.TestClockStatusInternalFailed {
			t.Fatalf("clock status %q, want internal_failure, so a later retry-advance works", st)
		}
	})
}

// markFailedFails is the mock store whose MarkFailed cannot write.
type markFailedFails struct{ *mockStore }

func (markFailedFails) MarkFailed(context.Context, string, string, string) (domain.TestClock, error) {
	return domain.TestClock{}, errors.New("connection reset")
}

// TestAdvance_QueueFullAndParkFailedIsNot503: when the clock cannot be
// parked either, it is still advancing (the next boot's RecoverInFlight
// queues it), so the answer must not send the caller to retry-advance, which
// refuses a clock that is not in internal_failure. It is a 500.
// Mutation-verify: keep ErrQueueFull in the chain in parkAfterEnqueueFailed's
// failure branch → 503.
func TestAdvance_QueueFullAndParkFailedIsNot503(t *testing.T) {
	store := newMockStore()
	store.clocks["c1"] = domain.TestClock{ID: "c1", TenantID: "t1", Status: domain.TestClockStatusReady,
		FrozenTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	svc := NewService(markFailedFails{store})
	svc.SetCatchupQueue(fullQueue(t))

	code, errCode, _ := postClock(t, NewHandler(svc), "/c1/advance", `{"frozen_time":"2026-02-01T00:00:00Z"}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("answer %d %q, want 500: the clock is still advancing, not parked", code, errCode)
	}
}
