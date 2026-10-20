package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

// stripeErrorMessage extracts a clean human-readable message from Stripe SDK
// errors. All output is run through errs.Scrub so card last4, raw PANs, and
// emails can't leak into the invoice.last_payment_error column or slog
// fields downstream. Scrubbing at this single ingress point means every
// caller (persistence, logging, HTTP responses) is automatically safe.
func stripeErrorMessage(err error) string {
	var stripeErr *stripe.Error
	if errors.As(err, &stripeErr) {
		if stripeErr.Msg != "" {
			return errs.Scrub(stripeErr.Msg)
		}
		return string(stripeErr.Code)
	}
	return errs.Scrub(err.Error())
}

// PaymentError categorises a failed PaymentIntent attempt. Unknown=true means
// the request's outcome could not be determined from the response (5xx, network
// drop, timeout) — callers must NOT treat this as a decline, because Stripe
// may have processed the charge server-side. A reconciler resolves these by
// querying Stripe after a cool-off window.
type PaymentError struct {
	Message         string
	DeclineCode     string // why the card was refused (insufficient_funds, lost_card, …): see declineCodeOf
	PaymentIntentID string // set when Stripe returned a PI object alongside the error
	Unknown         bool
}

func (e *PaymentError) Error() string { return e.Message }

// OperatorSafeMessage implements respond.SafeMessageError so the
// boundary sanitizer (respond.FromError) surfaces a curated message
// instead of falling through to "Internal error". For card declines
// we look up the typed decline_code in a curated map sourced from
// Stripe's docs (decline_codes.go) — gives operators readable
// English ("The card has been reported lost…") rather than the
// awkward title-cased SDK code ("Lost card."). Unknown codes fall
// back to title-case so new Stripe codes don't break the path.
// For non-decline errors we return a category-only message so raw
// Stripe SDK strings (idempotency conflicts, validation errors)
// never reach the operator UI verbatim. ADR-026.
func (e *PaymentError) OperatorSafeMessage() string {
	if e.DeclineCode != "" {
		return "Card was declined: " + declineCodeToOperatorMessage(string(e.DeclineCode))
	}
	if e.Unknown {
		return "Stripe didn't confirm whether the charge went through. Velox will check with Stripe and update the invoice; don't charge again until it does."
	}
	return "Payment provider rejected the request. Please retry; if the problem persists, contact support."
}

// OperatorErrorCode gives an unknown outcome its own wire code. Under the
// shared provider_error a caller could not tell "Stripe refused" (nothing
// was charged) from "no answer" (the card may have been charged), and the
// dashboard titled both "Couldn't charge".
func (e *PaymentError) OperatorErrorCode() string {
	if e.Unknown {
		return "payment_outcome_unknown"
	}
	return ""
}

// humanizeDeclineCode converts Stripe's snake_case decline codes
// ("card_declined", "insufficient_funds") into title-case English.
// Used as a fallback by declineCodeToOperatorMessage when a code
// isn't in the curated map.
func humanizeDeclineCode(code string) string {
	if code == "" {
		return ""
	}
	parts := strings.Split(code, "_")
	if len(parts) == 0 {
		return code
	}
	parts[0] = strings.ToUpper(parts[0][:1]) + parts[0][1:]
	return strings.Join(parts, " ")
}

// ErrPaymentTransient signals that this charge request charged nothing, so
// the invoice state is unchanged: the circuit breaker was open, the
// credentials could not be read, Stripe refused the request with 429, or
// Stripe refused the key (StripeKeyError). The
// scheduler/dunning path should treat this as "skip this tick" and not tick
// the dunning attempt count, unlike a real payment failure.
//
// A 429 speaks only for the request it answers. When stripe-go resent the
// request after a network error, the first request under the same key may
// still be running at Stripe and charge the card (#976). The hold stays, so
// the same initiator resends the same key; another charge kind or a changed
// card sends a new one.
var ErrPaymentTransient = errors.New("payment attempt skipped; upstream breaker open, timeout before Stripe, or Stripe rate limit")

