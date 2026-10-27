package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/payment"
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
