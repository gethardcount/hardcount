package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/payment"
)

// TestClassifyDunningRetryResult locks which retry results use up a dunning
// attempt: only a definite decline, no card, or a card refused for good.
// Everything else skips uncounted. A counted decline comes back as it went
// in, so the PaymentIntent id and the new-card flag reach dunning.
func TestClassifyDunningRetryResult(t *testing.T) {
	decline := &payment.PaymentError{Message: "Your card was declined.", DeclineCode: "lost_card", PaymentIntentID: "pi_lost"}
	wrappedDecline := &dunning.RetryError{PaymentIntentID: "pi_lost", Err: fmt.Errorf("payment failed: %w", decline), Ask: domain.DeclineAskNewCard}
	tests := []struct {
		name    string
		err     error
		counted bool // the classifier returns err itself; otherwise ErrTransientSkip
	}{
		{"nil", nil, true},
		{"already a skip", dunning.ErrTransientSkip, false},
		{"plain database error", fmt.Errorf("get invoice: %w", errors.New("sql: database is closed")), false},
		{"connection dropped", fmt.Errorf("claim dunning retry charge: %w", &pgconn.PgError{Code: "57P01"}), false},
		{"breaker open or rate limited", payment.ErrPaymentTransient, false},
		{"Stripe credentials unreadable", payment.ErrStripeCredentialsUnavailable, false},
		{"outcome unknown", fmt.Errorf("payment state unknown: %w", &payment.PaymentError{Unknown: true}), false},
		{"settle failed after Stripe took the money", fmt.Errorf("settle succeeded inline: %w", errors.New("mark invoice paid: conn closed")), false},
		{"definite decline", fmt.Errorf("payment failed: %w", decline), true},
		{"definite decline with its PaymentIntent", wrappedDecline, true},
		{"no card", domain.ErrNoPaymentMethodOnRetry, true},
		{"card refused for good", domain.ErrCardNeedsReplacing, true},
		// Not a skip: a skip leaves the run due, and the next tick would ask
		// Stripe again forever. Dunning ends the run on it.
		{"below Stripe's minimum charge", &payment.BelowMinimumError{Message: "Amount must be at least $0.50 usd"}, true},
		// Stripe was asked and a PaymentIntent exists: the attempt counts and
		// the run waits for the payment's outcome (#1289).
		{"payment still processing", &dunning.RetryError{PaymentIntentID: "pi_slow", Err: dunning.ErrPaymentInFlight}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyDunningRetryResult(context.Background(), "inv_1", tc.err)
			if !tc.counted {
				if got != dunning.ErrTransientSkip {
					t.Fatalf("got %v, want ErrTransientSkip (not counted)", got)
				}
				return
			}
			if got != tc.err {
				t.Fatalf("got %v, want the error itself (counted)", got)
			}
		})
	}

	got := classifyDunningRetryResult(context.Background(), "inv_1", wrappedDecline)
	var re *dunning.RetryError
	if !errors.As(got, &re) || re.PaymentIntentID != "pi_lost" || re.Ask != domain.DeclineAskNewCard {
		t.Fatalf("got %#v, want the RetryError with pi_lost asking for a new card", got)
	}
}

// TestDefiniteRetryFailure_ReasonLineIsTheCardMessage: a dunning retry Stripe
// definitely refused reaches dunning as a RetryError, and the customer's
// warning email shows a reason only when Stripe sent a card message, the one
// message Stripe says can be shown to the customer. Stripe's 400 to the
// charge request itself (here the error_on_requires_action refusal, which has
// no PaymentIntent and no card message) used to reach the customer as the
// email's "Reason:". Mutation: drop NoReason, and the request refusal's case
// fails.
func TestDefiniteRetryFailure_ReasonLineIsTheCardMessage(t *testing.T) {
	const devSentence = "The `error_on_requires_action` parameter can't be used with PaymentIntents which use payment methods managed through the Dashboard at https://dashboard.stripe.com/account/payments/settings."
	tests := []struct {
		name     string
		pe       *payment.PaymentError
		wantPI   string
		wantAsk  domain.DeclineAsk
		noReason bool
	}{
		{"card decline", &payment.PaymentError{Message: "Your card was declined.", DeclineCode: "generic_decline", PaymentIntentID: "pi_1", CardMessage: "Your card was declined."}, "pi_1", domain.DeclineAskNone, false},
		{"card refused for good", &payment.PaymentError{Message: "Your card was declined.", DeclineCode: "lost_card", PaymentIntentID: "pi_2", CardMessage: "Your card was declined."}, "pi_2", domain.DeclineAskNewCard, false},
		{"request refused, no PaymentIntent", &payment.PaymentError{Message: devSentence}, "", domain.DeclineAskNone, true},
		{"non-card refusal naming a PaymentIntent", &payment.PaymentError{Message: "This PaymentIntent's payment_method could not be updated.", PaymentIntentID: "pi_3"}, "pi_3", domain.DeclineAskNone, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("payment failed: %w", tc.pe)
			re := definiteRetryFailure(err)
			if re == nil {
				t.Fatalf("definiteRetryFailure(%v) = nil, want a RetryError", err)
			}
			if re.PaymentIntentID != tc.wantPI || re.Ask != tc.wantAsk || re.NoReason != tc.noReason {
				t.Errorf("got PI %q ask %q NoReason %v, want PI %q ask %q NoReason %v",
					re.PaymentIntentID, re.Ask, re.NoReason, tc.wantPI, tc.wantAsk, tc.noReason)
			}
			if !errors.Is(re, err) {
				t.Error("the RetryError does not wrap the charge's error")
			}
			// Still a counted attempt, as before.
			if got := classifyDunningRetryResult(context.Background(), "inv_1", re); got != error(re) {
				t.Errorf("classified as %v, want counted as it is", got)
			}
		})
	}

	for _, err := range []error{
		nil,
		fmt.Errorf("payment state unknown: %w", &payment.PaymentError{Unknown: true, Message: "timeout"}),
		payment.ErrPaymentTransient,
		&payment.BelowMinimumError{Message: "Amount must be at least $0.50 usd"},
		errors.New("get invoice: conn closed"),
	} {
		if re := definiteRetryFailure(err); re != nil {
			t.Errorf("definiteRetryFailure(%v) = %#v, want nil: not a definite refusal", err, re)
		}
	}
}
