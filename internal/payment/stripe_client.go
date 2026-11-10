package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
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
	// CardMessage is Stripe's message when the error is a card error, the
	// one kind of message Stripe says can be shown to the customer
	// (cardErrorMessage); empty otherwise.
	CardMessage string
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
	if domain.IsAuthenticationRequired(e.DeclineCode) {
		// Not a decline: the card works once the customer confirms the
		// payment, which a charge made without them can't do.
		return customerMustConfirmMessage
	}
	if e.DeclineCode != "" {
		return "Card was declined: " + declineCodeToOperatorMessage(string(e.DeclineCode))
	}
	if e.Unknown {
		return "Stripe didn't confirm whether the charge went through. Hardcount will check with Stripe and update the invoice; don't charge again until it does."
	}
	return "Payment provider rejected the request. Please retry; if the problem persists, contact support."
}

// customerMustConfirmMessage is what Collect payment answers when Stripe
// returns authentication_required.
const customerMustConfirmMessage = "The customer's bank needs them to confirm this payment. Email them the payment link; they pay and confirm it on the hosted invoice page."

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
// A 429 speaks only for the request it answers. When stripe-go resent a
// confirm after a network error, the first send under the same key may still
// be running at Stripe and charge the card (#976). The invoice already names
// the PaymentIntent, so the unknown sweep reads what it did. A 429 to the
// create is harmless: at most an unconfirmed PaymentIntent exists.
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
	// AtConfirm: the key was refused at the confirm, after the invoice was
	// stamped unknown on its PaymentIntent. The invoice is not unchanged, and
	// collecting again is refused until the unknown sweep has read that
	// PaymentIntent, which needs a working key.
	AtConfirm bool
}

// Reasons of a StripeKeyError; also the label of
// hardcount_stripe_key_unusable_total.
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
	if e.AtConfirm {
		return "Stripe refused this account's API key while the payment was being confirmed, so its outcome is not known yet. Update the key under Settings → Payments. Hardcount then reads the payment from Stripe and records the result; collecting again is refused until it has."
	}
	return "Stripe refused this account's API key (or none is connected for this mode). Nothing was charged. Update the key under Settings → Payments, then collect again."
}

// OperatorErrorCode names the fault so a caller can tell it from a decline.
func (e *StripeKeyError) OperatorErrorCode() string { return "stripe_key_unusable" }

// BelowMinimumError is Stripe refusing a charge, or a payment-mode Checkout
// Session, because the amount is below Stripe's minimum charge: HTTP 400,
// invalid_request_error, code amount_too_small (seen on a test-mode account,
// 2026-10-22: "Amount must be at least $0.50 usd"). Stripe makes no
// PaymentIntent and no session, so nothing was charged, and asking again
// gets the same answer with any card. domain.ErrBelowMinimumCharge says what
// Hardcount does with it.
//
// It must not be or wrap a *PaymentError, for the reason StripeKeyError
// gives: the auto-charge sweep, the dunning retry and Collect all read a
// *PaymentError with Unknown=false as a decline. Nor ErrPaymentTransient:
// that leaves the invoice queued, and the sweep would ask Stripe every tick.
type BelowMinimumError struct {
	Message string // Stripe's sentence, scrubbed; it names the minimum
}

func (e *BelowMinimumError) Error() string {
	return "below Stripe's minimum charge: " + e.Message
}

func (e *BelowMinimumError) Unwrap() error { return domain.ErrBelowMinimumCharge }

// belowMinimumOf reads Stripe's amount_too_small refusal off err, or nil.
// Keyed on the error code, not the message: the code is Stripe's contract.
func belowMinimumOf(err error) *BelowMinimumError {
	var se *stripe.Error
	if errors.As(err, &se) && se.Code == stripe.ErrorCodeAmountTooSmall {
		return &BelowMinimumError{Message: stripeErrorMessage(err)}
	}
	return nil
}

// BelowMinimumOf is belowMinimumOf for the hosted page's Checkout Session
// create, which calls stripe-go itself (api/adapters.go).
func BelowMinimumOf(err error) *BelowMinimumError { return belowMinimumOf(err) }

// IsDefiniteRefusal reports whether Stripe answered a create request with a
// refusal that proves it created nothing: a 4xx other than 409 (a request
// with the same idempotency key may still be running, #976) and 429 (it
// speaks only for this request), and not an idempotency_error (the key was
// used before, so an earlier request reached Stripe). A network error, a 5xx
// or anything that is not a Stripe error is not one.
func IsDefiniteRefusal(err error) bool {
	var se *stripe.Error
	if !errors.As(err, &se) {
		return false
	}
	switch {
	case se.HTTPStatusCode < 400 || se.HTTPStatusCode >= 500:
		return false
	case se.HTTPStatusCode == http.StatusConflict || se.HTTPStatusCode == http.StatusTooManyRequests:
		return false
	case se.Type == stripe.ErrorTypeIdempotency:
		return false
	}
	return true
}

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

