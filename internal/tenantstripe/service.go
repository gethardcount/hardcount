package tenantstripe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"

	veloxauth "github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// ProviderConfigErrorRetrier is the narrow contract Connect uses
// to flush invoices stuck on Stripe-configuration tax errors when
// fresh credentials land. Implemented by *invoice.Service. Kept
// as a tiny interface so the tenantstripe package doesn't import
// invoice. ADR-019.
type ProviderConfigErrorRetrier interface {
	RetryProviderConfigErrors(ctx context.Context, tenantID string, livemode bool) (int, []error)
	CountProviderConfigErrors(ctx context.Context, tenantID string, livemode bool) (int, error)
}

// PostConnectRetryTimeout caps the Connect-fan-out goroutine's
// wall-clock work. 5 minutes covers ~1500 invoices retrying at
// the typical ~200ms/invoice Stripe Tax round-trip — far beyond
// expected per-tenant volumes. After the timeout, in-flight
// retries log + exit; remaining stuck rows can be cleared by an
// operator-driven Retry tax click on each.
const PostConnectRetryTimeout = 5 * time.Minute

// StripeClientFactory builds a *stripe.Client from a secret key. Injected so
// tests can substitute a fake without hitting the network.
type StripeClientFactory func(secretKey string) *stripe.Client

// Service coordinates credential writes + a post-write Stripe verify so the
// tenant gets immediate feedback that their key works ("Connected as Acme
// Inc.") rather than discovering it later when their first invoice fails.
type Service struct {
	store        *Store
	newCli       StripeClientFactory
	logger       *slog.Logger
	stuckRetrier ProviderConfigErrorRetrier
}

func NewService(store *Store, newCli StripeClientFactory) *Service {
	return &Service{
		store:  store,
		newCli: newCli,
		logger: slog.Default(),
	}
}

// SetStuckRetrier wires the post-connect fan-out hook. When set,
// Connect verify-success kicks off a background retry of any
// invoices that were stuck on provider_not_configured or
// provider_auth in the (tenant, livemode) just connected. Optional:
// when nil, Connect skips the fan-out (operator falls back to
// per-invoice manual Retry tax). ADR-019.
func (s *Service) SetStuckRetrier(r ProviderConfigErrorRetrier) {
	s.stuckRetrier = r
}

// ConnectInput is the HTTP-facing body for POST /v1/settings/stripe. TenantID
// is not in the body — it's derived from the auth key on the handler.
type ConnectInput struct {
	Livemode       bool   `json:"livemode"`
	SecretKey      string `json:"secret_key"`
	PublishableKey string `json:"publishable_key"`
	WebhookSecret  string `json:"webhook_secret,omitempty"`
}

// Connect validates key shapes, writes the credentials, then calls Stripe
// with the supplied secret to confirm it works and capture account identity.
// A verify failure does NOT roll back the write — the row is persisted so
// the UI can show "connected but failing verify: <reason>" and the tenant
// can retry without re-entering keys.
func (s *Service) Connect(ctx context.Context, tenantID string, in ConnectInput) (domain.StripeProviderCredentials, error) {
	if tenantID == "" {
		return domain.StripeProviderCredentials{}, errs.Required("tenant_id")
	}
	in.SecretKey = strings.TrimSpace(in.SecretKey)
	in.PublishableKey = strings.TrimSpace(in.PublishableKey)
	in.WebhookSecret = strings.TrimSpace(in.WebhookSecret)

	if err := validateKeyShape(in.SecretKey, in.PublishableKey, in.WebhookSecret, in.Livemode); err != nil {
		return domain.StripeProviderCredentials{}, err
	}

	creds, err := s.store.Upsert(ctx, UpsertInput{
		TenantID:       tenantID,
		Livemode:       in.Livemode,
		SecretKey:      in.SecretKey,
		PublishableKey: in.PublishableKey,
		WebhookSecret:  in.WebhookSecret,
	})
	if err != nil {
		return domain.StripeProviderCredentials{}, err
	}

	// Verify by calling /v1/account with the supplied key. A 200 confirms
	// the key is valid and scoped to the intended Stripe account.
	sc := s.newCli(in.SecretKey)
	acct, verifyErr := sc.V1Accounts.Retrieve(ctx, nil)
	if verifyErr != nil {
		kind, msg := describeVerifyError(verifyErr)
		if err := s.store.MarkVerifyFailed(ctx, tenantID, in.Livemode, msg, kind); err != nil {
			s.logger.ErrorContext(ctx, "mark stripe verify failed", "error", err)
		}
		creds.LastVerifiedError = msg
		creds.LastVerifiedErrorKind = kind
		return creds, nil // return creds with error surfaced on the row, not as a 5xx
	}

	name := accountDisplayName(acct)
	if err := s.store.MarkVerified(ctx, tenantID, in.Livemode, acct.ID, name); err != nil {
		return creds, fmt.Errorf("mark verified: %w", err)
	}
	creds.StripeAccountID = acct.ID
	creds.StripeAccountName = name
	now := time.Now().UTC()
	creds.VerifiedAt = &now
	creds.LastVerifiedError = ""

	// Verify-success fan-out: catch up any invoices that were stuck
	// waiting on these credentials. ADR-019. The count is computed
	// synchronously (cheap query) so the response can tell the
	// operator "Retrying N stuck invoices"; the actual retries run
	// in a background goroutine so the connect HTTP call returns
	// in milliseconds regardless of stuck-count.
	if s.stuckRetrier != nil {
		stuckCount, err := s.stuckRetrier.CountProviderConfigErrors(ctx, tenantID, in.Livemode)
		if err != nil {
			s.logger.WarnContext(ctx, "tenantstripe: count stuck invoices failed",
				"tenant_id", tenantID, "livemode", in.Livemode, "error", err)
		} else {
			creds.RetriesQueued = stuckCount
			if stuckCount > 0 {
				go s.runPostConnectRetry(tenantID, in.Livemode, stuckCount)
			}
		}
	}
	return creds, nil
}

