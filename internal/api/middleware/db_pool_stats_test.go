package middleware

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// noDatabase is the connector of a pool that never connects. database/sql
// keeps the pool's counters itself, so a test that only reads them needs no
// database.
type noDatabase struct{}

func (noDatabase) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("this test opens no connection")
}

func (noDatabase) Driver() driver.Driver { return nil }

// The collector goes on the default Prometheus registry, which refuses a
// second registration. `go test -count=2` runs the test twice in one process,
// so both runs share one registration.
var registerPoolStats sync.Once

// TestDBPoolStats_Exported scrapes /metrics after a pool is registered. The
// runbook's pool alert reads go_sql_wait_count_total{db_name="app"}, and
// go_sql_wait_duration_seconds_total is the time those callers waited. A
// series that is missing, renamed or labelled differently leaves the alert
// silent, so each one is checked as the line a scrape returns. The max-open
// line carries the limit set on this pool, which shows the collector reads
// the pool it was given.
func TestDBPoolStats_Exported(t *testing.T) {
	registerPoolStats.Do(func() {
		pool := sql.OpenDB(noDatabase{})
		pool.SetMaxOpenConns(7)
		RegisterDBPoolStats(pool)
	})

	rec := httptest.NewRecorder()
	MetricsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics answered %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, line := range []string{
		`go_sql_wait_count_total{db_name="app"} 0`,
		`go_sql_wait_duration_seconds_total{db_name="app"} 0`,
		`go_sql_max_open_connections{db_name="app"} 7`,
	} {
		if !strings.Contains(body, line+"\n") {
			t.Errorf("/metrics has no line %q", line)
		}
	}
}
