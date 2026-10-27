package payment

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
)

// stripeHTTPClient bounds every Stripe API call at 30s. stripe-go's default
// http.Client allows 80s per request (defaultHTTPTimeout), and the background
// workers (payment reconciler, tax retrier) run on the long-lived process
// context with no request deadline of their own, so each stalled Stripe or
// network call would hold the scheduler's leader lease and a DB connection
// for up to 80s. One shared client with a shorter bound covers every caller.
var stripeHTTPClient = &http.Client{Timeout: 30 * time.Second}

// StripeCredentials resolves decrypted Stripe secrets for a (tenant, mode).
// Implemented by *tenantstripe.Service — payment depends only on this
// narrow interface so the credential store can evolve independently.
type StripeCredentials interface {
	GetPlaintext(ctx context.Context, tenantID string, livemode bool) (tenantstripe.PlaintextSecrets, error)
}

// StripeClients builds per-tenant *stripe.Client instances on demand. Each
// tenant brings their own Stripe keys (stored encrypted via tenantstripe);
// this resolver looks them up and constructs a client with the right secret.
//
// Resolve and ResolveCtx return (nil, nil) only when the tenant has not
// connected Stripe for the mode; callers surface that as
// ErrStripeNotConfigured. Any other miss is an error.
type StripeClients struct {
	fetcher StripeCredentials
	http    *http.Client // nil: stripeHTTPClient
}

// NewStripeClients wires the per-tenant resolver. fetcher=nil returns nil so
// boot code can gate Stripe-dependent components on a single `if clients
// != nil` check, matching the ergonomics of the previous env-var version.
func NewStripeClients(fetcher StripeCredentials) *StripeClients {
	if fetcher == nil {
		return nil
	}
	return &StripeClients{fetcher: fetcher}
}

// NewStripeClientsWithHTTPClient is NewStripeClients sending every Stripe
// request through hc instead of the shared 30-second client. Tests outside
// this package use it to answer as Stripe would, so the real client decodes
// and classifies the reply.
func NewStripeClientsWithHTTPClient(fetcher StripeCredentials, hc *http.Client) *StripeClients {
	c := NewStripeClients(fetcher)
	if c != nil {
		c.http = hc
	}
	return c
}

// errNoTenant is a lookup asked for with no tenant ID: a bug at the caller.
var errNoTenant = errors.New("stripe credentials: no tenant ID")

// Resolve returns the Stripe client for an explicit (tenant, mode). It
// returns (nil, nil) when Stripe is not wired or the tenant has not connected
// it for the mode, and an error when the credentials could not be read.
func (c *StripeClients) Resolve(ctx context.Context, tenantID string, livemode bool) (*stripe.Client, error) {
	if c == nil {
		return nil, nil
	}
	if tenantID == "" {
		return nil, errNoTenant
	}
	creds, err := c.fetcher.GetPlaintext(ctx, tenantID, livemode)
	if err != nil {
		if errors.Is(err, errs.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if creds.SecretKey == "" {
		return nil, nil
	}
	hc := stripeHTTPClient
	if c.http != nil {
		hc = c.http
	}
	return stripe.NewClient(creds.SecretKey, stripe.WithBackends(stripe.NewBackends(hc))), nil
}

// ResolveCtx is Resolve for the tenant and mode on ctx.
func (c *StripeClients) ResolveCtx(ctx context.Context) (*stripe.Client, error) {
	if c == nil {
		return nil, nil
	}
	live, err := postgres.Livemode(ctx)
	if err != nil {
		return nil, err
	}
	return c.Resolve(ctx, auth.TenantID(ctx), live)
}

// Has returns true iff a credential fetcher is wired. Used at server
// startup to gate Stripe-dependent components. A true return does NOT mean
// any specific tenant has credentials connected — only that the system CAN
// resolve per-tenant keys when asked.
func (c *StripeClients) Has() bool {
	return c != nil && c.fetcher != nil
}

// HasFor reports whether this tenant has Stripe credentials stored for the
// mode. Distinct from Has(): Has() reflects "the system can look up keys";
// HasFor reflects "this specific tenant has connected." It returns an error
// when the credentials could not be read.
func (c *StripeClients) HasFor(ctx context.Context, tenantID string, livemode bool) (bool, error) {
	sc, err := c.Resolve(ctx, tenantID, livemode)
	if err != nil {
		return false, err
	}
	return sc != nil, nil
}
