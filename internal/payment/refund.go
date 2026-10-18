package payment

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/stripe/stripe-go/v82"
)

// RefundError is a refund create that returned no refund. Unknown is the
// split classifyStripeError makes for charges (#965):
//
//   - true: Stripe's answer was lost or unclear (a timeout, a network error,
//     a Stripe 5xx, a key already in use). Stripe may have made the refund.
//     Stripe calls a 5xx result indeterminate
//     (https://docs.stripe.com/error-low-level#server-errors).
//   - false: Stripe refused (card_error, invalid_request_error, a 429), so no
//     refund was made.
//
// NotRun narrows false: Stripe ran nothing for this request. It refused
// before running it (a 429 rate limit, a 401 invalid API key), or Velox sent
// nothing (Stripe not configured for the mode). Stripe saves no result for
// such a request: "Errors that occur before execution begins, such as invalid
// parameters, an invalid API key or rate limiting, don't cache a result"
// (https://docs.stripe.com/error-low-level#content-errors). So when Retry
// refund resends an unclear request, a NotRun error says nothing about that
// earlier request.
//
// The credit-note service reads both through OutcomeUnknown and
// RequestNotRun, because it may not import this package.
type RefundError struct {
	Message string
	Unknown bool
	NotRun  bool
}

func (e *RefundError) Error() string { return e.Message }

// OutcomeUnknown reports whether Stripe may have made the refund.
func (e *RefundError) OutcomeUnknown() bool { return e.Unknown }

// RequestNotRun reports whether Stripe ran nothing for this request.
func (e *RefundError) RequestNotRun() bool { return e.NotRun }

// OperatorSafeMessage implements respond.SafeMessageError (ADR-026): a
// curated sentence, never Stripe's raw text.
func (e *RefundError) OperatorSafeMessage() string {
	if e.Unknown {
		return "Stripe didn't confirm whether the refund went through. Don't refund this payment in Stripe. Retry the refund later: Velox checks Stripe first and resends the same request, so the customer is refunded once."
	}
	return "Stripe didn't make the refund, so the customer was not refunded. Check the payment in Stripe, then retry the refund."
}

// OperatorErrorCode shares the charge path's code for "no clear answer", so
// an API caller and the dashboard treat both the same way.
func (e *RefundError) OperatorErrorCode() string {
	if e.Unknown {
		return "payment_outcome_unknown"
	}
	return ""
}

// classifyRefundError sorts a refund create error with classifyStripeError,
// plus two rules read from the HTTP status:
//
//   - 409 is unclear, whatever its type. Stripe answers 409 while another
//     request with the same idempotency key is still running, and Retry
//     refund resends an unclear request under its own key, so a 409 is the
//     answer "the first request may still make the refund". Its type is
//     invalid_request_error, which classifyStripeError reads as a refusal;
//     recording "failed" would free the next retry to use a new key and
//     refund twice. Stripe: "If a 409 indicates that another request with
//     the same key is still executing, wait and retry with that key"
//     (https://docs.stripe.com/error-low-level#content-errors). #1264 adds
//     the same rule to classifyStripeError for charges.
//   - 429 and 401 are refusals Stripe makes before it runs a request, so it
//     saves no result for them (same page): NotRun. A 429's real type,
//     rate_limit_error, is one classifyStripeError does not know, so alone it
//     would read as unclear. The charge path makes the same exception for
//     429 (CreatePaymentIntent).
func classifyRefundError(err error) *RefundError {
	var se *stripe.Error
	if errors.As(err, &se) {
		switch se.HTTPStatusCode {
		case http.StatusConflict:
			return &RefundError{Message: "stripe refund: conflict: " + stripeErrorMessage(err), Unknown: true}
		case http.StatusTooManyRequests, http.StatusUnauthorized:
			return &RefundError{Message: "stripe refund: not run: " + stripeErrorMessage(err), NotRun: true}
		}
	}
	pe := classifyStripeError(err)
	return &RefundError{Message: "stripe refund: " + pe.Message, Unknown: pe.Unknown}
}

// mapStripeRefundStatus collapses Stripe's 5 refund states into 3 of Velox's
// refund_status values (DB CHECK: none/pending/succeeded/failed/unknown; none
// and unknown are Velox's own and never come from Stripe). canceled→failed (the
// money returned to the platform balance, not the customer — operator-actionable);
// requires_action→pending (still in-flight). Shared by CreateRefund (create-time)
// and handleRefundUpdated (the async webhook truth) so both agree. Note: even
// `succeeded` means "submitted to the card network", NOT "on the cardholder
// statement" (5–10 business days, no confirming event).
// Takes the raw Stripe status STRING (stable API values) so the webhook handler
// in stripe.go — which abstracts stripe-go behind StripeClient and doesn't import
// it — can share this exact mapping.
func mapStripeRefundStatus(s string) domain.RefundStatus {
	switch s {
	case "succeeded":
		return domain.RefundSucceeded
	case "failed", "canceled":
		return domain.RefundFailed
	case "pending", "requires_action":
		return domain.RefundPending
	default:
		return domain.RefundPending // a status Stripe adds later → in-flight; the webhook corrects it
	}
}

// StripeRefunder processes refunds via the Stripe API.
// Mode-aware: selects live/test client per ctx livemode.
type StripeRefunder struct {
	clients *StripeClients
}