// ErrStripeCredentialsUnavailable is the ErrPaymentTransient for a request
// that never left: the tenant's Stripe credentials could not be read (a
// database blip). Unlike a 429 it says nothing could be running at Stripe, so
// the charge releases its hold (ADR-127).
var ErrStripeCredentialsUnavailable = fmt.Errorf("%w: stripe credentials could not be read", ErrPaymentTransient)

// StripeKeyError is a charge that never ran because Stripe refused the
// tenant's API key (401: the key is invalid or was rolled; 403: a restricted
// key without permission to create PaymentIntents) or because no key is
// connected for this mode. The key is wrong, not the card, so it is not a
// decline: no dunning attempt is used, the invoice is not marked failed, and
// no customer email carries Stripe's sentence. The operator is told instead
// (a metric, an ERROR line, and the Settings → Payments notice).
//
// It wraps ErrPaymentTransient so every charge caller skips the tick
// unchanged. It must not be or wrap a *PaymentError: the auto-charge sweep,
// the dunning retry and Collect all check errors.As(*PaymentError) with
// Unknown=false before ErrPaymentTransient, and would count it as a decline.
// So ErrStripeNotConfigured, itself a PaymentError that refunds, the
// checkout sync and the stale-hold sweep read, is not wrapped either.
type StripeKeyError struct {
	Reason  string // StripeKeyRejected or StripeKeyNotConfigured
	Message string // Stripe's own sentence, scrubbed; empty for not_configured
}

// Reasons of a StripeKeyError; also the label of
// velox_stripe_key_unusable_total.
const (
	StripeKeyRejected      = "rejected"
	StripeKeyNotConfigured = "not_configured"
)

func (e *StripeKeyError) Error() string {
	if e.Reason == StripeKeyNotConfigured {
		return "payment attempt skipped: stripe not configured for this mode"
	}
	return "payment attempt skipped: stripe rejected the API key: " + e.Message
}

func (e *StripeKeyError) Unwrap() error { return ErrPaymentTransient }

// OperatorSafeMessage implements respond.SafeMessageError. Stripe's own
// sentence names the key's last characters or the missing permission; it
// goes to the log and the Settings notice, not to this response.
func (e *StripeKeyError) OperatorSafeMessage() string {
	return "Stripe refused this account's API key (or none is connected for this mode). Nothing was charged. Update the key under Settings → Payments, then collect again."
}

// OperatorErrorCode names the fault so a caller can tell it from a decline.
func (e *StripeKeyError) OperatorErrorCode() string { return "stripe_key_unusable" }

// isRateLimited reports whether Stripe refused the request with HTTP 429. A
// real test-mode reply (2026-10-14) is status 429, type "rate_limit_error",
// code "rate_limit", with no PaymentIntent. Stripe sends 429 only before it
// processes a request, so this request charged nothing. It says nothing about
// an earlier request under the same key: stripe-go resends after a network
// error, and a 429 to the resend can follow a first request that Stripe is
// still running (#976). A 429 lock_timeout is retried by stripe-go itself and
// reaches here only once its retries are spent.
func isRateLimited(err error) bool {
	var se *stripe.Error
	return errors.As(err, &se) && se.HTTPStatusCode == http.StatusTooManyRequests
}

// isIdempotencyConflict reports whether err is Stripe refusing a key that was
// already used with different parameters. Distinguished from the other
// ambiguous outcomes because it carries a stronger fact: an attempt under this
// exact key definitely reached Stripe. Callers use it to name the key in an
// operator-facing ERROR, since the dashboard is searchable by it.
func isIdempotencyConflict(err error) bool {
	var stripeErr *stripe.Error
	return errors.As(err, &stripeErr) && stripeErr.Type == stripe.ErrorTypeIdempotency
}

// declineCodeOf is the code that says why Stripe refused a card: the error's
// decline_code, or, on a card_error that has none, the error's code when that
// code is a hard decline (domain.IsHardDecline, #966). Stripe sends
// incorrect_number either way: as the bank's decline code under
// card_declined, or as its own error code with no decline code
// (https://docs.stripe.com/testing#declined-payments, "Incorrect number
// decline"). Any other error code stays out, so that decline keeps Stripe's
// own message and the ordinary retry schedule, as before.
func declineCodeOf(errType, code, declineCode string) string {
	if declineCode != "" {
		return declineCode
	}
	if errType == string(stripe.ErrorTypeCard) && domain.IsHardDecline(code) {
		return code
	}
	return ""
}

