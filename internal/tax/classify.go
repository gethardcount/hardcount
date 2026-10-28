package tax

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

// ErrorCode is the typed classification of a tax-provider failure.
// Mirrors the Chargebee taxonomy our 2026-04-30 research surfaced as
// the cleanest precedent across reference platforms (Stripe Tax,
// Avalara, Lago, Recurly). Persisted on the invoice as tax_error_code
// per migration 0067; consumed by the operator-context banner and
// (future) webhook event routing.
type ErrorCode string

const (
	// ErrCodeCustomerDataInvalid: the customer's billing profile is
	// missing or malformed in a way the provider needs (no postal_code
	// for US, invalid tax_id, ambiguous country). Operator remediation
	// is "edit billing profile + retry."
	ErrCodeCustomerDataInvalid ErrorCode = "customer_data_invalid"

	// ErrCodeJurisdictionUnsupported: the provider can't compute tax
	// for the customer's jurisdiction (often: tenant hasn't registered
	// in that region yet, or provider doesn't cover it). Operator
	// remediation is "register with the provider, or override tax
	// manually."
	ErrCodeJurisdictionUnsupported ErrorCode = "jurisdiction_unsupported"

	// ErrCodeProviderOutage: 5xx, timeout, network unreachable. Self-
	// resolves when the provider recovers; operator remediation is
	// "wait or retry."
	ErrCodeProviderOutage ErrorCode = "provider_outage"

	// ErrCodeProviderAuth: invalid / revoked / mis-scoped Stripe key
	// (or equivalent). Operator remediation is "rotate the key in
	// Settings."
	ErrCodeProviderAuth ErrorCode = "provider_auth"

	// ErrCodeProviderNotConfigured: the tax provider is selected in
	// settings (e.g. tax_provider='stripe_tax') but no credentials
	// exist for the active mode. Distinct from ErrCodeProviderAuth —
	// here there's nothing to rotate, the connection has never been
	// made. Operator remediation is "Connect Stripe in Settings →
	// Payments for this mode (test or live)." Common surface point
	// for tenants who turned on Stripe Tax in test mode without
	// connecting their Stripe test keys first.
	ErrCodeProviderNotConfigured ErrorCode = "provider_not_configured"

	// ErrCodeUnknown: classification couldn't infer the cause. The
	// raw error in tax_pending_reason is the source of truth. Better
	// to be honest about uncertainty than guess wrong and route the
	// operator to the wrong remediation.
	ErrCodeUnknown ErrorCode = "unknown"
)

// CleanMessage extracts a human-readable line from a provider error.
// The full payload (often a Stripe Tax JSON envelope) is preserved
// verbatim in tax_pending_reason for diagnostic depth; this helper
// returns just the message field for headline display. Falls back to
// the raw string (capped) when the input isn't a recognisable
// envelope.
//
// Idempotent: passing an already-clean message returns it unchanged.
func CleanMessage(raw string) string {
	if raw == "" {
		return ""
	}
	// Strip a leading provider-name prefix ("stripe tax: ", "stripe_tax: ").
	// Producers of tax_pending_reason wrap the upstream payload with the
	// provider name to disambiguate when one tenant uses multiple providers.
	stripped := stripProviderPrefix(raw)
	// Try to parse as a Stripe-shaped error envelope.
	var env struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(stripped), &env); err == nil && env.Message != "" {
		return env.Message
	}
	if len(stripped) > 200 {
		return stripped[:200] + "…"
	}
	return stripped
}

