package clock

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Cutoff is the instant a job lists its due rows by, read once per tick from
// c. It binds nothing: each row's work binds its own instant
// (EnsureEffectiveNow), and a row's work never calls it. On a context that
// carries an instant (a test clock's catch-up, a request) it is that
// instant.
func Cutoff(ctx context.Context, c Clock) time.Time { return c.Now(ctx) }

// refuseMissedBind fails a test whose code reads the server's clock on a
// context decided as real time with no instant bound: work that reads the
// time before it binds the one instant it runs on (EnsureEffectiveNow), so
// two reads in one action could differ. No static check can see the order of
// a decide, a bind and a read, so this one runs where reads happen, in
// production code. testing.Testing() is false in any binary that is not a
// test, so it never fires in production.
//
// Nothing decided (a test's bare context) and live mode are not refused. Its
// only exemption is the clock package's three permitted unbound reads:
// EnsureEffectiveNow's bind, AtArrival's arrival and Cutoff's list cutoff.
//
// It panics, because the read has no *testing.T to fail. The test runner
// re-panics it, so a trip stops its package's test binary and that package's
// later tests do not run until it is fixed. A server under test answers the
// request 500.
func refuseMissedBind(ctx context.Context) {
	if !testing.Testing() {
		return
	}
	if s, ok := Decision(ctx); !ok || s != (Sim{}) {
		return
	}
	if live, err := postgres.Livemode(ctx); err == nil && live {
		return
	}
	pcs := make([]uintptr, 1)
	if runtime.Callers(3, pcs) == 0 { // skip Callers, this function, and the Now that called it
		return
	}
	f, _ := runtime.CallersFrames(pcs).Next()
	const pkg = "github.com/gethardcount/hardcount/internal/platform/clock."
	switch f.Function {
	case pkg + "EnsureEffectiveNow", pkg + "AtArrival", pkg + "Cutoff":
		return
	}
	file := f.File
	if i := strings.Index(file, "/internal/"); i >= 0 {
		file = file[i+1:]
	}
	panic(fmt.Sprintf("clock: %s (%s:%d) read the server's clock under a decision of real time with no instant bound. "+
		"Bind the work's instant before the read: clock.EnsureEffectiveNow for a request or a job's row, or decide the "+
		"record (clock.Decide, DecideWithoutDoor) and then bind; a job's list cutoff reads clock.Cutoff. A test that "+
		"means an instant passes clock.WithEffectiveNow, not clock.WithRealTime.",
		strings.TrimPrefix(f.Function, "github.com/gethardcount/hardcount/"), file, f.Line))
}
