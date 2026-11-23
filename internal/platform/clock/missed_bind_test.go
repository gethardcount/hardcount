package clock

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Under tests, a read of the server's clock on a context decided as real
// time with no instant bound panics, naming the read's caller and what to
// do. The clock package's three permitted unbound reads (EnsureEffectiveNow,
// AtArrival, Cutoff) do not, nor does a read on a context with nothing
// decided, a live one, or one with an instant bound.
func TestRefuseMissedBind(t *testing.T) {
	testMode := postgres.WithLivemode(context.Background(), false)
	realTime := WithRealTime(testMode)
	panicOf := func(f func()) (msg string) {
		defer func() {
			if r := recover(); r != nil {
				msg = fmt.Sprint(r)
			}
		}()
		f()
		return ""
	}

	for name, read := range map[string]func(context.Context) time.Time{"Real().Now": Real().Now, "Now": Now} {
		t.Run(name+" under real time with no instant panics", func(t *testing.T) {
			msg := panicOf(func() { read(realTime) })
			for _, want := range []string{"TestRefuseMissedBind", "internal/platform/clock/missed_bind_test.go:", "clock.EnsureEffectiveNow", "clock.Cutoff"} {
				if !strings.Contains(msg, want) {
					t.Errorf("panic %q; want it to name %q", msg, want)
				}
			}
		})
	}

	for _, c := range []struct {
		name string
		read func()
	}{
		{"EnsureEffectiveNow's bind", func() { EnsureEffectiveNow(realTime, Real()) }},
		{"AtArrival's arrival", func() { AtArrival(realTime, Real()) }},
		{"Cutoff's list cutoff", func() { Cutoff(realTime, Real()) }},
		{"a context with nothing decided", func() { Real().Now(testMode) }},
		{"live mode", func() { Real().Now(WithRealTime(postgres.WithLivemode(context.Background(), true))) }},
		{"an instant bound", func() { Now(WithEffectiveNow(realTime, time.Now())) }},
	} {
		t.Run(c.name+" does not panic", func(t *testing.T) {
			if msg := panicOf(c.read); msg != "" {
				t.Errorf("panicked: %s", msg)
			}
		})
	}
}
