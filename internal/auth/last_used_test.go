package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A key's last_used_at write is due at most once per lastUsedTouchInterval,
// per key: again once the interval has passed, and never held back by
// another key's write.
func TestLastUsedDue_OncePerIntervalPerKey(t *testing.T) {
	var d lastUsedDebounce
	t0 := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	for _, step := range []struct {
		id   string
		at   time.Time
		want bool
	}{
		{"key_a", t0, true},
		{"key_a", t0.Add(30 * time.Second), false},
		{"key_b", t0.Add(30 * time.Second), true},
		{"key_a", t0.Add(lastUsedTouchInterval - time.Nanosecond), false},
		{"key_a", t0.Add(lastUsedTouchInterval), true},
		{"key_a", t0.Add(lastUsedTouchInterval + time.Second), false},
	} {
		if got := d.due(step.id, step.at); got != step.want {
			t.Errorf("due(%s, +%s) = %v, want %v", step.id, step.at.Sub(t0), got, step.want)
		}
	}
}

// Many requests of one key at the same instant make exactly one write. A
// thousand bursts, each on a fresh debounce, so that a claim that is not
// atomic shows up as a burst with two writes.
func TestLastUsedDue_ExactlyOnceUnderAConcurrentBurst(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	for burst := range 1000 {
		var d lastUsedDebounce
		var wins atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if d.due("key_a", now) {
					wins.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if got := wins.Load(); got != 1 {
			t.Fatalf("burst %d: writes due = %d, want 1", burst, got)
		}
	}
}