// classifyStripeError maps a stripe-go SDK error to a PaymentError. Non-Stripe
// errors (context cancel, DNS failure wrapped by our code, etc.) are treated
// as unknown, because we cannot prove the request never reached Stripe.
func classifyStripeError(err error) *PaymentError {
	if err == nil {
		return nil
	}
	var stripeErr *stripe.Error
	if !errors.As(err, &stripeErr) {
		return &PaymentError{Message: errs.Scrub(err.Error()), Unknown: true}
	}

	pe := &PaymentError{
		Message:     stripeErrorMessage(err),
		DeclineCode: declineCodeOf(string(stripeErr.Type), string(stripeErr.Code), string(stripeErr.DeclineCode)),
	}
	if stripeErr.PaymentIntent != nil {
		pe.PaymentIntentID = stripeErr.PaymentIntent.ID
	}

	// A 409 is never a definite failure, whatever its type (#976). Stripe
	// answers 409 while another request with the same idempotency key is still
	// running, and stripe-go retries a network error under the same key. So a
	// dropped connection on a request Stripe is still running comes back as
	// 409 "key in use", and the first request can still charge the card.
	// Read by its type (invalid_request_error), it marked the invoice failed
	// and started dunning, whose next retry sends a new key: a second charge.
	// The other 409, "same key, different parameters" (idempotency_error),
	// was already unclear and is handled as before.
	if stripeErr.HTTPStatusCode == http.StatusConflict {
		pe.Unknown = true
		// Not a decline either, whatever its type: Stripe sends declines as
		// 402. A decline code here would land on the unknown attempt row,
		// where HardDeclineOnCard reads it and stops dunning from charging
		// the card (#966), and the API would say "Card was declined".
		pe.DeclineCode = ""
		return pe
	}

	switch stripeErr.Type {
	case stripe.ErrorTypeCard,
		stripe.ErrorTypeInvalidRequest:
		// Stripe explicitly rejected the request; no charge occurred. (A 409
		// of these types returned above.)
		pe.Unknown = false
	case stripe.ErrorTypeIdempotency:
		// AMBIGUOUS, not a rejection — and it used to be grouped with the two
		// above under "no charge occurred", which is backwards. Stripe raises
		// this when the key was ALREADY USED with different parameters, so the
		// one thing it proves is that an attempt with this key reached Stripe;
		// a PaymentIntent may well be live and charging right now.
		//
		// Classifying it as a definite failure stamped payment_status='failed',
		// started dunning inline, told the customer their card had failed, and
		// freed the next retry to mint a SECOND PaymentIntent alongside the
		// first. Unknown routes it to the reconciler instead, which is the
		// component whose entire job is "an attempt may exist at Stripe — go
		// find out". Found 2026-07-30 auditing the idempotency-key seed (#678).
		//
		// The key is logged at the call site precisely for this case: Stripe's
		// dashboard is searchable by idempotency key, so an operator can find
		// the live PI even when the error carries no PI id.
		pe.Unknown = true
	case stripe.ErrorTypeAPI:
		// 5xx from Stripe — request may or may not have been processed.
		pe.Unknown = true
	default:
		// Unknown error type — fail safe, treat as ambiguous.
		pe.Unknown = true
	}
	return pe
}

// ErrStripeNotConfigured is returned when a request routes to a Stripe mode
// (live or test) for which the tenant has not connected credentials. Surfaces
// as an explicit PaymentError rather than a nil deref so operators get an
// actionable signal ("connect Stripe under Settings → Payments").
var ErrStripeNotConfigured = &PaymentError{Message: "stripe not configured for this mode"}

// ErrStripeAuthFailed marks a read Stripe refused with 401 or 403: the
// tenant's key is wrong or revoked, or is a restricted key without permission
// to read PaymentIntents. It will not succeed until someone fixes the key, so
// the stale-hold sweep logs it as a check that cannot run (ADR-127).
var ErrStripeAuthFailed = errors.New("stripe rejected the API key")