// Classify maps a provider error (as returned by Provider.Calculate)
// to a typed ErrorCode. Order of checks matters: the first match
// wins, so more-specific patterns sit above more-general ones.
//
// The classifier is heuristic — it inspects the error message text
// rather than asking the provider to declare its category — because
// Provider.Calculate's contract is `error`, not a typed value. This
// matches the reality: Stripe Tax returns a JSON envelope inside an
// error wrapper, and the cleanest place to centralise pattern
// matching is here, not at every call site.
//
// Unmatched errors classify to ErrCodeUnknown; that's the safe
// default — the operator sees the raw tax_pending_reason and is no
// worse off than before this classifier existed.
func Classify(err error) ErrorCode {
	if err == nil {
		return ""
	}
	msg := err.Error()
	low := strings.ToLower(msg)

	// Not configured: provider was never connected for the active
	// mode. Hardcount's own wrapper (tax/stripe.go) produces "no Stripe
	// credentials connected for livemode=…" on Calculate and "no client
	// for context (livemode=…)" on reverse. This match must run BEFORE
	// provider_auth because "credentials" can co-occur with "key" /
	// "credential" phrasings in adjacent provider error variants.
	if reNotConfigured.MatchString(low) {
		return ErrCodeProviderNotConfigured
	}
	// Auth: most provider-specific. Stripe wraps as "invalid api key
	// provided" / "expired API key"; we match on common substrings.
	if reAuth.MatchString(low) {
		return ErrCodeProviderAuth
	}
	// Customer data: postal_code, address, country, tax_id mentioned
	// in the message. Stripe Tax's most common 422 (postal_code
	// missing) hits this. Order: above provider-outage so that a
	// customer-data 5xx (rare but possible) is still classified
	// correctly.
	if reCustomerData.MatchString(low) {
		return ErrCodeCustomerDataInvalid
	}
	// Jurisdiction: registration / nexus / not supported in
	// region phrasing.
	if reJurisdiction.MatchString(low) {
		return ErrCodeJurisdictionUnsupported
	}
	// Outage: timeout, 5xx, network errors.
	if reOutage.MatchString(low) {
		return ErrCodeProviderOutage
	}
	// Also classify nil-derefs / context cancellations as outages —
	// they're transient by definition.
	if errors.Is(err, errSentinelContextCanceled) {
		return ErrCodeProviderOutage
	}
	return ErrCodeUnknown
}

// errSentinelContextCanceled is a placeholder Sentinel — context.Canceled
// would be the natural one, but importing context here just for the
// classifier is overkill. Callers don't need to wrap context errors;
// the regex below catches the typical "context canceled" / "context
// deadline exceeded" message text.
var errSentinelContextCanceled = errors.New("context canceled")

var (
	reNotConfigured = regexp.MustCompile(
		`no client configured|no credentials configured|provider not connected|not configured for livemode|credentials connected for livemode|no .*credentials connected|no client for (mode|context)`,
	)
	reAuth = regexp.MustCompile(
		`invalid api key|expired api key|authentication required|unauthorized|invalid_api_key|api_key_invalid`,
	)
	reCustomerData = regexp.MustCompile(
		`postal[_ ]code|address|country|tax[_ ]?id|customer_details|invalid_request_error.*customer`,
	)
	reJurisdiction = regexp.MustCompile(
		`jurisdiction|registration|not registered|nexus|not supported|unsupported|tax_registration`,
	)
	reOutage = regexp.MustCompile(
		`timeout|timed out|connection refused|temporarily unavailable|service unavailable|503|502|504|gateway|context (canceled|deadline)`,
	)
)

// stripProviderPrefix strips a leading "<name>:" or "<name> tax:" or
// "<snake_name>:" prefix from a provider error string. Examples:
//
//	"stripe tax: {...}"     → "{...}"
//	"stripe_tax: {...}"     → "{...}"
//	"manual: rate not set"  → "rate not set"
//
// Idempotent: a string with no provider prefix is returned unchanged.
func stripProviderPrefix(s string) string {
	for _, prefix := range []string{
		"stripe_tax:", "stripe tax:", "manual:", "none:",
	} {
		if strings.HasPrefix(strings.ToLower(s), prefix) {
			return strings.TrimSpace(s[len(prefix):])
		}
	}
	return s
}
