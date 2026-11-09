package logging

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

// TestErrorLevel: WARN needs both the request's context ended by the client
// (context.Canceled) and an error that wraps context.Canceled. Anything
// else is ERROR.
func TestErrorLevel(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	timedOut, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	t.Cleanup(stop)
	canceled := fmt.Errorf("read next invoice sequence: %w", context.Canceled)

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want slog.Level
	}{
		{"client went away", gone, canceled, slog.LevelWarn},
		{"same error, request still live", context.Background(), canceled, slog.LevelError},
		{"other error after the client left", gone, fmt.Errorf("connection refused"), slog.LevelError},
		{"route timeout", timedOut, fmt.Errorf("list invoices: %w", context.DeadlineExceeded), slog.LevelError},
		// The context says why the request ended, whatever the error wraps.
		{"route timeout, error wraps Canceled", timedOut, canceled, slog.LevelError},
	}
	for _, tc := range cases {
		if got := ErrorLevel(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}
