package api

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// pingConnector is a database/sql connector whose Connect either fails with
// err or hands back a do-nothing connection, so the readiness handler's ping
// can be driven both ways without a database.
type pingConnector struct{ err error }

func (c pingConnector) Connect(context.Context) (driver.Conn, error) {
	if c.err != nil {
		return nil, c.err
	}
	return pingConn{}, nil
}
func (c pingConnector) Driver() driver.Driver { return nil }

type pingConn struct{}

func (pingConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (pingConn) Close() error                        { return nil }
func (pingConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

// TestReadiness_StatusCodeAndBody pins the two halves of the readiness
// contract that a load balancer and an anonymous caller depend on.
//
// A stale billing tick is reported in the body but answers 200: taking the
// replica out of the balancer does not restart the tick, and on a single
// replica it turns a background stall into an API outage. A failed database
// ping answers 503, and the body carries a fixed word, never the driver's
// error text (which names the database user, database and host).
func TestReadiness_StatusCodeAndBody(t *testing.T) {
	setScheduler := func(t *testing.T, lastRun time.Time, interval time.Duration) {
		t.Helper()
		schedulerMu.Lock()
		prevRun, prevInterval := schedulerLastRun, schedulerInterval
		schedulerLastRun, schedulerInterval = lastRun, interval
		schedulerMu.Unlock()
		t.Cleanup(func() {
			schedulerMu.Lock()
			schedulerLastRun, schedulerInterval = prevRun, prevInterval
			schedulerMu.Unlock()
		})
	}
	get := func(t *testing.T, c pingConnector) (int, string, map[string]any) {
		t.Helper()
		pool := sql.OpenDB(c)
		t.Cleanup(func() { _ = pool.Close() })
		rec := httptest.NewRecorder()
		handleDeepHealth(postgres.NewDB(pool, 0))(rec, httptest.NewRequest(http.MethodGet, "/health/ready", nil))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
		}
		return rec.Code, rec.Body.String(), body
	}

	t.Run("stale scheduler tick is reported but stays 200", func(t *testing.T) {
		setScheduler(t, time.Now().Add(-10*time.Minute), time.Minute)

		code, raw, body := get(t, pingConnector{})
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a stale tick must not fail readiness); body %s", code, raw)
		}
		checks, _ := body["checks"].(map[string]any)
		if sched, _ := checks["scheduler"].(string); !strings.HasPrefix(sched, "degraded") {
			t.Fatalf("checks.scheduler = %q, want it to still report the stale tick", sched)
		}
		if checks["database"] != "ok" {
			t.Fatalf("checks.database = %v, want ok", checks["database"])
		}
	})

	// The real-Postgres TestReadiness_DrainingIs503LivenessStays200 covers
	// drain through the router; this keeps the 503 pinned in -short runs.
	t.Run("draining is 503", func(t *testing.T) {
		setScheduler(t, time.Time{}, 0)
		t.Cleanup(resetDrainingForTest)
		SetDraining()

		code, raw, body := get(t, pingConnector{})
		if code != http.StatusServiceUnavailable || body["status"] != "draining" {
			t.Fatalf("draining: status = %d, want 503 draining; body %s", code, raw)
		}
	})

	t.Run("failed database ping is 503 with no error text", func(t *testing.T) {
		setScheduler(t, time.Time{}, 0)
		const detail = "failed to connect to `user=velox_app database=velox`: 10.0.0.7:5432: connection refused"

		code, raw, body := get(t, pingConnector{err: errors.New(detail)})
		if code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503; body %s", code, raw)
		}
		checks, _ := body["checks"].(map[string]any)
		if checks["database"] != "error" {
			t.Fatalf("checks.database = %v, want the fixed word \"error\"", checks["database"])
		}
		for _, leak := range []string{"velox_app", "10.0.0.7", "connection refused"} {
			if strings.Contains(raw, leak) {
				t.Fatalf("body carries database error detail %q: %s", leak, raw)
			}
		}
	})
}