// declineCodeOf is the code that says why Stripe refused a card: the error's
// decline_code, or, on a card_error that has none, the error's code when that
// code stops dunning from charging the card again: a hard decline
// (domain.IsHardDecline, #966) or authentication_required
// (domain.IsAuthenticationRequired). Stripe sends incorrect_number either
// way: as the bank's decline code under card_declined, or as its own error
// code with no decline code
// (https://docs.stripe.com/testing#declined-payments, "Incorrect number
// decline"). authentication_required is a decline code and an error code
// too (https://docs.stripe.com/declines/codes,
// https://docs.stripe.com/error-codes). Any other error code stays out, so
// that decline keeps Stripe's own message and the ordinary retry schedule,
// as before.
func declineCodeOf(errType, code, declineCode string) string {
	// error_on_requires_action fails a payment the bank wants confirmed with
	// code authentication_required and decline code
	// authentication_not_handled. It is the same ask of the customer as the
	// bank's own authentication_required decline, so it is read as that one.
	if errType == string(stripe.ErrorTypeCard) && code == domain.AuthenticationRequiredCode {
		return domain.AuthenticationRequiredCode
	}
	if declineCode != "" {
		return declineCode
	}
	if errType == string(stripe.ErrorTypeCard) && domain.RetryBlock(code) != nil {
		return code
	}
	return ""
}

