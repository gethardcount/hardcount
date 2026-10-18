package payment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/payment/breaker"
)

// The charge record on the charge path (ADR-127, #976), against the mock
// store's model of the record CAS. The same paths on real Postgres, with
// crashes, timeouts and the reconciler's check:
// interrupted_charge_integration_test.go.

// The key value is unchanged by building it in one place: the card suffix
// moved from LiveStripeClient into ChargeIdempotencyKey. A changed value would
// make every attempt in flight across the upgrade send a new key.
func TestChargeIdempotencyKey_Value(t *testing.T) {
	for _, tc := range []struct{ purpose, want string }{
		{"", "velox_inv_inv_1_7_pm_abc"},
		{piPurposeDunningRetry, "velox_inv_inv_1_7_dunning_retry_pm_abc"},
	} {
		if got := ChargeIdempotencyKey("inv_1", 7, tc.purpose, "pm_abc"); got != tc.want {
			t.Errorf("ChargeIdempotencyKey(purpose %q) = %q, want %q", tc.purpose, got, tc.want)
		}
	}
}

// "Not found" is final only because no request for a record can start, or be
// retried by stripe-go, after the check looks: a charge sends within
// chargeSendBudget of its record, and stripe-go sends at most three requests
// inside it. The check waits interruptedChargeMinAge.
func TestChargeRecord_TimeBudgets(t *testing.T) {
	if 3*chargeSendBudget >= interruptedChargeMinAge {
		t.Fatalf("3 × chargeSendBudget (%s) must be under interruptedChargeMinAge (%s): a request could still be sent after the check found nothing",
			3*chargeSendBudget, interruptedChargeMinAge)
	}
	if interruptedChargeMinAge >= interruptedChargeGiveUpAge {
		t.Fatalf("the check (%s) must start before the give-up age (%s)", interruptedChargeMinAge, interruptedChargeGiveUpAge)
	}
}

func recordFixture() (*mockInvoiceUpdater, domain.Invoice) {
	invoices := newMockInvoiceUpdater()
	snapshot := pendingSnapshot()
	snapshot.InvoiceNumber = "INV-1"
	invoices.invoices["inv_1"] = snapshot
	return invoices, snapshot
}

// The record is on the invoice when Stripe receives the request, and it holds
// the exact key Stripe receives, which the PaymentIntent also carries as
// velox_attempt_key.
func TestChargeInvoice_RecordsTheExactKeyBeforeStripe(t *testing.T) {
	for _, purpose := range []string{"", piPurposeDunningRetry} {
		t.Run("purpose "+purpose, func(t *testing.T) {
			invoices, snapshot := recordFixture()
			var atCreate mockChargeRecord
			var heldAtCreate bool
			client := &mockStripeClient{piID: "pi_1", chargeStatus: "succeeded"}
			client.onCreate = func(PaymentIntentParams) {
				atCreate, heldAtCreate = invoices.records["inv_1"]
			}
			s := NewStripe(client, invoices, newMockWebhookStore(), nil)

			var err error
			if purpose == piPurposeDunningRetry {
				_, err = s.ChargeInvoiceForDunningRetry(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
			} else {
				_, err = s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
			}
			if err != nil {
				t.Fatalf("charge: %v", err)
			}

			want := ChargeIdempotencyKey("inv_1", snapshot.ChargeAttemptSeq, purpose, "pm_test")
			if !heldAtCreate {
				t.Fatal("no charge record on the invoice when Stripe received the request: a crash during the call would leave nothing saying a charge was sent")
			}
			if atCreate.key != want || atCreate.seq != snapshot.ChargeAttemptSeq || atCreate.stripeCustomerID != "cus_stripe" {
				t.Errorf("record = %+v, want key %q, seq %d, Stripe customer cus_stripe", atCreate, want, snapshot.ChargeAttemptSeq)
			}
			if client.lastParams.IdempotencyKey != want {
				t.Errorf("idempotency key sent = %q, want %q", client.lastParams.IdempotencyKey, want)
			}
			if got := client.lastParams.Metadata["velox_attempt_key"]; got != want {
				t.Errorf("velox_attempt_key metadata = %q, want %q: the check matches the record to its PaymentIntent by it", got, want)
			}
			if invoices.recordHeld("inv_1") {
				t.Error("record still on the invoice after the payment was saved")
			}
		})
	}
}

// Fail closed: when the record cannot be written, or this charger no longer
// holds the invoice, Stripe is never called.
func TestChargeInvoice_NoRecordNoCharge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *mockInvoiceUpdater)
	}{
		{"the record write fails", func(m *mockInvoiceUpdater) { m.beginErr = errors.New("connection refused") }},
		{"the charge hold lapsed (a charger stalled past five minutes)", func(m *mockInvoiceUpdater) { m.holdLapsed["inv_1"] = true }},
		{"another charge's record is already there", func(m *mockInvoiceUpdater) {
			inv := m.invoices["inv_1"]
			started := time.Now().UTC()
			inv.ChargeStartedAt = &started
			m.invoices["inv_1"] = inv
			m.records["inv_1"] = mockChargeRecord{seq: inv.ChargeAttemptSeq, key: "other"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invoices, snapshot := recordFixture()
			tc.setup(invoices)
			client := &mockStripeClient{piID: "pi_1", chargeStatus: "succeeded"}
			s := NewStripe(client, invoices, newMockWebhookStore(), nil)

			_, err := s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
			if !errors.Is(err, ErrPaymentTransient) {
				t.Errorf("err = %v, want ErrPaymentTransient (nothing was charged; try again later)", err)
			}
			if client.createCalls != 0 {
				t.Fatalf("Stripe called %d times without a charge record", client.createCalls)
			}
			if len(invoices.leaseReleases) != 1 {
				t.Errorf("charge hold released %d times, want 1: nothing was sent", len(invoices.leaseReleases))
			}
		})
	}
}