// runPostConnectRetry is the goroutine body that flushes stuck
// invoices after a successful Connect. Built off context.Background
// so it survives the connect HTTP request lifecycle, with explicit
// WithTenantID + WithLivemode (per the ctx-attribute audit memory:
// every ctx-detached worker must pin all relevant ctx attrs, not
// just livemode). Capped at PostConnectRetryTimeout so a stuck
// retry can't tie up resources indefinitely.
func (s *Service) runPostConnectRetry(tenantID string, livemode bool, expected int) {
	ctx, cancel := context.WithTimeout(context.Background(), PostConnectRetryTimeout)
	defer cancel()
	ctx = veloxauth.WithTenantID(ctx, tenantID)
	ctx = postgres.WithLivemode(ctx, livemode)

	start := time.Now()
	processed, errs := s.stuckRetrier.RetryProviderConfigErrors(ctx, tenantID, livemode)
	if len(errs) > 0 {
		s.logger.ErrorContext(ctx, "tenantstripe: post-connect retry errors",
			"tenant_id", tenantID, "livemode", livemode,
			"expected", expected, "processed", processed, "error_count", len(errs))
		for _, e := range errs {
			s.logger.WarnContext(ctx, "tenantstripe: post-connect retry per-invoice error",
				"tenant_id", tenantID, "error", e)
		}
		return
	}
	s.logger.InfoContext(ctx, "tenantstripe: post-connect retry complete",
		"tenant_id", tenantID, "livemode", livemode,
		"expected", expected, "processed", processed,
		"duration_ms", time.Since(start).Milliseconds())
}

// List returns both modes' public views. Handlers enforce that the caller's
// auth key tenant matches tenantID before calling.
func (s *Service) List(ctx context.Context, tenantID string) ([]domain.StripeProviderCredentials, error) {
	if tenantID == "" {
		return nil, errs.Required("tenant_id")
	}
	return s.store.ListByTenant(ctx, tenantID)
}

// Delete removes credentials for a single mode. Returns errs.ErrNotFound if
// no row exists — handler translates to 404.
func (s *Service) Delete(ctx context.Context, tenantID string, livemode bool) error {
	if tenantID == "" {
		return errs.Required("tenant_id")
	}
	return s.store.Delete(ctx, tenantID, livemode)
}

// SetWebhookSecret completes the second half of the two-step BYOS setup
// (connect API keys → register endpoint in Stripe → paste whsec_ here) without
// forcing the tenant to re-enter API keys. Stripe isn't re-probed: the key was
// already verified on Connect, and the new secret is HMAC-validated per
// incoming webhook rather than at save time. Returns errs.ErrNotFound if the
// tenant hasn't connected API keys yet — caller must finish Connect first.
func (s *Service) SetWebhookSecret(ctx context.Context, tenantID string, livemode bool, secret string) (domain.StripeProviderCredentials, error) {
	if tenantID == "" {
		return domain.StripeProviderCredentials{}, errs.Required("tenant_id")
	}
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return domain.StripeProviderCredentials{}, errs.Required("webhook_secret")
	}
	if !strings.HasPrefix(secret, "whsec_") {
		return domain.StripeProviderCredentials{}, errs.Invalid("webhook_secret", `must start with "whsec_"`)
	}
	return s.store.SetWebhookSecret(ctx, tenantID, livemode, secret)
}