// ErrStripeUnreachable marks a read that got no answer from Stripe: a
// network error, or the caller's time limit ended it. Unlike a 5xx or a 429,
// which Stripe answers at once, it can take as long as the time limit allows,
// so the stale-hold sweep stops for the tick after several in a row.
var ErrStripeUnreachable = errors.New("stripe could not be reached")

// ErrStripeCustomerMissing marks a read Stripe refused because the customer
// does not exist in this account (resource_missing on the customer): deleted,
// or created under other credentials. It will not succeed on retry.
var ErrStripeCustomerMissing = errors.New("stripe customer not found in this account")

// ErrNoSavedCard is FetchCardDetails' "the Stripe customer has no card" — a
// one-off payment where the customer did not save the card. It is the only
// FetchCardDetails error that means "nothing to record"; any other is a
// failed lookup and must be retried.
var ErrNoSavedCard = errors.New("no saved card on the Stripe customer")

// LiveStripeClient wraps the Stripe SDK for PaymentIntent operations. Despite
// the "Live" in the name, it handles both live and test modes — it selects
// the underlying per-mode client from clients.ForCtx(ctx) on each call.
// Implements the StripeClient interface used by the payment adapter.
type LiveStripeClient struct {
	clients *StripeClients
}

// NewLiveStripeClient creates a client from the mode-aware StripeClients
// bundle. Returns nil if clients is nil or has no configured modes.
func NewLiveStripeClient(clients *StripeClients) *LiveStripeClient {
	if !clients.Has() {
		return nil
	}
	return &LiveStripeClient{clients: clients}
}

// client resolves the Stripe client for ctx: ErrStripeNotConfigured when the
// tenant has no credentials for this mode, ErrStripeCredentialsUnavailable
// (an ErrPaymentTransient: nothing was sent to Stripe; retry) when the
// credentials could not be read. A lookup blip used to read as "not
// configured" — a definite failure that spent a dunning attempt.
func (c *LiveStripeClient) client(ctx context.Context) (*stripe.Client, error) {
	// No Stripe mode configured at all: NewLiveStripeClient returned nil, and
	// router.go still hands it to the reconciler. The stale-hold sweep can
	// meet a held invoice whose customer has a Stripe id even then (Stripe
	// removed after charges ran), so answer "not configured" instead of
	// panicking the tick.
	if c == nil || c.clients == nil {
		return nil, ErrStripeNotConfigured
	}
	sc, err := c.clients.ResolveCtx(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStripeCredentialsUnavailable, err)
	}
	if sc == nil {
		return nil, ErrStripeNotConfigured
	}
	return sc, nil
}

