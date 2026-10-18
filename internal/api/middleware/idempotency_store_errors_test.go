package middleware

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

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
// gets 409 for the rest of the key's life. Both must be visible to an
// operator: counted in velox_idempotency_cache_errors_total and logged at
// ERROR. releaseKey used to drop its errors with no log and no metric, so a
// key stuck after a 409/422 (or an ingest 5xx) left no trace at all.
func TestIdempotency_StoreWriteErrorsAreCountedAndLogged(t *testing.T) {
	pool, err := sql.Open("pgx", "postgres://nobody@127.0.0.1:1/none")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_ = pool.Close() // every BeginTx now fails: "sql: database is closed"
	db := postgres.NewDB(pool, time.Second)

	cases := []struct {
		stage string
		write func(ctx context.Context)
	}{
		{"release", func(ctx context.Context) {
			releaseKey(ctx, db, "t1", "grant-abc123", http.StatusConflict)
		}},
		{"finalize", func(ctx context.Context) {
			finalizeKey(ctx, db, "t1", "grant-abc123", http.StatusOK, []byte(`{}`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.stage, func(t *testing.T) {
			rec := &recordingHandler{}
			prev := slog.Default()
			slog.SetDefault(slog.New(rec))
			t.Cleanup(func() { slog.SetDefault(prev) })

			before := testutil.ToFloat64(idempotencyCacheErrors.WithLabelValues(tc.stage))
			tc.write(postgres.WithLivemode(context.Background(), false))

			if got := testutil.ToFloat64(idempotencyCacheErrors.WithLabelValues(tc.stage)) - before; got != 1 {
				t.Errorf("velox_idempotency_cache_errors_total{stage=%q} rose by %v, want 1", tc.stage, got)
			}

			var logged *slog.Record
			for i := range rec.records {
				if rec.records[i].Level == slog.LevelError {
					logged = &rec.records[i]
					break
				}
			}
			if logged == nil {
				t.Fatalf("no ERROR log for a failed %s; the stuck key leaves no trace", tc.stage)
			}
			// The message must say what a retry actually gets. The handler
			// never re-runs: the pending row answers 409.
			if !strings.Contains(logged.Message, "retries with it get 409") {
				t.Errorf("log message %q does not say retries get 409", logged.Message)
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
