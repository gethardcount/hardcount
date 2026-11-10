//go:build stripelive

package payment

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
)

// Two-step charge against real Stripe TEST mode, through LiveStripeClient
// (ADR-137). Run only on purpose:
//
//	STRIPE_TEST_SECRET_KEY_FILE=path go test -tags stripelive -run TestTwoStepLive ./internal/payment/
//
// The file holds one sk_test_/rk_test_ key; anything else is refused.

type liveKey struct{ key string }

func (k liveKey) GetPlaintext(context.Context, string, bool) (tenantstripe.PlaintextSecrets, error) {
	return tenantstripe.PlaintextSecrets{SecretKey: k.key}, nil
}

func liveRig(t *testing.T) (*LiveStripeClient, *stripe.Client, context.Context) {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("STRIPE_TEST_SECRET_KEY_FILE"))
	if err != nil {
		t.Skip("no STRIPE_TEST_SECRET_KEY_FILE")
	}
	key := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(key, "sk_test_") && !strings.HasPrefix(key, "rk_test_") {
		t.Fatal("refusing: the key is not a test-mode key")
	}
	ctx := auth.WithTenantID(postgres.WithLivemode(context.Background(), false), "hc_ten_live_test")
	return NewLiveStripeClient(NewStripeClients(liveKey{key})), stripe.NewClient(key), ctx
}

func liveCustomerWithCard(t *testing.T, sc *stripe.Client, ctx context.Context, testPM string) (string, string) {
	t.Helper()
	cus, err := sc.V1Customers.Create(ctx, &stripe.CustomerCreateParams{
		Name:   stripe.String("PI design live test"),
		Params: stripe.Params{Metadata: map[string]string{"hardcount_test": "two_step_live"}},
	})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	pm, err := sc.V1PaymentMethods.Attach(ctx, testPM, &stripe.PaymentMethodAttachParams{Customer: stripe.String(cus.ID)})
	if err != nil {
		t.Fatalf("attach %s: %v", testPM, err)
	}
	return cus.ID, pm.ID
}

func liveParams(cus, pm, key string, amount int64) PaymentIntentParams {
	return PaymentIntentParams{
		AmountCents: amount, Currency: "usd", CustomerID: cus, PaymentMethodID: pm,
		Description: "PI design live test", IdempotencyKey: key,
		Metadata: map[string]string{"hardcount_test": "two_step_live", "hardcount_invoice_id": "hc_inv_live_test"},
	}
}

func TestTwoStepLive(t *testing.T) {
	c, sc, ctx := liveRig(t)
	run := "_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	t.Run("create unconfirmed, confirm, resend after the key is gone", func(t *testing.T) {
		cus, pm := liveCustomerWithCard(t, sc, ctx, "pm_card_visa")
		p := liveParams(cus, pm, "hc_live_ok_"+cus+run, 5000)
		created, err := c.CreateUnconfirmedPaymentIntent(ctx, p)
		if err != nil || created.Status != "requires_confirmation" {
			t.Fatalf("create: %+v, %v; want requires_confirmation", created, err)
		}
		got, err := c.ConfirmPaymentIntent(ctx, created.ID, p)
		if err != nil || got.Status != domain.ProviderPISucceeded {
			t.Fatalf("confirm: %+v, %v; want succeeded", got, err)
		}
		// Same key: Stripe replays its saved answer.
		if again, err := c.ConfirmPaymentIntent(ctx, created.ID, p); err != nil || again.Status != domain.ProviderPISucceeded {
			t.Fatalf("resend, same key: %+v, %v; want the saved success", again, err)
		}
		// Another key (as after Stripe pruned the first): Stripe answers
		// payment_intent_unexpected_state, which must come back as the
		// PaymentIntent's real state, never a decline.
		p2 := p
		p2.IdempotencyKey += "_pruned"
		again, err := c.ConfirmPaymentIntent(ctx, created.ID, p2)
		if err != nil || again.Status != domain.ProviderPISucceeded || again.ID != created.ID {
			t.Fatalf("resend, new key: %+v, %v; want succeeded on %s", again, err, created.ID)
		}
	})

	t.Run("a decline names its PaymentIntent and is definite", func(t *testing.T) {
		cus, pm := liveCustomerWithCard(t, sc, ctx, "pm_card_chargeCustomerFail")
		p := liveParams(cus, pm, "hc_live_decl_"+cus+run, 5000)
		created, err := c.CreateUnconfirmedPaymentIntent(ctx, p)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		_, err = c.ConfirmPaymentIntent(ctx, created.ID, p)
		var pe *PaymentError
		if !errors.As(err, &pe) || pe.Unknown || pe.PaymentIntentID != created.ID || pe.DeclineCode == "" {
			t.Fatalf("decline: %#v; want a definite PaymentError naming %s with a decline code", err, created.ID)
		}
	})

	t.Run("a card that needs 3D Secure fails off-session and names its PaymentIntent", func(t *testing.T) {
		cus, pm := liveCustomerWithCard(t, sc, ctx, "pm_card_authenticationRequired")
		p := liveParams(cus, pm, "hc_live_3ds_"+cus+run, 5000)
		created, err := c.CreateUnconfirmedPaymentIntent(ctx, p)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		_, err = c.ConfirmPaymentIntent(ctx, created.ID, p)
		var pe *PaymentError
		if !errors.As(err, &pe) || pe.Unknown || pe.PaymentIntentID != created.ID {
			t.Fatalf("3DS: %#v; want a definite PaymentError naming %s", err, created.ID)
		}
	})

	t.Run("below Stripe's minimum fails at the create, before any confirm", func(t *testing.T) {
		cus, pm := liveCustomerWithCard(t, sc, ctx, "pm_card_visa")
		_, err := c.CreateUnconfirmedPaymentIntent(ctx, liveParams(cus, pm, "hc_live_min_"+cus+run, 10))
		var bm *BelowMinimumError
		if !errors.As(err, &bm) {
			t.Fatalf("below minimum: %#v; want BelowMinimumError", err)
		}
	})
}