func (c *LiveStripeClient) CreatePaymentIntent(ctx context.Context, params PaymentIntentParams) (PaymentIntentResult, error) {
	sc, err := c.client(ctx)
	if errors.Is(err, ErrStripeNotConfigured) {
		return PaymentIntentResult{}, &StripeKeyError{Reason: StripeKeyNotConfigured}
	}
	if err != nil {
		return PaymentIntentResult{}, err
	}

	metadata := make(map[string]string)
	for k, v := range params.Metadata {
		metadata[k] = v
	}

	// Charge the exact PaymentMethod the caller resolved from Velox's
	// payment_methods table (the single source of truth for the customer's
	// default card). We deliberately do NOT consult Stripe's
	// invoice_settings.default_payment_method or fall back to "most recent
	// card" — those are independent selectors that can disagree with
	// Velox's default and previously let an off-session charge hit a
	// different card than the one Velox gated on. No PM id → loud failure,
	// never a silent guess.
	if params.PaymentMethodID == "" {
		return PaymentIntentResult{}, &PaymentError{Message: "customer has no payment method on file"}
	}

	pi, err := sc.V1PaymentIntents.Create(ctx, &stripe.PaymentIntentCreateParams{
		Amount:        stripe.Int64(params.AmountCents),
		Currency:      stripe.String(params.Currency),
		Customer:      stripe.String(params.CustomerID),
		PaymentMethod: stripe.String(params.PaymentMethodID),
		Confirm:       stripe.Bool(true),
		OffSession:    stripe.Bool(true),
		Params: stripe.Params{
			IdempotencyKey: stripe.String(fmt.Sprintf("%s_%s", params.IdempotencyKey, params.PaymentMethodID)),
			Metadata:       metadata,
		},
		Description: stripe.String(params.Description),
	})
	if err != nil {
		if isRateLimited(err) {
			// Stripe refused this request before processing it, so this
			// request charged nothing (an earlier one that stripe-go resent
			// may still be running; see isRateLimited). A transient skip: no
			// state write, and the next tick tries again. Unlike the breaker
			// short-circuit the hold stays, for that earlier request. Classified
			// as an error it would park the invoice at 'unknown' (an
			// unrecognised type) for a person to resolve.
			return PaymentIntentResult{}, fmt.Errorf("%w: stripe rate limit: %v", ErrPaymentTransient, errs.Scrub(err.Error()))
		}
		// Stripe refused the key itself, before looking at the charge: 401 for
		// an invalid or rolled key, 403 for a restricted key without
		// PaymentIntent write access. Both come as invalid_request_error, so
		// read by type they were a definite failure: the invoice marked
		// failed, a dunning attempt used, Stripe's sentence in the customer's
		// email. Keyed on the HTTP status, which is what says "the key".
		var se *stripe.Error
		if errors.As(err, &se) && (se.HTTPStatusCode == http.StatusUnauthorized || se.HTTPStatusCode == http.StatusForbidden) {
			return PaymentIntentResult{}, &StripeKeyError{Reason: StripeKeyRejected, Message: errs.Scrub(se.Msg)}
		}
		return PaymentIntentResult{}, classifyStripeError(err)
	}

	return PaymentIntentResult{
		ID:           pi.ID,
		Status:       string(pi.Status),
		ClientSecret: pi.ClientSecret,
	}, nil
}

func (c *LiveStripeClient) FetchCardDetails(ctx context.Context, stripeCustomerID string) (CardDetails, error) {
	sc, err := c.client(ctx)
	if err != nil {
		return CardDetails{}, err
	}

	// Get the most recently created card
	var latest *stripe.PaymentMethod
	for pm, err := range sc.V1PaymentMethods.List(ctx, &stripe.PaymentMethodListParams{
		Customer: stripe.String(stripeCustomerID),
		Type:     stripe.String("card"),
	}) {
		if err != nil {
			// A failed page is not "no card" (and a partial list could pick
			// an older card as the newest): fail, the caller retries.
			return CardDetails{}, fmt.Errorf("list cards for customer %s: %w", stripeCustomerID, classifyStripeError(err))
		}
		if latest == nil || pm.Created > latest.Created {
			latest = pm
		}
	}
	if latest == nil || latest.Card == nil {
		return CardDetails{}, fmt.Errorf("customer %s: %w", stripeCustomerID, ErrNoSavedCard)
	}

	// Set this card as the customer's default payment method
	_, _ = sc.V1Customers.Update(ctx, stripeCustomerID, &stripe.CustomerUpdateParams{
		InvoiceSettings: &stripe.CustomerUpdateInvoiceSettingsParams{
			DefaultPaymentMethod: stripe.String(latest.ID),
		},
	})

	return CardDetails{
		PaymentMethodID: latest.ID,
		Brand:           string(latest.Card.Brand),
		Last4:           latest.Card.Last4,
		ExpMonth:        int(latest.Card.ExpMonth),
		ExpYear:         int(latest.Card.ExpYear),
	}, nil
}

