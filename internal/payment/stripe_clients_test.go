package payment

import (
	"context"
	"errors"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

type stubCredentials struct {
	rows map[string]tenantstripe.PlaintextSecrets // key: tenantID + "|" + livemode
	err  map[string]error
}

func (s *stubCredentials) GetPlaintext(_ context.Context, tenantID string, livemode bool) (tenantstripe.PlaintextSecrets, error) {
	key := tenantID + "|"
	if livemode {
		key += "live"
	} else {
		key += "test"
	}
	if e, ok := s.err[key]; ok {
		return tenantstripe.PlaintextSecrets{}, e
	}
	if row, ok := s.rows[key]; ok {
		return row, nil
	}
	return tenantstripe.PlaintextSecrets{}, errs.ErrNotFound
}

func ctxForTenant(tenantID string, livemode bool) context.Context {
	ctx := context.WithValue(context.Background(), auth.TestTenantIDKey(), tenantID)
	return postgres.WithLivemode(ctx, livemode)
}

func TestStripeClients_ResolveCtx_ResolvesPerTenantMode(t *testing.T) {
	fetcher := &stubCredentials{rows: map[string]tenantstripe.PlaintextSecrets{
		"t1|live": {SecretKey: "sk_live_A"},
		"t1|test": {SecretKey: "sk_test_A"},
		"t2|live": {SecretKey: "sk_live_B"},
	}}
	c := NewStripeClients(fetcher)
	if c == nil {
		t.Fatal("NewStripeClients returned nil with fetcher set")
	}
	for _, tc := range []struct {
		tenant string
		live   bool
		want   bool
	}{{"t1", true, true}, {"t1", false, true}, {"t2", false, false}} {
		sc, err := c.ResolveCtx(ctxForTenant(tc.tenant, tc.live))
		if err != nil {
			t.Fatalf("ResolveCtx(%s, live=%v): %v", tc.tenant, tc.live, err)
		}
		if (sc != nil) != tc.want {
			t.Errorf("ResolveCtx(%s, live=%v) client = %v, want connected=%v", tc.tenant, tc.live, sc != nil, tc.want)
		}
	}
}

// A ctx with no mode, or a lookup with no tenant, is a bug at the caller. It
// is an error, never read as "Stripe not connected".
func TestStripeClients_MissingModeOrTenantIsAnError(t *testing.T) {
	c := NewStripeClients(&stubCredentials{})
	noMode := context.WithValue(context.Background(), auth.TestTenantIDKey(), "t1")
	if _, err := c.ResolveCtx(noMode); !errors.Is(err, postgres.ErrLivemodeUnset) {
		t.Errorf("ResolveCtx with no mode: err = %v, want postgres.ErrLivemodeUnset", err)
	}
	if _, err := c.ResolveCtx(postgres.WithLivemode(context.Background(), true)); err == nil {
		t.Error("ResolveCtx with no tenant: err = nil, want an error")
	}
	if _, err := c.Resolve(context.Background(), "", true); err == nil {
		t.Error("Resolve with no tenant: err = nil, want an error")
	}
}

func TestStripeClients_NilBundle(t *testing.T) {
	c := NewStripeClients(nil)
	if c != nil {
		t.Error("NewStripeClients(nil) must return nil so boot code can gate on it")
	}
	if c.Has() {
		t.Error("(*StripeClients)(nil).Has() must be false")
	}
	if sc, err := c.Resolve(context.Background(), "t1", true); sc != nil || err != nil {
		t.Errorf("(*StripeClients)(nil).Resolve = (%v, %v), want (nil, nil): Stripe not wired", sc, err)
	}
	if ok, err := c.HasFor(context.Background(), "t1", true); ok || err != nil {
		t.Errorf("(*StripeClients)(nil).HasFor = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestStripeClients_Resolve_ExplicitTenantMode(t *testing.T) {
	// Background paths (reconciler) have no auth ctx — they pass tenant +
	// mode explicitly from the invoice row. This must work regardless of
	// what's in ctx.
	fetcher := &stubCredentials{rows: map[string]tenantstripe.PlaintextSecrets{
		"t3|test": {SecretKey: "sk_test_X"},
	}}
	c := NewStripeClients(fetcher)
	if sc, err := c.Resolve(context.Background(), "t3", false); err != nil || sc == nil {
		t.Errorf("Resolve(t3, test) = (%v, %v), want a client", sc, err)
	}
	if sc, err := c.Resolve(context.Background(), "t3", true); err != nil || sc != nil {
		t.Errorf("Resolve(t3, live) = (%v, %v), want (nil, nil): not connected", sc, err)
	}
}

// A credential read that fails is returned as an error from Resolve and
// HasFor. Before, For logged it and returned nil, which every caller read
// as "Stripe not connected".
func TestStripeClients_ReadErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	c := NewStripeClients(&stubCredentials{err: map[string]error{"t4|live": boom}})
	if sc, err := c.Resolve(context.Background(), "t4", true); !errors.Is(err, boom) || sc != nil {
		t.Errorf("Resolve with a read error = (%v, %v), want (nil, boom)", sc, err)
	}
	if ok, err := c.HasFor(context.Background(), "t4", true); !errors.Is(err, boom) || ok {
		t.Errorf("HasFor with a read error = (%v, %v), want (false, boom)", ok, err)
	}
}
