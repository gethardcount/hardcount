package postgres

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// An unset livemode is a bug at the entry point. Under strict mode (tests and
// CI) reading it panics, so the bug fails the suite instead of silently
// reading as live.
func TestLivemode_UnsetPanicsUnderStrict(t *testing.T) {
	// Not t.Parallel — mutates env.
	t.Setenv("VELOX_LIVEMODE_STRICT", "true")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Livemode of a bare ctx should panic under VELOX_LIVEMODE_STRICT")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "livemode was never set") {
			t.Fatalf("panic message should name the issue; got %q", msg)
		}
	}()
	Livemode(context.Background())
}

// In production an unset livemode reads as live, as the database does, and
// logs one warning per call site naming the code that read it.
func TestLivemode_UnsetWarnsOnceAndNamesCaller(t *testing.T) {
	// Not t.Parallel — mutates env, the slog default and the dedup map.
	t.Setenv("VELOX_LIVEMODE_STRICT", "")
	resetLivemodeSeen()

	var buf bytes.Buffer
	var bufMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&syncBuf{Buffer: &buf, mu: &bufMu}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for range 3 {
		if !Livemode(context.Background()) {
			t.Fatal("unset livemode should read as live outside strict mode")
		}
	}
	if livemodeSet(context.Background()) {
		t.Fatal("livemodeSet of bare ctx should be false")
	}

	bufMu.Lock()
	output := buf.String()
	bufMu.Unlock()
	if n := strings.Count(output, "velox: livemode propagation missing"); n != 1 {
		t.Fatalf("expected exactly 1 warning, got %d:\n%s", n, output)
	}
	if !strings.Contains(output, "livemode_test.go:") {
		t.Fatalf("warning should name the test as the caller, not a frame inside postgres.go:\n%s", output)
	}
}

// TestWithLivemode_RoundTrip verifies the ctx key stores *bool correctly so
// a later WithLivemode(false) call isn't masked by the default fallback.
func TestWithLivemode_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := WithLivemode(context.Background(), false)
	if !livemodeSet(ctx) {
		t.Fatal("livemodeSet should be true after WithLivemode")
	}
	if Livemode(ctx) {
		t.Fatal("Livemode should return false after WithLivemode(ctx, false)")
	}

	ctx = WithLivemode(context.Background(), true)
	if !Livemode(ctx) {
		t.Fatal("Livemode should return true after WithLivemode(ctx, true)")
	}
}

// TestWithRequiredLivemode_PanicsWhenUnset is the fan-out safety assertion:
// the helper must panic if called without an upstream WithLivemode. This is
// the behaviour that catches "I forgot to fan out" at the boundary instead
// of 30 frames later.
func TestWithRequiredLivemode_PanicsWhenUnset(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("WithRequiredLivemode should panic when ctx has no livemode")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "without explicit livemode") {
			t.Fatalf("panic message should name the issue; got %q", msg)
		}
	}()
	WithRequiredLivemode(context.Background())
}

// TestWithRequiredLivemode_PassesWhenSet confirms the helper is a no-op when
// the caller did propagate — otherwise the assertion would break every
// legitimate fan-out site.
func TestWithRequiredLivemode_PassesWhenSet(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WithRequiredLivemode should not panic when ctx has livemode; got %v", r)
		}
	}()
	out := WithRequiredLivemode(WithLivemode(context.Background(), false))
	if Livemode(out) {
		t.Fatal("livemode should survive the assertion unchanged")
	}
}

// TestReportUnsetLivemode_WarnsOncePerSite verifies the diagnostic dedup: the
// warning is valuable the first time per call site but noisy afterwards, so
// it must fire exactly once per unique caller file:line.
//
// Self-test of the diagnostic itself: under VELOX_LIVEMODE_STRICT=true the
// reportUnsetLivemode call below would panic, which is exactly what THIS
// test is trying to exercise the non-strict path of. Unset strict for the
// duration of the test so the WARN-and-dedupe behavior runs.
func TestReportUnsetLivemode_WarnsOncePerSite(t *testing.T) {
	// Not t.Parallel — mutates package-level slog default + dedup map.
	resetLivemodeSeen()
	t.Setenv("VELOX_LIVEMODE_STRICT", "false")

	var buf bytes.Buffer
	var bufMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&syncBuf{Buffer: &buf, mu: &bufMu}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Same call site hit three times — expect one log line.
	for range 3 {
		reportUnsetLivemode(1) // skip=1: caller is this for-loop statement
	}

	bufMu.Lock()
	output := buf.String()
	bufMu.Unlock()
	if n := strings.Count(output, "velox: livemode propagation missing"); n != 1 {
		t.Fatalf("expected exactly 1 warning, got %d:\n%s", n, output)
	}
}

// TestReportUnsetLivemode_PanicsUnderStrict verifies VELOX_LIVEMODE_STRICT
// escalates the warning to a panic. This is the mode CI/tests run under so
// any accidentally mode-blind TxTenant surfaces immediately.
func TestReportUnsetLivemode_PanicsUnderStrict(t *testing.T) {
	// Not t.Parallel — mutates env.
	t.Setenv("VELOX_LIVEMODE_STRICT", "true")

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("reportUnsetLivemode should panic under VELOX_LIVEMODE_STRICT")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "VELOX_LIVEMODE_STRICT") {
			t.Fatalf("panic message should name the flag; got %q", msg)
		}
	}()
	reportUnsetLivemode(1)
}

// TestReportUnsetLivemode_StrictDisabledByDefault documents the production
// default: no env → warn, not panic. If this flips, every prod deploy with a
// stray unset-livemode call site starts crashing.
func TestReportUnsetLivemode_StrictDisabledByDefault(t *testing.T) {
	// Not t.Parallel — mutates env and dedup map.
	t.Setenv("VELOX_LIVEMODE_STRICT", "")
	resetLivemodeSeen()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reportUnsetLivemode should not panic without the flag; got %v", r)
		}
	}()
	reportUnsetLivemode(1)
}

// resetLivemodeSeen clears the package-level dedup map so tests that exercise
// warning behaviour start from a clean slate. Test-only helper.
func resetLivemodeSeen() {
	unsetLivemodeSeen.Range(func(k, _ any) bool {
		unsetLivemodeSeen.Delete(k)
		return true
	})
}

// syncBuf serializes writes to a *bytes.Buffer across goroutines. slog.Handler
// gives no concurrency guarantees on the underlying writer, so wrap it.
type syncBuf struct {
	*bytes.Buffer
	mu *sync.Mutex
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Buffer.Write(p)
}