// FetchCardForPaymentIntent retrieves the card actually charged on
// a specific PaymentIntent. Unlike FetchCardDetails (which returns
// the customer's default PM), this works for one-off Checkout
// payments where the customer didn't save the PM — Stripe still
// creates an ephemeral PaymentMethod object for the charge that
// stays accessible through the PI's `payment_method` field. ADR-020.
func (c *LiveStripeClient) FetchCardForPaymentIntent(ctx context.Context, paymentIntentID string) (CardDetails, error) {
	sc, err := c.client(ctx)
	if err != nil {
		return CardDetails{}, err
	}
	pi, err := sc.V1PaymentIntents.Retrieve(ctx, paymentIntentID, &stripe.PaymentIntentRetrieveParams{
		Expand: []*string{stripe.String("payment_method")},
	})
	if err != nil {
		return CardDetails{}, classifyStripeError(err)
	}
	if pi.PaymentMethod == nil || pi.PaymentMethod.Card == nil {
		// Non-card payment method (bank, wallet) or expansion
		// returned the bare ID — we don't render anything.
		return CardDetails{}, nil
	}
	// Prefer DisplayBrand: it accounts for dual-branded cards where
	// the customer chose which network to use (e.g. Cartes
	// Bancaires vs Visa on a French co-branded card). Fall back to
	// Brand when DisplayBrand isn't populated by older Stripe API
	// versions. Both fields are Stripe's lowercase enum form;
	// invoice.brandDisplayName handles the title-cased presentation.
	brand := pi.PaymentMethod.Card.DisplayBrand
	if brand == "" {
		brand = string(pi.PaymentMethod.Card.Brand)
	}
	return CardDetails{
		PaymentMethodID: pi.PaymentMethod.ID,
		Brand:           brand,
		Last4:           pi.PaymentMethod.Card.Last4,
		ExpMonth:        int(pi.PaymentMethod.Card.ExpMonth),
		ExpYear:         int(pi.PaymentMethod.Card.ExpYear),
	}, nil
}

func (c *LiveStripeClient) CancelPaymentIntent(ctx context.Context, paymentIntentID string) error {
	sc, err := c.client(ctx)
	if err != nil {
		return err
	}
	_, err = sc.V1PaymentIntents.Cancel(ctx, paymentIntentID, nil)
	if err != nil {
		return fmt.Errorf("stripe cancel: %s", stripeErrorMessage(err))
	}
	return nil
}

// GetPaymentIntent fetches the current state of a PaymentIntent. Used by the
// reconciler to resolve PaymentUnknown invoices — Stripe is the source of
// truth for whether a charge actually succeeded.
func (c *LiveStripeClient) GetPaymentIntent(ctx context.Context, paymentIntentID string) (PaymentIntentResult, error) {
	sc, err := c.client(ctx)
	if err != nil {
		return PaymentIntentResult{}, err
	}
	pi, err := sc.V1PaymentIntents.Retrieve(ctx, paymentIntentID, nil)
	if err != nil {
		return PaymentIntentResult{}, classifyStripeError(err)
	}
	return paymentIntentResult(pi), nil
}

// paymentIntentResult maps a retrieved PaymentIntent to what the reconciler
// reads: status, purpose, captured amount, the charge's anchor and, for a
// declined attempt, the bank's decline code and the card it declined
// (last_payment_error, #966).
func paymentIntentResult(pi *stripe.PaymentIntent) PaymentIntentResult {
	res := PaymentIntentResult{
		ID:                  pi.ID,
		Status:              string(pi.Status),
		ClientSecret:        pi.ClientSecret,
		CreatedAt:           pi.Created,
		Purpose:             pi.Metadata["velox_purpose"],
		InvoiceID:           pi.Metadata["velox_invoice_id"],
		AmountReceivedCents: pi.AmountReceived,
		AnchorAt:            anchorFromPIMetadata(pi.Metadata),
	}
	// Only an expanded latest_charge says whether it was refunded; an
	// unexpanded one carries just its id, and Refunded reads false.
	if pi.LatestCharge != nil && pi.LatestCharge.Refunded {
		res.FullyRefunded = true
	}
	if lpe := pi.LastPaymentError; lpe != nil {
		res.DeclineCode = declineCodeOf(string(lpe.Type), string(lpe.Code), string(lpe.DeclineCode))
		if lpe.PaymentMethod != nil {
			res.PaymentMethodID = lpe.PaymentMethod.ID
		}
	}
	return res
}