// cardErrorMessage is Stripe's message for a card error, scrubbed, and ""
// for any other error type. Stripe: "For card errors, these messages can be
// shown to your users" (https://docs.stripe.com/api/errors, message). The
// other types' messages are written for the developer ("This
// PaymentIntent's payment_method could not be updated"), so the customer's
// payment-failed email shows only this one (customerFailureReason).
func cardErrorMessage(errType, msg string) string {
	if errType != string(stripe.ErrorTypeCard) {
		return ""
	}
	return errs.Scrub(msg)
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
		CardMessage: cardErrorMessage(string(stripeErr.Type), stripeErr.Msg),
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
		// where RetryBlockingDecline reads it and stops dunning from charging
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

// ErrNoSavedCard is FetchCardDetails' "the Stripe customer has no card" — a
// one-off payment where the customer did not save the card. It is the only
// FetchCardDetails error that means "nothing to record"; any other is a
// failed lookup and must be retried.
var ErrNoSavedCard = errors.New("no saved card on the Stripe customer")

// LiveStripeClient wraps the Stripe SDK for PaymentIntent operations. Despite
// the "Live" in the name, it handles both live and test modes: each call
// resolves the tenant and mode on ctx through StripeClients.ResolveCtx. A
// caller outside a request (a background job, a sweep) must set the tenant
// first with auth.WithTenantID, or the call fails and nothing reaches Stripe.
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

// CreateUnconfirmedPaymentIntent creates one charge attempt's PaymentIntent
// without confirming it, so no money can move (ADR-137). The caller puts its
// id on the invoice, then calls ConfirmPaymentIntent. The
// confirm-time settings (off_session, error_on_requires_action) go on the
// confirm: Stripe takes off_session only together with confirmation.
func (c *LiveStripeClient) CreateUnconfirmedPaymentIntent(ctx context.Context, params PaymentIntentParams) (PaymentIntentResult, error) {
	sc, err := c.client(ctx)
	if errors.Is(err, ErrStripeNotConfigured) {
		return PaymentIntentResult{}, &StripeKeyError{Reason: StripeKeyNotConfigured}
	}
	if err != nil {
		return PaymentIntentResult{}, err
	}
	if params.PaymentMethodID == "" {
		return PaymentIntentResult{}, &PaymentError{Message: "customer has no payment method on file"}
	}
	metadata := make(map[string]string, len(params.Metadata))
	for k, v := range params.Metadata {
		metadata[k] = v
	}
	pi, err := sc.V1PaymentIntents.Create(ctx, &stripe.PaymentIntentCreateParams{
		Amount:             stripe.Int64(params.AmountCents),
		Currency:           stripe.String(params.Currency),
		Customer:           stripe.String(params.CustomerID),
		PaymentMethod:      stripe.String(params.PaymentMethodID),
		PaymentMethodTypes: stripe.StringSlice([]string{"card"}),
		Params: stripe.Params{
			IdempotencyKey: stripe.String(SentIdempotencyKey(params.IdempotencyKey, params.PaymentMethodID)),
			Metadata:       metadata,
		},
		Description: stripe.String(params.Description),
	})
	if err != nil {
		return PaymentIntentResult{}, classifyPaymentIntentWriteError(err)
	}
	return PaymentIntentResult{ID: pi.ID, Status: string(pi.Status), ClientSecret: pi.ClientSecret}, nil
}

// ConfirmPaymentIntent confirms a PaymentIntent that CreateUnconfirmedPaymentIntent
// made: the step where money moves. Every error it returns names the
// PaymentIntent, so an unclear answer is never one Hardcount cannot look up.
//
// A confirm Stripe refuses as payment_intent_unexpected_state is not a
// decline: an earlier confirm of the same PaymentIntent already moved it on
// (stripe-go resent after a lost reply, or a retry of this attempt). Read by
// its type (invalid_request_error) it would mark a paid invoice failed, so
// the PaymentIntent is read and its real state returned instead.
func (c *LiveStripeClient) ConfirmPaymentIntent(ctx context.Context, paymentIntentID string, params PaymentIntentParams) (PaymentIntentResult, error) {
	sc, err := c.client(ctx)
	if errors.Is(err, ErrStripeNotConfigured) {
		return PaymentIntentResult{}, &StripeKeyError{Reason: StripeKeyNotConfigured}
	}
	if err != nil {
		return PaymentIntentResult{}, err
	}
	pi, err := sc.V1PaymentIntents.Confirm(ctx, paymentIntentID, &stripe.PaymentIntentConfirmParams{
		PaymentMethod:         stripe.String(params.PaymentMethodID),
		OffSession:            stripe.Bool(true),
		ErrorOnRequiresAction: stripe.Bool(true),
		Params: stripe.Params{
			IdempotencyKey: stripe.String(SentIdempotencyKey(params.IdempotencyKey, params.PaymentMethodID) + "_confirm"),
		},
	})
	if err != nil {
		var se *stripe.Error
		if errors.As(err, &se) && string(se.Code) == "payment_intent_unexpected_state" {
			got, gerr := c.GetPaymentIntent(ctx, paymentIntentID)
			if gerr == nil && (got.Status == domain.ProviderPISucceeded || got.Status == domain.ProviderPIProcessing) {
				return got, nil
			}
			return PaymentIntentResult{}, &PaymentError{Message: stripeErrorMessage(err), PaymentIntentID: paymentIntentID, Unknown: true}
		}
		// A definite refusal names the PaymentIntent only when it is a card
		// error: a bank's answer about this payment. A request Stripe refused
		// outright (an invalid request) names none, whatever its error body
		// carries: Stripe puts the payment_intent object on errors of a
		// request that involves one, a refused confirm included. The caller
		// tells the two apart by this field: only a decline is reported as a
		// failed payment, with a customer email.
		werr := classifyPaymentIntentWriteError(err)
		var pe *PaymentError
		if errors.As(werr, &pe) && !pe.Unknown {
			if se != nil && se.Type == stripe.ErrorTypeCard {
				pe.PaymentIntentID = paymentIntentID
			} else {
				pe.PaymentIntentID = ""
			}
		}
		return PaymentIntentResult{}, werr
	}
	return PaymentIntentResult{ID: pi.ID, Status: string(pi.Status), ClientSecret: pi.ClientSecret}, nil
}

// classifyPaymentIntentWriteError maps a create or confirm error: a 429 is transient, a refused key is a key error,
// below Stripe's minimum is its own error, the rest by classifyStripeError.
func classifyPaymentIntentWriteError(err error) error {
	if isRateLimited(err) {
		return fmt.Errorf("%w: stripe rate limit: %v", ErrPaymentTransient, errs.Scrub(err.Error()))
	}
	var se *stripe.Error
	if errors.As(err, &se) && (se.HTTPStatusCode == http.StatusUnauthorized || se.HTTPStatusCode == http.StatusForbidden) {
		return &StripeKeyError{Reason: StripeKeyRejected, Message: errs.Scrub(se.Msg)}
	}
	if bm := belowMinimumOf(err); bm != nil {
		return bm
	}
	return classifyStripeError(err)
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

	// Read only. This used to set the newest card as Stripe's default,
	// ignoring errors, which split Stripe's copy from Hardcount's default (the
	// card Hardcount charges). Stripe's default now follows Hardcount's, set by the
	// payment-methods service whenever Hardcount's default changes (ADR-053).
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
// the newest card on the Stripe customer), this works for one-off Checkout
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

// GetPaymentIntent fetches the current state of a PaymentIntent. The unknown
// and processing sweeps read it: Stripe is the source of truth for what a
// charge did. A failed read (a refused key, a PaymentIntent not in this
// account, a 5xx, no answer) is an error the sweep reports and retries on a
// later tick; the invoice stays as it is.
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
		Purpose:             pi.Metadata["hardcount_purpose"],
		InvoiceID:           pi.Metadata["hardcount_invoice_id"],
		TenantID:            pi.Metadata["hardcount_tenant_id"],
		AmountReceivedCents: pi.AmountReceived,
		AnchorAt:            anchorFromPIMetadata(pi.Metadata),
	}
	// latest_charge is set (unexpanded: its id) once a confirm reached the
	// card network.
	if pi.LatestCharge != nil {
		res.Charged = true
	}
	if lpe := pi.LastPaymentError; lpe != nil {
		res.DeclineCode = declineCodeOf(string(lpe.Type), string(lpe.Code), string(lpe.DeclineCode))
		res.CardMessage = cardErrorMessage(string(lpe.Type), lpe.Msg)
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
	raw := meta["hardcount_anchor_at"]
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
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
