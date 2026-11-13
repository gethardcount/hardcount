package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gethardcount/hardcount/internal/auth"
)

// canceledAggStore fails Aggregate the way a query does once its request's
// client has disconnected: with context.Canceled.
type canceledAggStore struct{ *memStore }

func (canceledAggStore) Aggregate(context.Context, ListFilter) (Aggregate, error) {
	return Aggregate{}, context.Canceled
}

// TestUsageAggregate_ClientGoneLogsAtWarn: when the dashboard's client leaves
// while the usage totals are loading, the failed query is logged at WARN, not
// ERROR. The same failure on a live request stays at ERROR.
// Mutation-verify: log with slog.ErrorContext in aggregate → the client-gone
// case logs at ERROR.
func TestUsageAggregate_ClientGoneLogsAtWarn(t *testing.T) {
	gone, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"client went away", gone, "WARN"},
		{"same failure, request still live", context.Background(), "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			h := NewHandler(NewService(canceledAggStore{newMemStore()}), &stubCustomers{}, &stubMeters{})
			ctx := auth.WithTenantID(tc.ctx, "tnt_test")
			req := httptest.NewRequest(http.MethodGet, "/v1/usage/aggregate", nil).WithContext(ctx)
			h.aggregate(httptest.NewRecorder(), req)

			var entry map[string]any
			if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
				t.Fatalf("want exactly one JSON log line, got %q: %v", buf.String(), err)
			}
			if entry["level"] != tc.want {
				t.Errorf("logged level %v, want %s", entry["level"], tc.want)
			}
		})
	}
}