// anchorFromPIMetadata reads the contracted simulated instant off a retrieved
// PI, mirroring anchorFromEventPayload's read of the same key on the webhook
// path. Absence and unparseable values both yield the zero time and are simply
// ignored by the caller: a malformed anchor must degrade to the pin binding,
// never block a settlement.
func anchorFromPIMetadata(meta map[string]string) time.Time {
	raw := meta["velox_anchor_at"]
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ListPaymentIntentsForCustomer is the stale-hold sweep's read (ADR-127):
// every PaymentIntent of the Stripe customer created at or after createdGTE,
// each with its velox_invoice_id, status, captured amount and whether its
// latest charge was refunded in full (expand[]=data.latest_charge).
//
// Why List and not Search (Stripe's docs, fetched 2026-10-19): List is the
// read-after-write API — "Don't use search for read-after-write flows… use the
// various list APIs… These APIs aren't subject to the availability delays" —
// and it is offered in every country, while Search "isn't available to
// businesses in India". The sweep reads "no PaymentIntent for this invoice" as
// "Stripe never received the charge", which only a consistent read supports.
//
// An error on any page fails the whole read: a partial list is never
// returned, because a missing page could hold the payment. Errors are typed
// for the sweep: ErrStripeNotConfigured, ErrStripeAuthFailed (401, 403) and
// ErrStripeCustomerMissing (resource_missing on the customer) will not
// succeed on retry; anything else (5xx, 429, network, unreadable
// credentials) may, and a read Stripe never answered (network, time limit)
// is also ErrStripeUnreachable.
func (c *LiveStripeClient) ListPaymentIntentsForCustomer(ctx context.Context, stripeCustomerID string, createdGTE time.Time) ([]PaymentIntentResult, error) {
	sc, err := c.client(ctx)
	if err != nil {
		return nil, err
	}
	params := &stripe.PaymentIntentListParams{
		Customer:     stripe.String(stripeCustomerID),
		CreatedRange: &stripe.RangeQueryParams{GreaterThanOrEqual: createdGTE.Unix()},
	}
	params.Limit = stripe.Int64(100)
	params.AddExpand("data.latest_charge")
	var out []PaymentIntentResult
	for pi, err := range sc.V1PaymentIntents.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("list PaymentIntents of Stripe customer %s: %w", stripeCustomerID, classifyListError(err))
		}
		out = append(out, paymentIntentResult(pi))
	}
	return out, nil
}

// classifyListError types a failed PaymentIntent list for the stale-hold
// sweep: a 401, a 403 (a restricted key without PaymentIntent read access)
// and a missing customer will not heal on retry; a read Stripe never answered
// (no *stripe.Error: a network error, a time limit) is ErrStripeUnreachable;
// everything else is classifyStripeError's. The sweep retries all but the
// first three.
func classifyListError(err error) error {
	var se *stripe.Error
	if !errors.As(err, &se) {
		return fmt.Errorf("%w: %w", ErrStripeUnreachable, classifyStripeError(err))
	}
	switch {
	case se.HTTPStatusCode == http.StatusUnauthorized || se.HTTPStatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrStripeAuthFailed, errs.Scrub(se.Msg))
	case se.Code == stripe.ErrorCodeResourceMissing && (se.Param == "customer" || strings.Contains(se.Msg, "customer")):
		return fmt.Errorf("%w: %s", ErrStripeCustomerMissing, errs.Scrub(se.Msg))
	}
	return classifyStripeError(err)
}

// GetCheckoutSession reads a hosted Checkout Session's outcome (the hosted
// sweep, reconciler_hosted.go).
func (c *LiveStripeClient) GetCheckoutSession(ctx context.Context, sessionID string) (CheckoutSessionResult, error) {
	sc, err := c.client(ctx)
	if err != nil {
		return CheckoutSessionResult{}, err
	}
	sess, err := sc.V1CheckoutSessions.Retrieve(ctx, sessionID, nil)
	if err != nil {
		return CheckoutSessionResult{}, classifyStripeError(err)
	}
	out := CheckoutSessionResult{
		Status:        string(sess.Status),
		PaymentStatus: string(sess.PaymentStatus),
	}
	if sess.PaymentIntent != nil {
		out.PaymentIntentID = sess.PaymentIntent.ID
	}
	if sess.Customer != nil {
		out.CustomerID = sess.Customer.ID
	}
	return out, nil
}