func NewStripeRefunder(clients *StripeClients) *StripeRefunder {
	if !clients.Has() {
		return nil
	}
	return &StripeRefunder{clients: clients}
}

// CreateRefund creates a Stripe refund for the given PaymentIntent.
//
// `idempotencyKey` is mandatory in production — same key + same params
// returns the existing refund without creating a duplicate. Velox passes
// `velox_cn_<credit_note_id>` so that a credit-note Issue() retry after
// a partial failure (e.g. DB hiccup on the post-refund credit grant
// step) hits Stripe's cache and gets back the original refund_id,
// not a second refund.
//
// Without this protection, the pre-fix shape was: Stripe refund
// succeeds → in-memory refund_id set → credit grant fails → function
// returns error → CN stays draft → operator retries → Stripe called
// again with no idempotency key → DUPLICATE refund → customer over-
// refunded (caught 2026-05-22).
// Returns the refund id AND Stripe's create-time status (mapped to a Velox
// refund_status). A card refund on a healthy balance returns `succeeded`
// synchronously; ACH/balance-constrained refunds legitimately return `pending`,
// whose terminal outcome (succeeded/failed) lands later via a refund webhook —
// so the caller must record what Stripe actually said, not a blanket success.
// creditNoteID stamps metadata velox_cn_id on the refund. That metadata is
// the ADOPTION key: Stripe v1 idempotency keys expire after ~24h, so the key
// alone cannot make a late retry converge on an earlier attempt — but a
// metadata-stamped refund can always be found again by FindRefundForCreditNote
// and adopted instead of duplicated.
// Every error is a *RefundError, which says whether Stripe may have made the
// refund anyway.
func (r *StripeRefunder) CreateRefund(ctx context.Context, paymentIntentID string, amountCents int64, idempotencyKey, creditNoteID string) (string, domain.RefundStatus, error) {
	sc := r.clients.ForCtx(ctx)
	if sc == nil {
		// Nothing was sent, so nothing was refunded.
		return "", "", &RefundError{Message: "stripe refund: " + ErrStripeNotConfigured.Message, NotRun: true}
	}
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(paymentIntentID),
		Amount:        stripe.Int64(amountCents),
	}
	if creditNoteID != "" {
		params.Metadata = map[string]string{"velox_cn_id": creditNoteID}
	}
	if idempotencyKey != "" {
		params.IdempotencyKey = stripe.String(idempotencyKey)
	}
	ref, err := sc.V1Refunds.Create(ctx, params)
	if err != nil {
		return "", "", classifyRefundError(err)
	}

	return ref.ID, mapStripeRefundStatus(string(ref.Status)), nil
}

// GetRefund reads a refund's CURRENT provider state. This is a plain read —
// unlike an idempotency-key replay of the create call, it can never return a
// stale saved response, which is what makes it safe to consult before a
// retry. The second return is Stripe's failure_reason (empty unless failed).
func (r *StripeRefunder) GetRefund(ctx context.Context, refundID string) (domain.RefundStatus, string, error) {
	sc := r.clients.ForCtx(ctx)
	if sc == nil {
		return "", "", ErrStripeNotConfigured
	}
	ref, err := sc.V1Refunds.Retrieve(ctx, refundID, nil)
	if err != nil {
		return "", "", fmt.Errorf("stripe get refund: %s", stripeErrorMessage(err))
	}
	return mapStripeRefundStatus(string(ref.Status)), string(ref.FailureReason), nil
}

// FindRefundForCreditNote searches the PaymentIntent's refunds for a LIVE
// (succeeded/pending) refund minted for this credit note (metadata
// velox_cn_id). Returns ("", "", nil) when none exists. This is the search
// half of search-and-adopt (ADR-108's pattern, applied to refunds): it
// recovers the lost-response shapes — a create that errored on the wire
// after Stripe minted the refund, in either the original Issue leg (Velox
// holds no id) or a later re-drive (Velox still holds a dead predecessor).
//
// Failed matches are deliberately NOT returned. Adopting a live twin is what
// prevents a duplicate; a failed twin is a dead prior attempt, and returning
// it would let a pile of dead attempts permanently shadow the create leg
// (adopt returns early), livelocking the retry — the finder review caught
// exactly that with an exclude-one-id variant. Dead attempts stay visible in
// the audit trail, which records every id and failure reason as it happened.
func (r *StripeRefunder) FindRefundForCreditNote(ctx context.Context, paymentIntentID, creditNoteID string) (string, domain.RefundStatus, error) {
	sc := r.clients.ForCtx(ctx)
	if sc == nil {
		return "", "", ErrStripeNotConfigured
	}
	params := &stripe.RefundListParams{PaymentIntent: stripe.String(paymentIntentID)}
	for ref, err := range sc.V1Refunds.List(ctx, params) {
		if err != nil {
			return "", "", fmt.Errorf("stripe list refunds: %s", stripeErrorMessage(err))
		}
		if ref.Metadata["velox_cn_id"] != creditNoteID {
			continue
		}
		if status := mapStripeRefundStatus(string(ref.Status)); status != domain.RefundFailed {
			return ref.ID, status, nil
		}
	}
	return "", "", nil
}
