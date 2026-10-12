package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/leader"
)

// TestBillRetryWait_Rungs names the billing retry ladder and its bound
// (ADR-119). The ladder is billing's own policy data: it is not the tax-retry
// ladder, and a change to either number should have to edit this test.
func TestBillRetryWait_Rungs(t *testing.T) {
	if domain.MaxBillingAttempts != 8 {
		t.Fatalf("MaxBillingAttempts = %d, want 8", domain.MaxBillingAttempts)
	}
	want := []time.Duration{
		15 * time.Minute, // after failure 1
		time.Hour,        // 2
		4 * time.Hour,    // 3
		12 * time.Hour,   // 4
		24 * time.Hour,   // 5
		24 * time.Hour,   // 6
		24 * time.Hour,   // 7
	}
	var total time.Duration
	for i, w := range want {
		got, ok := billRetryWait(i + 1)
		if !ok || got != w {
			t.Fatalf("billRetryWait(%d) = %s, %v; want %s, true", i+1, got, ok, w)
		}
		total += got
	}
	if wantTotal := 3*24*time.Hour + 17*time.Hour + 15*time.Minute; total != wantTotal {
		t.Fatalf("earliest park is %s after the first failure, want %s", total, wantTotal)
	}
	// The eighth failure parks, and so does any count past the table: a count
	// above the bound must never produce a next attempt.
	for _, n := range []int{8, 9, 100} {
		if w, ok := billRetryWait(n); ok {
			t.Fatalf("billRetryWait(%d) = %s, true; want parked", n, w)
		}
	}
	// A count below 1 is treated as the first failure, never as "no wait".
	if w, ok := billRetryWait(0); !ok || w != 15*time.Minute {
		t.Fatalf("billRetryWait(0) = %s, %v; want the first rung", w, ok)
	}
}

// TestClassifyBillFailure pins the three classes. Only "counted" is saved.
func TestClassifyBillFailure(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	lost, cancelCause := context.WithCancelCause(context.Background())
	cancelCause(leader.ErrLeaseLost)
	expired, cancelExp := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExp()
	pg := func(code string) error { return fmt.Errorf("query: %w", &pgconn.PgError{Code: code}) }

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		// The run's own ctx is done: interrupted, whatever the error says.
		{"cancelled ctx, ctx error", cancelled, fmt.Errorf("billing loop ctx done: %w", context.Canceled), billClassInterrupted},
		{"cancelled ctx, driver-shaped error", cancelled, errors.New("conn closed"), billClassInterrupted},
		{"cancelled ctx, deterministic refusal", cancelled, errors.New("subscription has no items to bill"), billClassInterrupted},
		{"lease lost", lost, errors.New("anything"), billClassInterrupted},
		{"run deadline passed", expired, context.DeadlineExceeded, billClassInterrupted},

		{"connection failure", live, pg("08006"), billClassTransient},
		{"deadlock", live, pg("40P01"), billClassTransient},
		{"serialization failure", live, pg("40001"), billClassTransient},

		{"statement timeout on a live run", live, pg("57014"), billClassCounted},
		{"inner Go deadline on a live run", live, fmt.Errorf("settings: %w", context.DeadlineExceeded), billClassCounted},
		{"lock timeout", live, pg("55P03"), billClassCounted},
		{"unique violation", live, pg("23505"), billClassCounted},
		{"deterministic refusal", live, errors.New("subscription has no items to bill"), billClassCounted},
		{"panic", live, errBillPanic, billClassCounted},
		{"no progress", live, errStillDue, billClassCounted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyBillFailure(c.ctx, c.err); got != c.want {
				t.Fatalf("class = %q, want %q", got, c.want)
			}
		})
	}
}

type curatedErr struct{ msg string }

func (e curatedErr) Error() string               { return "raw text that must not be saved" }
func (e curatedErr) OperatorSafeMessage() string { return e.msg }

// TestBillFailureText pins the allowlist for the saved error text.
func TestBillFailureText(t *testing.T) {
	const id = "vlx_sub_x"
	generic := "An internal error occurred. Reference: " + id

	if got := billFailureText(id, errors.New("pq: duplicate key value violates unique constraint idx_secret")); got != generic {
		t.Fatalf("raw database text: got %q, want the generic reference", got)
	}
	// A plain domain-looking message is still not on the allowlist.
	if got := billFailureText(id, errors.New("get plan vlx_pln_1: not found")); got != generic {
		t.Fatalf("plain error: got %q, want the generic reference", got)
	}
	if got := billFailureText(id, errBillPanic); got != generic {
		t.Fatalf("panic: got %q, want the generic reference", got)
	}
	wrapped := fmt.Errorf("apply tax: %w", refuse(refusalTaxProvider, errors.New("resolve tax provider: boom")))
	if got := billFailureText(id, wrapped); got != refusalTaxProvider {
		t.Fatalf("refusal: got %q, want its sentence verbatim", got)
	}
	if wrapped.Error() != "apply tax: resolve tax provider: boom" {
		t.Fatalf("a refusal changed the error text: %q", wrapped.Error())
	}
	if got := billFailureText(id, errStillDue); got != refusalNoProgress {
		t.Fatalf("still due: got %q", got)
	}
	if got := billFailureText(id, fmt.Errorf("charge: %w", curatedErr{"Card declined."})); got != "Card declined." {
		t.Fatalf("SafeMessageError: got %q", got)
	}
	hostile := billFailureText(id, curatedErr{"bad \xff byte, a NUL \x00, then " + strings.Repeat("é", 2000)})
	if !utf8.ValidString(hostile) || strings.ContainsRune(hostile, 0) || utf8.RuneCountInString(hostile) != maxBillingErrorRunes {
		t.Fatalf("hostile text: valid=%v nul=%v runes=%d", utf8.ValidString(hostile), strings.ContainsRune(hostile, 0), utf8.RuneCountInString(hostile))
	}
	for _, s := range []string{refusalNoPeriod, refusalNoItems, refusalNetTerms, refusalTimezone, refusalTaxProvider, refusalNoProgress} {
		if utf8.RuneCountInString(s) > maxBillingErrorRunes {
			t.Fatalf("curated sentence is longer than the column cap: %q", s)
		}
	}
}