// GetPlaintext returns the decrypted keys for a (tenant, mode) — used by the
// client resolver and webhook verifier. Not wired to any HTTP route.
func (s *Service) GetPlaintext(ctx context.Context, tenantID string, livemode bool) (PlaintextSecrets, error) {
	return s.store.GetPlaintext(ctx, tenantID, livemode)
}

// LookupEndpoint resolves an opaque webhook endpoint id to the owning tenant
// plus its decrypted signing secret. The webhook handler calls this before
// verifying the signature on an incoming Stripe event.
func (s *Service) LookupEndpoint(ctx context.Context, endpointID string) (EndpointLookup, error) {
	return s.store.GetByID(ctx, endpointID)
}

// Kinds of a failed key check, stored in last_verified_error_kind.
const (
	// VerifyErrorRejected: Stripe answered and refused the key (a 4xx:
	// invalid, revoked, or a restricted key without access to the account).
	VerifyErrorRejected = "rejected"
	// VerifyErrorUnavailable: no answer from Stripe (network failure,
	// timeout) or a 429/5xx, so the key was not checked at all.
	VerifyErrorUnavailable = "unavailable"
)

// describeVerifyError turns a failed GET /v1/account into the kind of failure
// and one sentence for the operator. stripe-go's Error() is the whole error
// marshalled as JSON (stripe-go error.go), which the dashboard used to show
// verbatim; Stripe's own sentence is the Msg field. A failure with no Stripe
// error (dial, DNS, timeout) never reached Stripe, so its text is the
// transport error. Both pass through errs.Scrub like before.
func describeVerifyError(err error) (kind, msg string) {
	var se *stripe.Error
	if errors.As(err, &se) {
		msg = strings.TrimSpace(se.Msg)
		if msg == "" {
			msg = fmt.Sprintf("Stripe answered with HTTP %d.", se.HTTPStatusCode)
		}
		kind = VerifyErrorUnavailable
		if se.HTTPStatusCode >= 400 && se.HTTPStatusCode < 500 && se.HTTPStatusCode != http.StatusTooManyRequests {
			kind = VerifyErrorRejected
		}
		return kind, errs.Scrub(msg)
	}
	return VerifyErrorUnavailable, errs.Scrub(err.Error())
}

// validateKeyShape rejects obviously-wrong keys before touching the DB. The
// Stripe verify call catches revoked/bad keys but catching "sk_live_ on test
// mode" here gives a field-level error the UI can highlight.
func validateKeyShape(secret, publishable, webhook string, livemode bool) error {
	wantSK := "sk_test_"
	wantPK := "pk_test_"
	if livemode {
		wantSK = "sk_live_"
		wantPK = "pk_live_"
	}

	// Restricted keys (rk_test_ / rk_live_) are also valid for secret.
	secretPrefixes := []string{wantSK, strings.Replace(wantSK, "sk_", "rk_", 1)}
	if !hasAnyPrefix(secret, secretPrefixes) {
		return errs.Invalid("secret_key", fmt.Sprintf("must start with %q for this mode", wantSK))
	}
	if !strings.HasPrefix(publishable, wantPK) {
		return errs.Invalid("publishable_key", fmt.Sprintf("must start with %q for this mode", wantPK))
	}
	if webhook != "" && !strings.HasPrefix(webhook, "whsec_") {
		return errs.Invalid("webhook_secret", `must start with "whsec_"`)
	}
	return nil
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// accountDisplayName picks a human-friendly name from a Stripe account. Falls
// back through business name → dashboard display name → email → "".
func accountDisplayName(acct *stripe.Account) string {
	if acct == nil {
		return ""
	}
	if acct.BusinessProfile != nil && acct.BusinessProfile.Name != "" {
		return acct.BusinessProfile.Name
	}
	if acct.Settings != nil && acct.Settings.Dashboard != nil && acct.Settings.Dashboard.DisplayName != "" {
		return acct.Settings.Dashboard.DisplayName
	}
	return acct.Email
}