// One rule for clearing (ADR-127): clear the record only once the attempt's
// result is saved on the invoice, or when nothing was sent. Every unclear
// answer keeps the record and writes nothing else, except the "same key,
// different parameters" 409, which parks as before (ADR-107).
func TestChargeInvoice_ClearsTheRecordOnlyOnceTheResultIsSaved(t *testing.T) {
	keyInUse := &PaymentError{Message: "The idempotency key provided is currently being used in another request.", Unknown: true}
	paramsDiffer := &PaymentError{Message: "Keys for idempotent requests can only be used with the same parameters.", Unknown: true, IdempotencyConflict: true}
	paidDuringCall := func(m *mockInvoiceUpdater) {
		m.duringCall = func(m *mockInvoiceUpdater) {
			paidAt := time.Now().UTC()
			row := m.invoices["inv_1"]
			row.Status, row.PaymentStatus = domain.InvoicePaid, domain.PaymentSucceeded
			row.StripePaymentIntentID, row.PaidAt = "pi_older_attempt", &paidAt
			row.ChargeAttemptSeq++
			m.invoices["inv_1"] = row
		}
	}

	for _, tc := range []struct {
		name         string
		client       *mockStripeClient
		breakerOpen  bool
		setup        func(m *mockInvoiceUpdater)
		wantSkipped  bool // ErrPaymentTransient
		wantErr      bool
		wantKept     bool
		wantReleased bool
		wantStatus   domain.InvoicePaymentStatus
		wantPI       string
		wantDunning  int
	}{
		{name: "circuit breaker open: nothing sent",
			client: &mockStripeClient{piID: "pi_x", chargeStatus: "succeeded"}, breakerOpen: true,
			wantSkipped: true, wantReleased: true, wantStatus: domain.PaymentPending},
		{name: "credentials unreadable: nothing sent",
			client:      &mockStripeClient{shouldFail: true, failErr: fmt.Errorf("%w: %w: stripe credential lookup: pool exhausted", ErrPaymentTransient, ErrNotSent)},
			wantSkipped: true, wantReleased: true, wantStatus: domain.PaymentPending},
		{name: "429 to a request stripe-go resent: kept",
			client:      &mockStripeClient{shouldFail: true, failErr: fmt.Errorf("%w: stripe rate limit", ErrPaymentTransient)},
			wantSkipped: true, wantKept: true, wantStatus: domain.PaymentPending},
		{name: "the caller's deadline ends during the call: kept, nothing written",
			client:  &mockStripeClient{shouldFail: true, failErr: fmt.Errorf("Post \"https://api.stripe.com/v1/payment_intents\": %w", context.DeadlineExceeded)},
			wantErr: true, wantKept: true, wantStatus: domain.PaymentPending},
		{name: "network error or 5xx with no PaymentIntent: kept",
			client:  &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "An unknown error occurred", Unknown: true}},
			wantErr: true, wantKept: true, wantStatus: domain.PaymentPending},
		{name: "409 key in use: kept",
			client:  &mockStripeClient{shouldFail: true, failErr: keyInUse},
			wantErr: true, wantKept: true, wantStatus: domain.PaymentPending},
		{name: "409 same key, different parameters: parked, cleared",
			client:  &mockStripeClient{shouldFail: true, failErr: paramsDiffer},
			wantErr: true, wantStatus: domain.PaymentUnknown},
		{name: "decline naming its PaymentIntent: failed, cleared",
			client:  &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "Your card was declined.", DeclineCode: "generic_decline", PaymentIntentID: "pi_d"}},
			wantErr: true, wantReleased: true, wantStatus: domain.PaymentFailed, wantPI: "pi_d", wantDunning: 1},
		{name: "decline whose save fails: kept",
			client:  &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "Your card was declined.", DeclineCode: "generic_decline", PaymentIntentID: "pi_d"}},
			setup:   func(m *mockInvoiceUpdater) { m.markFailedErr = errors.New("connection reset") },
			wantErr: true, wantKept: true, wantReleased: true, wantStatus: domain.PaymentPending},
		{name: "definite rejection with no PaymentIntent: failed, cleared",
			client:  &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "No such customer: 'cus_stripe'"}},
			wantErr: true, wantReleased: true, wantStatus: domain.PaymentFailed, wantDunning: 1},
		{name: "Stripe not connected for this mode: failed, cleared",
			client:  &mockStripeClient{shouldFail: true, failErr: ErrStripeNotConfigured},
			wantErr: true, wantReleased: true, wantStatus: domain.PaymentFailed, wantDunning: 1},
		{name: "definite rejection whose stamp is refused (paid during the call): kept",
			client:  &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "No such customer: 'cus_stripe'"}},
			setup:   paidDuringCall,
			wantErr: true, wantKept: true, wantReleased: true, wantStatus: domain.PaymentSucceeded, wantPI: "pi_older_attempt"},
		{name: "unclear answer naming a PaymentIntent: unknown with it, cleared",
			client:  &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "An unknown error occurred", Unknown: true, PaymentIntentID: "pi_u"}},
			wantErr: true, wantStatus: domain.PaymentUnknown, wantPI: "pi_u"},
		{name: "succeeded: paid, cleared",
			client:       &mockStripeClient{piID: "pi_ok", chargeStatus: "succeeded"},
			wantReleased: true, wantStatus: domain.PaymentSucceeded, wantPI: "pi_ok"},
		{name: "processing: stamped, cleared",
			client:     &mockStripeClient{piID: "pi_p", chargeStatus: "processing"},
			wantStatus: domain.PaymentProcessing, wantPI: "pi_p"},
		{name: "processing, another outcome saved during the call: kept",
			client:   &mockStripeClient{piID: "pi_p", chargeStatus: "processing"},
			setup:    paidDuringCall,
			wantKept: true, wantStatus: domain.PaymentSucceeded, wantPI: "pi_older_attempt"},
		{name: "processing, its own webhook saved it first: cleared",
			client: &mockStripeClient{piID: "pi_p", chargeStatus: "processing"},
			setup: func(m *mockInvoiceUpdater) {
				m.duringCall = func(m *mockInvoiceUpdater) {
					row := m.invoices["inv_1"]
					row.PaymentStatus, row.StripePaymentIntentID = domain.PaymentProcessing, "pi_p"
					row.ChargeAttemptSeq++
					m.invoices["inv_1"] = row
				}
			},
			wantStatus: domain.PaymentProcessing, wantPI: "pi_p"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			invoices, snapshot := recordFixture()
			if tc.setup != nil {
				tc.setup(invoices)
			}
			dunning := &recordingDunningStarter{}
			s := NewStripe(tc.client, invoices, newMockWebhookStore(), nil, dunning)
			if tc.breakerOpen {
				b := breaker.New(breaker.Config{FailureThreshold: 1, Cooldown: time.Hour, Interval: time.Hour, Countable: func(error) bool { return true }})
				_, _ = b.Execute(context.Background(), func(context.Context) (any, error) { return nil, errors.New("trip") })
				s.SetBreaker(b)
			}

			_, err := s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test")
			switch {
			case tc.wantSkipped:
				if !errors.Is(err, ErrPaymentTransient) {
					t.Errorf("err = %v, want ErrPaymentTransient", err)
				}
			case tc.wantErr:
				if err == nil || errors.Is(err, ErrPaymentTransient) {
					t.Errorf("err = %v, want the charge's error", err)
				}
			default:
				if err != nil {
					t.Errorf("err = %v, want nil", err)
				}
			}
			if tc.breakerOpen && tc.client.createCalls != 0 {
				t.Errorf("Stripe called %d times through an open breaker", tc.client.createCalls)
			}
			if invoices.recordBegins != 1 {
				t.Fatalf("records written = %d, want 1", invoices.recordBegins)
			}
			if got := invoices.recordHeld("inv_1"); got != tc.wantKept {
				if tc.wantKept {
					t.Errorf("record cleared: this result is not saved on the invoice, so the record must stay until Stripe is asked")
				} else {
					t.Errorf("record kept: the result is saved (or nothing was sent), so it must be cleared")
				}
			}
			if released := len(invoices.leaseReleases) > 0; released != tc.wantReleased {
				t.Errorf("charge hold released = %v, want %v", released, tc.wantReleased)
			}
			got := invoices.invoices["inv_1"]
			if got.PaymentStatus != tc.wantStatus || got.StripePaymentIntentID != tc.wantPI {
				t.Errorf("invoice %s / %q, want %s / %q", got.PaymentStatus, got.StripePaymentIntentID, tc.wantStatus, tc.wantPI)
			}
			if len(dunning.calls) != tc.wantDunning {
				t.Errorf("StartDunning calls = %d, want %d", len(dunning.calls), tc.wantDunning)
			}
			if tc.wantKept {
				// The record's owner is the interrupted-charge check now: no
				// CRITICAL that tells an operator to go and find the charge.
				for _, banned := range []string{"CRITICAL: could not persist", "look the charge up in the Stripe dashboard"} {
					if strings.Contains(logs.String(), banned) {
						t.Errorf("log says %q while the record stays and the check resolves it:\n%s", banned, logs)
					}
				}
			}
		})
	}
}

// A stamp write that fails after Stripe named the PaymentIntent leaves the
// record, and says so at WARN: the check resolves it.
func TestChargeInvoice_StampWriteFails_RecordStaysAndLogsWarn(t *testing.T) {
	logs := captureLogs(t)
	base, snapshot := recordFixture()
	f := &flakyUpdater{mockInvoiceUpdater: base, failFirst: 99}
	client := &mockStripeClient{shouldFail: true, failErr: &PaymentError{Message: "An unknown error occurred", Unknown: true, PaymentIntentID: "pi_u"}}
	s := NewStripe(client, f, newMockWebhookStore(), nil)

	if _, err := s.ChargeInvoice(context.Background(), "t1", snapshot, "cus_stripe", "pm_test"); err == nil {
		t.Fatal("want the unclear charge's error")
	}
	if !base.recordHeld("inv_1") {
		t.Fatal("record cleared although the result was never saved")
	}
	if strings.Contains(logs.String(), "CRITICAL: could not persist") {
		t.Errorf("CRITICAL logged for a case the interrupted-charge check owns:\n%s", logs)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "interrupted-charge check") {
		t.Errorf("no WARN naming the interrupted-charge check:\n%s", logs)
	}
}
