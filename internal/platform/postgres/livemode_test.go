package postgres

import (
	"context"
	"errors"
	"testing"
)

// An unset livemode is a bug at the entry point: Livemode returns
// ErrLivemodeUnset, never a guessed mode.
func TestLivemode_UnsetIsAnError(t *testing.T) {
	t.Parallel()
	live, err := Livemode(context.Background())
	if !errors.Is(err, ErrLivemodeUnset) {
		t.Fatalf("Livemode of a bare ctx: err = %v, want ErrLivemodeUnset", err)
	}
	if live {
		t.Fatal("Livemode of a bare ctx returned live alongside the error")
	}
}

// The ctx key stores *bool, so an explicit false is told apart from unset.
func TestWithLivemode_RoundTrip(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{false, true} {
		got, err := Livemode(WithLivemode(context.Background(), want))
		if err != nil || got != want {
			t.Fatalf("Livemode after WithLivemode(%v) = (%v, %v)", want, got, err)
		}
	}
}

// BeginTx refuses a TxTenant with no mode or no tenant before it opens a
// transaction: the zero DB has no pool, so reaching it would panic.
func TestBeginTx_TenantNeedsModeAndTenant(t *testing.T) {
	t.Parallel()
	db := &DB{}
	if _, err := db.BeginTx(context.Background(), TxTenant, "ten_1"); !errors.Is(err, ErrLivemodeUnset) {
		t.Errorf("TxTenant with no mode: err = %v, want ErrLivemodeUnset", err)
	}
	ctx := WithLivemode(context.Background(), false)
	for _, tid := range []string{"", "  "} {
		if _, err := db.BeginTx(ctx, TxTenant, tid); !errors.Is(err, ErrTenantUnset) {
			t.Errorf("TxTenant with tenant %q: err = %v, want ErrTenantUnset", tid, err)
		}
	}
}
