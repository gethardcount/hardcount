package invoice

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// helper: minimal draft invoice in healthy state
func draft() domain.Invoice {
	return domain.Invoice{
		ID:            "hc_inv_test",
		Status:        domain.InvoiceDraft,
		PaymentStatus: domain.PaymentPending,
		TaxFacts: domain.TaxFacts{
			TaxStatus: domain.InvoiceTaxOK,
		},
	}
}

func TestClassifyInvoiceAttention_HealthyReturnsNil(t *testing.T) {
	if got := classifyInvoiceAttention(draft(), attentionContext{}); got != nil {
		t.Fatalf("healthy invoice should return nil, got %+v", got)
	}
}

func TestClassifyInvoiceAttention_TerminalStatesReturnNil(t *testing.T) {
	for _, status := range []domain.InvoiceStatus{domain.InvoicePaid, domain.InvoiceVoided} {
		t.Run(string(status), func(t *testing.T) {
			inv := draft()
			inv.Status = status
			// Even with active failure modes, terminal status must suppress.
			inv.TaxStatus = domain.InvoiceTaxPending
			inv.TaxErrorCode = "customer_data_invalid"
			inv.PaymentStatus = domain.PaymentFailed
			if got := classifyInvoiceAttention(inv, attentionContext{}); got != nil {
				t.Fatalf("terminal status %s should suppress attention, got %+v", status, got)
			}
		})
	}
}

// Each code on a pending invoice whose automatic retries are used up (or
// never ran, for a code that is not retried) reads critical, with its own
// reason and action.
func TestClassifyInvoiceAttention_TaxFailedSubcodes(t *testing.T) {
	cases := []struct {
		errorCode   string
		wantReason  domain.AttentionReason
		wantParam   string
		wantPrimAct domain.AttentionAction
	}{
		{"customer_data_invalid", domain.AttentionReasonTaxLocationRequired, "customer.address.postal_code", domain.AttentionActionEditBillingProfile},
		{"jurisdiction_unsupported", domain.AttentionReasonTaxCalculationFailed, "", domain.AttentionActionReviewRegistration},
		{"provider_outage", domain.AttentionReasonTaxCalculationFailed, "", domain.AttentionActionWaitProvider},
		{"provider_auth", domain.AttentionReasonTaxCalculationFailed, "", domain.AttentionActionRotateAPIKey},
		{"unknown", domain.AttentionReasonTaxCalculationFailed, "", domain.AttentionActionRetryTax},
		{"", domain.AttentionReasonTaxCalculationFailed, "", domain.AttentionActionRetryTax}, // empty falls through to unknown branch
	}
	for _, tc := range cases {
		t.Run(tc.errorCode, func(t *testing.T) {
			inv := draft()
			inv.TaxStatus = domain.InvoiceTaxPending
			inv.TaxRetryCount = domain.MaxTaxRetryAttempts
			inv.TaxErrorCode = tc.errorCode
			inv.TaxPendingReason = "raw provider response"
			now := time.Now()
			inv.TaxDeferredAt = &now

			att := classifyInvoiceAttention(inv, attentionContext{})
			if att == nil {
				t.Fatalf("expected attention, got nil")
			}
			if att.Severity != domain.AttentionSeverityCritical {
				t.Errorf("severity = %s, want critical", att.Severity)
			}
			if att.Reason != tc.wantReason {
				t.Errorf("reason = %s, want %s", att.Reason, tc.wantReason)
			}
			if att.Param != tc.wantParam {
				t.Errorf("param = %q, want %q", att.Param, tc.wantParam)
			}
			if len(att.Actions) == 0 {
				t.Fatalf("expected at least one action")
			}
			if att.Actions[0].Code != tc.wantPrimAct {
				t.Errorf("primary action = %s, want %s", att.Actions[0].Code, tc.wantPrimAct)
			}
			// No doc pages are published yet; a set DocURL would be a dead link.
			if att.DocURL != "" {
				t.Errorf("DocURL = %q, want empty until the doc pages exist", att.DocURL)
			}
			// ADR-025: upstream provider responses go in ProviderResponse,
			// not Detail. Every code in this test fixture except
			// provider_not_configured (not exercised here) is post-flight
			// — the raw string came back from the provider.
			if att.ProviderResponse != "raw provider response" {
				t.Errorf("expected ProviderResponse to carry raw provider response, got %q", att.ProviderResponse)
			}
			if att.Detail != "" {
				t.Errorf("Detail should be empty for tax codes — Hardcount-internal slot is reserved for our own framing, got %q", att.Detail)
			}
			if att.Since == nil {
				t.Errorf("expected Since to be set from TaxDeferredAt")
			}
			if want := "tax." + tc.errorCode; tc.errorCode != "" && att.Code != want {
				t.Errorf("code = %q, want %q", att.Code, want)
			}
		})
	}
}

// TestClassifyInvoiceAttention_ProviderNotConfiguredEmptyResponse asserts
// the ADR-025 contract for the only pre-flight tax code: provider_not_
// configured fired before any HTTP call to Stripe, so neither slot
// should carry the Hardcount-internal string ("no client configured for
// livemode=…"). The headline + Connect Stripe action carry the whole
// UI; surfacing the internal string under "Provider response" would
// mislead operators into thinking a 4xx came back from Stripe.
func TestClassifyInvoiceAttention_ProviderNotConfiguredEmptyResponse(t *testing.T) {
	inv := draft()
	inv.TaxStatus = domain.InvoiceTaxPending
	inv.TaxErrorCode = "provider_not_configured"
	inv.TaxPendingReason = "no client configured for livemode=false" // Hardcount-internal, NOT a provider response
	now := time.Now()
	inv.TaxDeferredAt = &now

	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil {
		t.Fatalf("expected attention, got nil")
	}
	if att.ProviderResponse != "" {
		t.Errorf("ProviderResponse should be empty for pre-flight code (we never called the provider), got %q", att.ProviderResponse)
	}
	if att.Detail != "" {
		t.Errorf("Detail should be empty — headline + Connect Stripe action are the whole UI, got %q", att.Detail)
	}
}

// TestClassifyInvoiceAttention_ProviderNotConfigured_StripeConnectedSwapsCopy
// pins the gap-window UX fix: when an invoice has tax_error_code=
// provider_not_configured (stamped at calculation-fail time) AND the
// tenant has now connected Stripe, the banner must NOT keep telling
// the operator to "Connect Stripe in Settings → Payments" — Stripe
// is connected; the only thing the invoice is waiting for is the
// next scheduler tick. Surface the queued-and-retry-now path
// instead.
func TestClassifyInvoiceAttention_ProviderNotConfigured_StripeConnectedSwapsCopy(t *testing.T) {
	inv := draft()
	inv.TaxStatus = domain.InvoiceTaxPending
	inv.TaxErrorCode = "provider_not_configured"

	t.Run("not connected → connect-stripe action", func(t *testing.T) {
		att := classifyInvoiceAttention(inv, attentionContext{StripeConnected: false})
		if att == nil {
			t.Fatalf("expected attention, got nil")
		}
		if !strings.Contains(att.Message, "Stripe isn't connected") {
			t.Errorf("expected 'Stripe isn't connected' copy, got: %q", att.Message)
		}
		if len(att.Actions) == 0 || att.Actions[0].Code != domain.AttentionActionConnectTaxProvider {
			t.Errorf("expected primary action = connect_tax_provider, got: %+v", att.Actions)
		}
	})

	t.Run("connected → calculation-queued + retry-now", func(t *testing.T) {
		att := classifyInvoiceAttention(inv, attentionContext{StripeConnected: true})
		if att == nil {
			t.Fatalf("expected attention, got nil")
		}
		if strings.Contains(att.Message, "Stripe isn't connected") {
			t.Errorf("must NOT say 'Stripe isn't connected' when Stripe IS connected, got: %q", att.Message)
		}
		if !strings.Contains(att.Message, "queued") && !strings.Contains(att.Message, "retry") {
			t.Errorf("expected queued/retry-shortly copy, got: %q", att.Message)
		}
		if len(att.Actions) != 1 || att.Actions[0].Code != domain.AttentionActionRetryTax {
			t.Errorf("connected branch should expose only Retry now (no Connect Stripe), got: %+v", att.Actions)
		}
		if att.Severity != domain.AttentionSeverityInfo {
			t.Errorf("queued state should be info severity (transient, system will resolve), got: %v", att.Severity)
		}
	})
}

// Pending is warning ONLY while an automatic mover remains (a retryable code
// below the retry cap). This test originally pinned customer_data_invalid —
// a NON-retryable code the reconciler never scans — as warning, which showed
// the "self-healing in progress" tier over an invoice only an operator can
// move. The severity-tracks-mover rule (see
// TestClassifyInvoiceAttention_PendingSeverityTracksAutomaticMover) corrected
// it; this test keeps the half that was true: a retryable, non-exhausted
// pending IS a warning, not a critical.
func TestClassifyInvoiceAttention_TaxPendingIsWarning(t *testing.T) {
	inv := draft()
	inv.TaxStatus = domain.InvoiceTaxPending
	inv.TaxErrorCode = "provider_outage" // retryable, retry count 0: the reconciler is on it
	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil || att.Severity != domain.AttentionSeverityWarning {
		t.Fatalf("pending with a live automatic mover should be warning, got %+v", att)
	}
	if att.Reason != domain.AttentionReasonTaxCalculationFailed {
		t.Errorf("reason = %s, want %s", att.Reason, domain.AttentionReasonTaxCalculationFailed)
	}
}

func TestClassifyInvoiceAttention_PaymentFailed(t *testing.T) {
	inv := draft()
	inv.PaymentStatus = domain.PaymentFailed
	inv.LastPaymentError = "card declined: insufficient funds"
	inv.UpdatedAt = time.Now()

	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Severity != domain.AttentionSeverityCritical {
		t.Errorf("severity = %s, want critical", att.Severity)
	}
	if att.Reason != domain.AttentionReasonPaymentFailed {
		t.Errorf("reason = %s, want %s", att.Reason, domain.AttentionReasonPaymentFailed)
	}
	// Operator voice: the banner headline is Hardcount's sentence, never the
	// provider's cardholder-facing string ("Your card was declined" read
	// as Hardcount addressing the operator). The provider's exact wording is
	// preserved verbatim in ProviderResponse.
	if att.Message != "The customer's card was declined." {
		t.Errorf("message = %q, want the operator-voice headline", att.Message)
	}
	if att.ProviderResponse != "card declined: insufficient funds" {
		t.Errorf("provider response = %q, want the verbatim provider string", att.ProviderResponse)
	}
	// Primary action is now update_payment_method (the card on file
	// is broken — retrying the same card will decline again).
	// retry_payment remains as the secondary action for transient
	// declines where the operator wants to re-attempt without
	// changing the card.
	if len(att.Actions) < 2 ||
		att.Actions[0].Code != domain.AttentionActionUpdatePaymentMethod ||
		att.Actions[1].Code != domain.AttentionActionRetryPayment {
		t.Errorf("expected actions [update_payment_method, retry_payment], got %v", att.Actions)
	}
	// ADR-025: LastPaymentError is Stripe's last_payment_error body,
	// upstream payload — goes in ProviderResponse, not Detail.
	if att.ProviderResponse != "card declined: insufficient funds" {
		t.Errorf("expected ProviderResponse to carry LastPaymentError, got %q", att.ProviderResponse)
	}
	if att.Detail != "" {
		t.Errorf("Detail should be empty (no Hardcount-internal context for this code yet), got %q", att.Detail)
	}
}

func TestClassifyInvoiceAttention_PaymentUnknownIsInfo(t *testing.T) {
	inv := draft()
	inv.PaymentStatus = domain.PaymentUnknown
	// The reconciler reads the PaymentIntent by id, so the charge resolves
	// itself and Info is honest.
	inv.StripePaymentIntentID = "pi_resolving"
	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil || att.Severity != domain.AttentionSeverityInfo {
		t.Fatalf("payment_unknown should be info, got %+v", att)
	}
	if att.Reason != domain.AttentionReasonPaymentUnconfirmed {
		t.Errorf("reason = %s, want %s", att.Reason, domain.AttentionReasonPaymentUnconfirmed)
	}
}

func TestClassifyInvoiceAttention_PriorityOrder(t *testing.T) {
	// payment_failed beats tax pending, which beats payment_unknown.
	inv := draft()
	inv.TaxStatus = domain.InvoiceTaxPending
	inv.TaxErrorCode = "customer_data_invalid"
	inv.PaymentStatus = domain.PaymentFailed // also bad

	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil || att.Reason != domain.AttentionReasonPaymentFailed {
		t.Errorf("priority broken: payment_failed should win, got %+v", att)
	}

	// payment_unknown instead of failed — tax pending now wins.
	inv.PaymentStatus = domain.PaymentUnknown
	att = classifyInvoiceAttention(inv, attentionContext{})
	if att == nil || att.Reason != domain.AttentionReasonTaxLocationRequired {
		t.Errorf("priority broken: tax pending should beat payment_unknown, got %+v", att)
	}

	// Drop tax — payment_unknown remains.
	inv.TaxStatus = domain.InvoiceTaxOK
	inv.TaxErrorCode = ""
	att = classifyInvoiceAttention(inv, attentionContext{})
	if att == nil || att.Reason != domain.AttentionReasonPaymentUnconfirmed {
		t.Errorf("priority broken: payment_unknown should remain, got %+v", att)
	}
}

func TestClassifyInvoiceAttention_PaymentProcessing(t *testing.T) {
	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentProcessing
	inv.UpdatedAt = time.Now()

	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Severity != domain.AttentionSeverityInfo {
		t.Errorf("severity = %s, want info", att.Severity)
	}
	if att.Reason != domain.AttentionReasonPaymentProcessing {
		t.Errorf("reason = %s, want %s", att.Reason, domain.AttentionReasonPaymentProcessing)
	}
	if len(att.Actions) != 0 {
		t.Errorf("processing should expose no actions (waiting on provider), got %d", len(att.Actions))
	}
}

func TestClassifyInvoiceAttention_PaymentScheduled(t *testing.T) {
	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentPending
	inv.AutoChargePending = true
	inv.UpdatedAt = time.Now()

	// payment_scheduled requires HasPaymentMethod=true: when both
	// auto_charge_pending AND no PM, no_payment_method wins (the
	// scheduler retry would skip again until PM is attached, so
	// "engine will retry" would lie to the operator). See
	// TestClassifyInvoiceAttention_NoPaymentMethod_BeatsScheduled.
	att := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: true})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Severity != domain.AttentionSeverityInfo {
		t.Errorf("severity = %s, want info", att.Severity)
	}
	if att.Reason != domain.AttentionReasonPaymentScheduled {
		t.Errorf("reason = %s, want %s", att.Reason, domain.AttentionReasonPaymentScheduled)
	}
	if len(att.Actions) == 0 || att.Actions[0].Code != domain.AttentionActionChargeNow {
		t.Errorf("expected primary action charge_now, got %+v", att.Actions)
	}
	// The sweep runs on the billing tick, hourly by default, so the sentence
	// promises "within an hour".
	if !strings.Contains(att.Message, "usually within an hour") {
		t.Errorf("message = %q, want the hourly-schedule promise", att.Message)
	}
	if !strings.Contains(att.Detail, "hourly") {
		t.Errorf("detail = %q, want the schedule explained", att.Detail)
	}
}

// TestClassifyInvoiceAttention_NoPaymentMethod_BeatsScheduled pins the
// priority order: when an invoice has both auto_charge_pending=true
// AND no PM ready, the classifier surfaces no_payment_method (the
// actionable reason) — surfacing payment_scheduled would tell the
// operator "engine will retry on its next tick" when in fact the
// retry will skip again until a PM is attached.
func TestClassifyInvoiceAttention_NoPaymentMethod_BeatsScheduled(t *testing.T) {
	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentPending
	inv.AutoChargePending = true // engine queued for retry post-no-PM-finalize
	inv.UpdatedAt = time.Now()

	att := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: false})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Reason != domain.AttentionReasonNoPaymentMethod {
		t.Errorf("reason = %s, want %s (no_payment_method must beat payment_scheduled when PM is missing)", att.Reason, domain.AttentionReasonNoPaymentMethod)
	}
}

func TestClassifyInvoiceAttention_AwaitingPayment(t *testing.T) {
	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentPending
	// AutoChargePending = false (default) — no scheduler queue, no charge yet.
	inv.UpdatedAt = time.Now()

	// HasPaymentMethod=true: PM is on file but engine hasn't run yet.
	// This is the race-window case; classifier should pick awaiting_
	// payment (generic info), not no_payment_method.
	att := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: true})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Severity != domain.AttentionSeverityInfo {
		t.Errorf("severity = %s, want info", att.Severity)
	}
	if att.Reason != domain.AttentionReasonAwaitingPayment {
		t.Errorf("reason = %s, want %s", att.Reason, domain.AttentionReasonAwaitingPayment)
	}
	codes := make(map[domain.AttentionAction]bool)
	for _, a := range att.Actions {
		codes[a.Code] = true
	}
	if !codes[domain.AttentionActionChargeNow] || !codes[domain.AttentionActionSendReminder] {
		t.Errorf("awaiting_payment should offer charge_now + send_reminder, got %+v", att.Actions)
	}
	if att.NextAttemptAt != nil {
		t.Errorf("awaiting_payment must not set NextAttemptAt — engine has nothing scheduled, got %v", att.NextAttemptAt)
	}
}

// TestClassifyInvoiceAttention_NoPaymentMethod pins the operator-
// actionable distinction: a finalized invoice with no PaymentSetup
// surfaces no_payment_method (warning, action: add_payment_method),
// not the generic awaiting_payment. Without this branch, operators
// see "Invoice is finalized and awaiting payment" and have no
// indication that the engine will never auto-charge.
func TestClassifyInvoiceAttention_NoPaymentMethod(t *testing.T) {
	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentPending
	inv.UpdatedAt = time.Now()

	// Customer HAS an email → the engine emailed a setup link, so the banner
	// claims it and offers both a resend and the customer-page path.
	att := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: false, CustomerHasEmail: true, StripeConnected: true})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Reason != domain.AttentionReasonNoPaymentMethod {
		t.Errorf("reason = %s, want %s", att.Reason, domain.AttentionReasonNoPaymentMethod)
	}
	if att.Severity != domain.AttentionSeverityWarning {
		t.Errorf("severity = %s, want warning", att.Severity)
	}
	codes := make(map[domain.AttentionAction]bool)
	for _, a := range att.Actions {
		codes[a.Code] = true
	}
	if !codes[domain.AttentionActionAddPaymentMethod] {
		t.Errorf("no_payment_method must offer add_payment_method, got %+v", att.Actions)
	}
	if !codes[domain.AttentionActionSendReminder] {
		t.Errorf("has-email no_payment_method must offer a resend, got %+v", att.Actions)
	}
	// Disposition form (2026-07-10): the banner states what the ENGINE DOES
	// and where delivery is verifiable — never "has been emailed", which is
	// unverifiable from the classifier's inputs (false under suppression/DLQ).
	if strings.Contains(att.Message, "has been emailed") {
		t.Errorf("banner must not assert a completed send it cannot observe, got %q", att.Message)
	}
	// Line 1: what adding a card does. Line 2: the setup-link email.
	want := "This invoice is charged automatically once the customer adds a card.\n" +
		"A setup link is emailed to the customer once; use Resend setup link to send it again."
	if att.Message != want {
		t.Errorf("has-email message =\n%q\nwant\n%q", att.Message, want)
	}
	if !strings.Contains(att.Detail, "activity shows whether the email was sent") {
		t.Errorf("has-email detail must say where delivery shows, got %q", att.Detail)
	}
	if att.NextAttemptAt != nil {
		t.Errorf("no_payment_method must not set NextAttemptAt — engine won't auto-charge without PM, got %v", att.NextAttemptAt)
	}
}

// TestClassifyInvoiceAttention_NoPaymentMethod_NoEmail pins the honest variant
// for a customer with no email on file: the engine's setup-link email skips
// silently (no address), so the banner must NOT claim it was emailed, must NOT
// offer a resend that can't send, and must point the operator at the copy-a-
// link / add-an-email path (add_payment_method). Zero-value CustomerHasEmail
// (the conservative default) selects this variant.
func TestClassifyInvoiceAttention_NoPaymentMethod_NoEmail(t *testing.T) {
	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentPending
	inv.UpdatedAt = time.Now()

	att := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: false, CustomerHasEmail: false, StripeConnected: true})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Reason != domain.AttentionReasonNoPaymentMethod {
		t.Errorf("reason = %s, want %s", att.Reason, domain.AttentionReasonNoPaymentMethod)
	}
	if strings.Contains(att.Message, "has been emailed") {
		t.Errorf("no-email variant must NOT claim a setup link was emailed, got %q", att.Message)
	}
	// Honest under uncertainty: states engine behavior ("only to customers
	// with an email address"), never asserts this customer's email state — so
	// it's correct whether the address is confirmably absent or merely
	// undetermined.
	if !strings.Contains(att.Message, "\nSetup links are emailed only to customers with an email address, so copy one from the customer page") {
		t.Errorf("no-email variant must state the conditional engine behavior on line 2, got %q", att.Message)
	}
	codes := make(map[domain.AttentionAction]bool)
	for _, a := range att.Actions {
		codes[a.Code] = true
	}
	if !codes[domain.AttentionActionAddPaymentMethod] {
		t.Errorf("no-email variant must offer add_payment_method (copy link / add email), got %+v", att.Actions)
	}
	if codes[domain.AttentionActionSendReminder] {
		t.Errorf("no-email variant must NOT offer a resend that can't send, got %+v", att.Actions)
	}
}

func TestClassifyInvoiceAttention_DraftSuppressesAttention(t *testing.T) {
	inv := draft()
	// Status=draft, payment_status=pending — should NOT raise attention
	// (the page itself communicates draft state).
	att := classifyInvoiceAttention(inv, attentionContext{})
	if att != nil {
		t.Errorf("draft should suppress info attention, got %+v", att)
	}
}

func TestClassifyInvoiceAttention_EmptyPaymentErrorFallsBackToGeneric(t *testing.T) {
	inv := draft()
	inv.PaymentStatus = domain.PaymentFailed
	inv.LastPaymentError = ""
	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil {
		t.Fatalf("expected attention")
	}
	if att.Message == "" {
		t.Errorf("expected non-empty fallback message")
	}
}

// TestClassify_PaymentProcessing_AgeAware locks ADR-049 Phase 4: a fresh
// in-flight payment is Info ("resolves automatically"), but a REAL invoice
// stuck processing past the expected-settle window escalates to Warning and
// points the operator at Stripe, and a zero Now stays Info.
func TestClassify_PaymentProcessing_AgeAware(t *testing.T) {
	now := time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
	base := func(updatedAgo time.Duration) domain.Invoice {
		return domain.Invoice{
			ID: "hc_inv_test", Status: domain.InvoiceFinalized,
			PaymentStatus: domain.PaymentProcessing, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
			UpdatedAt: now.Add(-updatedAgo),
		}
	}

	t.Run("fresh real → Info, auto-resolve copy", func(t *testing.T) {
		att := classifyInvoiceAttention(base(1*time.Hour), attentionContext{Now: now})
		if att == nil || att.Severity != domain.AttentionSeverityInfo {
			t.Fatalf("att = %+v, want Info", att)
		}
		if !strings.Contains(att.Message, "automatically") {
			t.Errorf("fresh copy = %q, want it to mention automatic confirmation", att.Message)
		}
	})

	t.Run("stale real → Warning, points to Stripe", func(t *testing.T) {
		att := classifyInvoiceAttention(base(7*time.Hour), attentionContext{Now: now})
		if att == nil || att.Severity != domain.AttentionSeverityWarning {
			t.Fatalf("att = %+v, want Warning past the stale window", att)
		}
		if !strings.Contains(att.Message, "Stripe") {
			t.Errorf("stale copy = %q, want it to point at Stripe (no false auto-resolve promise)", att.Message)
		}
	})

	t.Run("zero Now → stays Info", func(t *testing.T) {
		att := classifyInvoiceAttention(base(7*time.Hour), attentionContext{})
		if att == nil || att.Severity != domain.AttentionSeverityInfo {
			t.Errorf("zero Now must not escalate: %+v", att)
		}
	})
}

// TestClassify_PaymentUnconfirmed_NoDeadAction: the unconfirmed banner no longer
// ships a non-functional "Check provider" button — the reconciler resolves it
// automatically (ADR-049 Phase 2); an on-demand action is deferred.
func TestClassify_PaymentUnconfirmed_NoDeadAction(t *testing.T) {
	inv := domain.Invoice{
		ID: "hc_inv_test", Status: domain.InvoiceFinalized,
		PaymentStatus: domain.PaymentUnknown, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		// The resolving case — see the note in
		// TestClassifyInvoiceAttention_PaymentUnknownIsInfo.
		StripePaymentIntentID: "pi_resolving",
	}
	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil || att.Code != "payment.unconfirmed" {
		t.Fatalf("att = %+v, want payment.unconfirmed", att)
	}
	if len(att.Actions) != 0 {
		t.Errorf("unconfirmed banner carries %d actions, want 0 (dead disabled button removed)", len(att.Actions))
	}
}

// TestClassifyTaxAttention_RetryPolicyTruth locks the 2026-07-19 truth-audit
// fixes: the banner's retry claims mirror the reconciler's actual policy.
// Pre-fix: "will retry automatically" rendered forever after the cap
// excluded the invoice; provider_not_configured promised scheduler retries
// the predicate never made; NextAttemptAt was populated by no code path.
func TestClassifyTaxAttention_RetryPolicyTruth(t *testing.T) {
	next := time.Date(2027, 6, 2, 9, 0, 0, 0, time.UTC)
	base := domain.Invoice{Status: domain.InvoiceFinalized, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending}}

	t.Run("retries remaining: Warning, attempts-used copy, real NextAttemptAt", func(t *testing.T) {
		inv := base
		inv.TaxErrorCode = "provider_outage"
		inv.TaxRetryCount = 3
		inv.TaxNextRetryAt = &next
		att := classifyInvoiceAttention(inv, attentionContext{})
		if att == nil || att.Severity != domain.AttentionSeverityWarning {
			t.Fatalf("want Warning while retries remain, got %+v", att)
		}
		if !strings.Contains(att.Message, "3 of 8 attempts used") {
			t.Errorf("message must state real attempt usage, got %q", att.Message)
		}
		if att.NextAttemptAt == nil || !att.NextAttemptAt.Equal(next) {
			t.Errorf("NextAttemptAt must surface the reconciler's tax_next_retry_at, got %v", att.NextAttemptAt)
		}
	})

	t.Run("exhausted: escalates to Critical, no NextAttemptAt, no retry promise", func(t *testing.T) {
		inv := base
		inv.TaxErrorCode = "provider_outage"
		inv.TaxRetryCount = domain.MaxTaxRetryAttempts
		inv.TaxNextRetryAt = &next // stale row value must NOT surface
		att := classifyInvoiceAttention(inv, attentionContext{})
		if att == nil || att.Severity != domain.AttentionSeverityCritical {
			t.Fatalf("exhausted retries must escalate to Critical, got %+v", att)
		}
		if att.NextAttemptAt != nil {
			t.Error("no next attempt exists after exhaustion — surfacing one is a lie")
		}
		if strings.Contains(att.Message, "retries automatically") {
			t.Errorf("exhausted banner must not promise automatic retries: %q", att.Message)
		}
	})

	t.Run("not_configured + connected: Info with truthful queue copy", func(t *testing.T) {
		inv := base
		inv.TaxErrorCode = "provider_not_configured"
		inv.TaxRetryCount = 1
		att := classifyInvoiceAttention(inv, attentionContext{StripeConnected: true})
		if att == nil || att.Severity != domain.AttentionSeverityInfo {
			t.Fatalf("want Info for queued post-connect recompute, got %+v", att)
		}
		if strings.Contains(att.Message, "scheduler tick") {
			t.Errorf("copy must not reference internals; got %q", att.Message)
		}
	})

	t.Run("not_configured is genuinely retryable by the policy the banner asserts", func(t *testing.T) {
		if !domain.TaxErrorCodeRetryable("provider_not_configured") {
			t.Error("banner promises automatic recompute; the policy must actually include provider_not_configured")
		}
	})
}

// TestClassifyInvoiceAttention_DunningExhausted pins the escalated-run state
// (2026-07-22 audit, P1-4): once the invoice's dunning run has escalated,
// the banner must stop implying recovery is still running — it names the
// exhaustion, keeps charge-on-attach as the fix, and keeps the resend
// action only when an email exists. Mutation seam: drop the
// atc.DunningEscalated branch and every assertion here fails.
func TestClassifyInvoiceAttention_DunningExhausted(t *testing.T) {
	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentPending
	inv.UpdatedAt = time.Now()

	att := classifyInvoiceAttention(inv, attentionContext{
		HasPaymentMethod: false, CustomerHasEmail: true, DunningEscalated: true, StripeConnected: true,
	})
	if att == nil {
		t.Fatal("expected attention")
	}
	if att.Reason != domain.AttentionReasonDunningExhausted {
		t.Fatalf("reason = %s, want %s", att.Reason, domain.AttentionReasonDunningExhausted)
	}
	if att.Code != "payment.dunning_exhausted" {
		t.Errorf("code = %s, want payment.dunning_exhausted", att.Code)
	}
	if att.Message != "Retries stopped; this invoice is charged automatically once the customer adds a card." {
		t.Errorf("escalated (not paused) banner must state retries stopped and keep charge-on-attach as the fix, got %q", att.Message)
	}
	if !strings.Contains(att.Detail, "ended without collecting") {
		t.Errorf("escalated detail must state recovery ended, got %q", att.Detail)
	}
	codes := make(map[domain.AttentionAction]bool)
	for _, a := range att.Actions {
		codes[a.Code] = true
	}
	if !codes[domain.AttentionActionSendReminder] || !codes[domain.AttentionActionAddPaymentMethod] {
		t.Errorf("escalated (has-email) must offer resend + customer page, got %+v", att.Actions)
	}

	// No email: the resend action must drop, same rule as the base branch.
	att = classifyInvoiceAttention(inv, attentionContext{
		HasPaymentMethod: false, CustomerHasEmail: false, DunningEscalated: true,
	})
	if att.Reason != domain.AttentionReasonDunningExhausted {
		t.Fatalf("no-email escalated reason = %s, want dunning_exhausted", att.Reason)
	}
	for _, a := range att.Actions {
		if a.Code == domain.AttentionActionSendReminder {
			t.Errorf("no-email escalated must not offer a resend that cannot send, got %+v", att.Actions)
		}
	}
}

// TestClassifyInvoiceAttention_SinceIsUpdatedAt: a banner's `since` is the
// invoice's last write, not its issue instant.
func TestClassifyInvoiceAttention_SinceIsUpdatedAt(t *testing.T) {
	issued := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 7, 26, 17, 53, 0, 0, time.UTC)

	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentPending
	inv.AmountDueCents = 4300
	inv.IssuedAt = &issued
	inv.UpdatedAt = updated

	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil || att.Since == nil {
		t.Fatalf("expected an attention with Since, got %+v", att)
	}
	if !att.Since.Equal(updated) {
		t.Errorf("Since = %v, want UpdatedAt %v", att.Since, updated)
	}
}

// TestClassify_CollectionPaused_BeatsPaymentScheduled pins that a queued
// invoice on a paused subscription does not claim a charge is coming.
//
// Both auto-charge sweeps skip paused subscriptions, so the queue flag alone
// no longer means "the engine will charge this". The state is reachable
// whenever a subscription is paused AFTER its invoice was queued — which is
// exactly what dunning's `pause` final action does to every in-flight invoice.
func TestClassify_CollectionPaused_BeatsPaymentScheduled(t *testing.T) {
	inv := domain.Invoice{
		Status:            domain.InvoiceFinalized,
		PaymentStatus:     domain.PaymentPending,
		AutoChargePending: true,
		TaxFacts:          domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
	}

	scheduled := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: true})
	if scheduled == nil || scheduled.Reason != domain.AttentionReasonPaymentScheduled {
		t.Fatalf("unpaused: reason = %v, want payment_scheduled", scheduled)
	}

	paused := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: true, CollectionPaused: true})
	if paused == nil {
		t.Fatal("paused: got no attention, want collection_paused")
	}
	if paused.Reason != domain.AttentionReasonCollectionPaused {
		t.Errorf("paused: reason = %s, want collection_paused", paused.Reason)
	}
	if strings.Contains(paused.Message, "within an hour") {
		t.Errorf("paused copy must not promise an automatic charge, got %q", paused.Message)
	}
	// Charge now survives: a manual collect is an explicit operator override
	// and has no pause filter.
	var hasChargeNow bool
	for _, a := range paused.Actions {
		if a.Code == domain.AttentionActionChargeNow {
			hasChargeNow = true
		}
	}
	if !hasChargeNow {
		t.Error("paused banner must still offer Charge now — the pause governs automation, not the operator")
	}
}

// TestRecoveryInFlight_NarrowByDesign pins the bad-debt-recovery banner and,
// more importantly, its SILENCE.
//
// Post-ADR-113 no charge path admits uncollectible, so the state is reachable
// only for rows whose charge was in flight when the removal deployed. The
// banner stays for those, and
// it must fire ONLY while a payment is genuinely in flight — a
// written-off invoice sitting at `failed` is every dunning-exhausted write-off
// in the system, and lighting those up would put a permanent banner on the
// most common terminal state there is.
func TestRecoveryInFlight_NarrowByDesign(t *testing.T) {
	cases := []struct {
		name    string
		status  domain.InvoiceStatus
		payment domain.InvoicePaymentStatus
		want    bool
	}{
		{"written off + recovery in flight", domain.InvoiceUncollectible, domain.PaymentProcessing, true},
		{"written off + failed (every dunning exhaustion)", domain.InvoiceUncollectible, domain.PaymentFailed, false},
		{"written off + pending", domain.InvoiceUncollectible, domain.PaymentPending, false},
		{"written off + unknown", domain.InvoiceUncollectible, domain.PaymentUnknown, false},
		{"voided + processing", domain.InvoiceVoided, domain.PaymentProcessing, false},
		{"paid + processing", domain.InvoicePaid, domain.PaymentProcessing, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := draft()
			inv.Status = tc.status
			inv.PaymentStatus = tc.payment
			got := classifyInvoiceAttention(inv, attentionContext{})
			fired := got != nil && got.Code == "payment.recovery_processing"
			if fired != tc.want {
				t.Fatalf("recovery banner fired=%v, want %v (attention=%+v)", fired, tc.want, got)
			}
		})
	}
}

// TestClassifyInvoiceAttention_PendingSeverityTracksAutomaticMover pins the
// severity rule as a rule, not per-code taste: warning is the tier meaning
// "self-healing in progress", so it is legal ONLY while an automatic mover
// remains. A pending invoice with a retryable code below the cap keeps
// warning; a non-retryable code (born with no mover — the reconciler never
// scans it) and an exhausted retryable code (mover permanently gone) must
// both read critical. Found by walking the provider_auth banner: the box
// promised red, the classifier rendered amber.
func TestClassifyInvoiceAttention_PendingSeverityTracksAutomaticMover(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		errorCode  string
		retryCount int
		want       domain.AttentionSeverity
	}{
		{"retryable below cap self-heals: warning", "provider_outage", 2, domain.AttentionSeverityWarning},
		{"non-retryable has no mover from birth: critical", "provider_auth", 0, domain.AttentionSeverityCritical},
		{"non-retryable customer data: critical", "customer_data_invalid", 0, domain.AttentionSeverityCritical},
		{"retryable but exhausted: critical", "provider_outage", domain.MaxTaxRetryAttempts, domain.AttentionSeverityCritical},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := draft()
			inv.TaxStatus = domain.InvoiceTaxPending
			inv.TaxErrorCode = tc.errorCode
			inv.TaxRetryCount = tc.retryCount
			inv.TaxDeferredAt = &now
			att := classifyInvoiceAttention(inv, attentionContext{})
			if att == nil {
				t.Fatal("expected attention, got nil")
			}
			if att.Severity != tc.want {
				t.Fatalf("severity = %s, want %s", att.Severity, tc.want)
			}
		})
	}
}

// While the customer's payment page is open no Hardcount charge can start
// (ADR-068), so the banner must not offer Retry / Charge now — the buttons
// would be refused — and it says why the retry date is not acted on.
func TestClassifyInvoiceAttention_PaymentPageOpen(t *testing.T) {
	for _, ps := range []domain.InvoicePaymentStatus{domain.PaymentFailed, domain.PaymentPending} {
		inv := draft()
		inv.Status = domain.InvoiceFinalized
		inv.PaymentStatus = ps
		inv.AutoChargePending = true
		att := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: true, PaymentPageOpen: true})
		if att == nil || att.Code != "payment.page_open" || att.Reason != domain.AttentionReasonPaymentProcessing || att.Severity != domain.AttentionSeverityInfo {
			t.Fatalf("payment_status=%s: got %+v, want info payment.page_open", ps, att)
		}
		if len(att.Actions) != 0 {
			t.Errorf("payment_status=%s: page-open banner offers %v; every charge is refused while the page is open", ps, att.Actions)
		}
	}
	// Negative control: the same failed invoice without a live page keeps the
	// decline banner and its Retry.
	inv := draft()
	inv.Status = domain.InvoiceFinalized
	inv.PaymentStatus = domain.PaymentFailed
	if att := classifyInvoiceAttention(inv, attentionContext{HasPaymentMethod: true}); att == nil || att.Code != "payment.declined" {
		t.Fatalf("no page: got %+v, want payment.declined", att)
	}
	// A paid invoice stays silent even if a stale row says a page is open.
	inv.Status = domain.InvoicePaid
	if att := classifyInvoiceAttention(inv, attentionContext{PaymentPageOpen: true}); att != nil {
		t.Errorf("paid: got %+v, want nil", att)
	}
}

// The attention notice's shape (operator-messages work, 2026-10-15): Message
// is one plain sentence of 25 words or fewer; a notice that must show a
// second fact up front adds it as a second line after "\n"; the mechanism and
// longer instructions go in Detail. These tests pin every branch's wording to
// the real signal it depends on (collection paused, customer email, email
// sending), so a branch that stops branching fails here.

var sentenceBreak = regexp.MustCompile(`[.!?]\s+[A-Z]`)

// engineWords are terms an operator should never have to decode.
var engineWords = []string{"engine", "tick", "reconciler", "sweep", "pi_", "minor units", "ADR-"}

// checkShape asserts the notice shape on every line of a message.
func checkShape(t *testing.T, msg string) {
	t.Helper()
	if msg == "" {
		t.Fatal("empty message")
	}
	for _, line := range strings.Split(msg, "\n") {
		if n := len(strings.Fields(line)); n > 25 {
			t.Errorf("line has %d words, want 25 or fewer: %q", n, line)
		}
		if sentenceBreak.MatchString(line) {
			t.Errorf("line is more than one sentence: %q", line)
		}
		if !strings.HasSuffix(line, ".") {
			t.Errorf("line does not end as a sentence: %q", line)
		}
		for _, w := range engineWords {
			if strings.Contains(line, w) {
				t.Errorf("line uses %q: %q", w, line)
			}
		}
	}
}

func actionCodes(att *domain.Attention) []domain.AttentionAction {
	out := make([]domain.AttentionAction, 0, len(att.Actions))
	for _, a := range att.Actions {
		out = append(out, a.Code)
	}
	return out
}

func sameActions(got []domain.AttentionAction, want ...domain.AttentionAction) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func noPMInvoice() domain.Invoice {
	return domain.Invoice{
		ID: "hc_inv_copy", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		AutoChargePending: true, UpdatedAt: time.Now(),
		TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
	}
}

// TestNoPaymentMethodCopy_EveryBranch: line 1 branches on paused; line 2
// branches on customer email, then on email sending; the
// Resend action exists only when an email can actually be sent.
func TestNoPaymentMethodCopy_EveryBranch(t *testing.T) {
	const (
		wall   = "This invoice is charged automatically once the customer adds a card."
		paused = "Collection is paused on the subscription, so adding a card won't charge this invoice until you resume it."

		sent    = "A setup link is emailed to the customer once; use Resend setup link to send it again."
		noEmail = "Setup links are emailed only to customers with an email address, so copy one from the customer page and send it."
		off     = "This server can't email setup links, so none was sent; copy one from the customer page instead."
	)
	cases := []struct {
		name               string
		pause              bool
		hasEmail, emailOff bool
		line1, line2       string
		resend             bool
	}{
		{"email", false, true, false, wall, sent, true},
		{"no email", false, false, false, wall, noEmail, false},
		{"email off", false, true, true, wall, off, false},
		{"paused, email", true, true, false, paused, sent, true},
		{"paused, no email", true, false, false, paused, noEmail, false},
		{"paused, email off", true, true, true, paused, off, false},
		// No email AND email off: the missing address is the first thing to fix.
		{"no email and email off", false, false, true, wall, noEmail, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := classifyInvoiceAttention(noPMInvoice(), attentionContext{
				StripeConnected:  true,
				CustomerHasEmail: tc.hasEmail, SetupLinkEmailOff: tc.emailOff, CollectionPaused: tc.pause,
			})
			if att == nil || att.Reason != domain.AttentionReasonNoPaymentMethod {
				t.Fatalf("got %+v, want no_payment_method", att)
			}
			if want := tc.line1 + "\n" + tc.line2; att.Message != want {
				t.Errorf("message =\n%q\nwant\n%q", att.Message, want)
			}
			checkShape(t, att.Message)
			if att.Detail == "" {
				t.Error("detail is empty; the email explanation belongs there")
			}
			if att.Severity != domain.AttentionSeverityWarning {
				t.Errorf("severity = %s, want warning", att.Severity)
			}
			want := []domain.AttentionAction{domain.AttentionActionAddPaymentMethod}
			if tc.resend {
				want = []domain.AttentionAction{domain.AttentionActionSendReminder, domain.AttentionActionAddPaymentMethod}
			}
			if got := actionCodes(att); !sameActions(got, want...) {
				t.Errorf("actions = %v, want %v", got, want)
			}
		})
	}
}

// TestRecoveryEndedCopy_EveryBranch: once dunning has escalated, the fix
// depends on whether the final step paused collection. When no setup link can
// be emailed, the reason Resend is missing shows as a second line, not only
// in Details.
func TestRecoveryEndedCopy_EveryBranch(t *testing.T) {
	const (
		noEmailLine  = "Setup links are emailed only to customers with an email address, so copy one from the customer page and send it."
		emailOffLine = "This server can't email setup links, so none was sent; copy one from the customer page instead."
	)
	cases := []struct {
		name               string
		pause              bool
		hasEmail, emailOff bool
		msg                string
		resend             bool
	}{
		{"paused", true, true, false,
			"Retries stopped and collection is paused; ask the customer to add a card, then resume collection.", true},
		{"paused, no email", true, false, false,
			"Retries stopped and collection is paused; ask the customer to add a card, then resume collection.\n" + noEmailLine, false},
		{"not paused", false, true, false,
			"Retries stopped; this invoice is charged automatically once the customer adds a card.", true},
		{"not paused, email off", false, true, true,
			"Retries stopped; this invoice is charged automatically once the customer adds a card.\n" + emailOffLine, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := classifyInvoiceAttention(noPMInvoice(), attentionContext{
				StripeConnected:  true,
				DunningEscalated: true, CollectionPaused: tc.pause,
				CustomerHasEmail: tc.hasEmail, SetupLinkEmailOff: tc.emailOff,
			})
			if att == nil || att.Reason != domain.AttentionReasonDunningExhausted {
				t.Fatalf("got %+v, want dunning_exhausted", att)
			}
			if att.Message != tc.msg {
				t.Errorf("message = %q, want %q", att.Message, tc.msg)
			}
			checkShape(t, att.Message)
			if !strings.Contains(att.Detail, "ended without collecting") {
				t.Errorf("detail = %q, want the recovery-ended explanation", att.Detail)
			}
			want := []domain.AttentionAction{domain.AttentionActionAddPaymentMethod}
			if tc.resend {
				want = []domain.AttentionAction{domain.AttentionActionSendReminder, domain.AttentionActionAddPaymentMethod}
			}
			if got := actionCodes(att); !sameActions(got, want...) {
				t.Errorf("actions = %v, want %v", got, want)
			}
		})
	}
}

// TestPaymentStateCopy_EveryBranch covers the remaining payment notices.
func TestPaymentStateCopy_EveryBranch(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	withCard := attentionContext{HasPaymentMethod: true, Now: now}
	queued := func() domain.Invoice {
		inv := noPMInvoice()
		inv.UpdatedAt = now
		return inv
	}
	cases := []struct {
		name    string
		inv     domain.Invoice
		atc     attentionContext
		reason  domain.AttentionReason
		sev     domain.AttentionSeverity
		msg     string
		detail  string // substring; "" means Detail must be empty
		actions []domain.AttentionAction
	}{
		{"scheduled", queued(), withCard, domain.AttentionReasonPaymentScheduled, domain.AttentionSeverityInfo,
			"The card on file is charged automatically, usually within an hour; use Charge now to collect it right away.",
			"hourly by default", []domain.AttentionAction{domain.AttentionActionChargeNow}},
		{"collection paused, card on file", queued(), attentionContext{HasPaymentMethod: true, CollectionPaused: true},
			domain.AttentionReasonCollectionPaused, domain.AttentionSeverityInfo,
			"Collection is paused on the subscription, so this invoice won't be charged automatically; use Charge now to collect it anyway.",
			"Resume collection", []domain.AttentionAction{domain.AttentionActionChargeNow}},
		{"awaiting (pre-ADR-116 row)", func() domain.Invoice { i := queued(); i.AutoChargePending = false; return i }(), withCard,
			domain.AttentionReasonAwaitingPayment, domain.AttentionSeverityInfo,
			"This invoice hasn't been charged yet; use Charge now, or email the customer a payment link.",
			"", []domain.AttentionAction{domain.AttentionActionChargeNow, domain.AttentionActionSendReminder}},
		{"payment page open", queued(), attentionContext{HasPaymentMethod: true, PaymentPageOpen: true},
			domain.AttentionReasonPaymentProcessing, domain.AttentionSeverityInfo,
			"The customer has the payment page open or just paid on it, so Hardcount holds charges until that finishes, usually within an hour.",
			"", nil},
		{"processing fresh", func() domain.Invoice { i := queued(); i.PaymentStatus = domain.PaymentProcessing; return i }(), withCard,
			domain.AttentionReasonPaymentProcessing, domain.AttentionSeverityInfo,
			"Stripe is confirming this payment, and this page updates automatically when it's done.",
			"", nil},
		{"processing stale", func() domain.Invoice {
			i := queued()
			i.PaymentStatus = domain.PaymentProcessing
			i.UpdatedAt = now.Add(-7 * time.Hour)
			return i
		}(), withCard, domain.AttentionReasonPaymentProcessing, domain.AttentionSeverityWarning,
			"Stripe still hasn't confirmed this payment after 6 hours, so check it in Stripe.",
			"usually confirmed within seconds", nil},
		{"unconfirmed with a payment id", func() domain.Invoice {
			i := queued()
			i.PaymentStatus = domain.PaymentUnknown
			i.StripePaymentIntentID = "pi_x"
			return i
		}(), withCard, domain.AttentionReasonPaymentUnconfirmed, domain.AttentionSeverityInfo,
			"Stripe hasn't confirmed this charge yet, and Hardcount re-checks automatically until it does.",
			"3-D Secure", nil},
		{"declined", func() domain.Invoice {
			i := queued()
			i.PaymentStatus = domain.PaymentFailed
			i.LastPaymentError = "Your card was declined."
			return i
		}(), withCard,
			domain.AttentionReasonPaymentFailed, domain.AttentionSeverityCritical,
			"The customer's card was declined.",
			"", []domain.AttentionAction{domain.AttentionActionUpdatePaymentMethod, domain.AttentionActionRetryPayment}},
		{"recovery charge in flight on a written-off invoice", func() domain.Invoice {
			i := queued()
			i.Status = domain.InvoiceUncollectible
			i.PaymentStatus = domain.PaymentProcessing
			return i
		}(), withCard, domain.AttentionReasonPaymentProcessing, domain.AttentionSeverityInfo,
			"Stripe is still confirming a payment on this written-off invoice, so don't charge it again.",
			"stays written off", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := classifyInvoiceAttention(tc.inv, tc.atc)
			if att == nil {
				t.Fatal("no attention")
			}
			if att.Reason != tc.reason || att.Severity != tc.sev {
				t.Errorf("reason/severity = %s/%s, want %s/%s", att.Reason, att.Severity, tc.reason, tc.sev)
			}
			if att.Message != tc.msg {
				t.Errorf("message = %q, want %q", att.Message, tc.msg)
			}
			checkShape(t, att.Message)
			if tc.detail == "" && att.Detail != "" {
				t.Errorf("detail = %q, want empty", att.Detail)
			}
			if tc.detail != "" && !strings.Contains(att.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to contain %q", att.Detail, tc.detail)
			}
			if got := actionCodes(att); !sameActions(got, tc.actions...) {
				t.Errorf("actions = %v, want %v", got, tc.actions)
			}
		})
	}
}

// TestPaymentAnomalyCopy_EveryKind: no raw payment id in the sentence, the id
// in Detail, and an "Open in Stripe" action whenever there is an id to open.
func TestPaymentAnomalyCopy_EveryKind(t *testing.T) {
	kinds := map[string]string{
		domain.EventPaymentDuplicateCharge:         "A second payment succeeded after this invoice was already paid, so refund the extra charge in Stripe.",
		domain.EventPaymentAmountMismatch:          "Stripe captured 96.52 USD but this invoice recorded 100.00 USD as paid, so reconcile the difference.",
		domain.EventPaymentReceivedOnVoidedInvoice: "A payment succeeded on this voided invoice, so the customer paid money they don't owe; refund it in Stripe.",
		domain.EventPaymentCurrencyMismatch:        "A payment was charged in a different currency than this invoice, so review it in Stripe and refund or correct it.",
		"payment.something_new":                    "Hardcount found a payment on this invoice that doesn't match its records, so review it in Stripe.",
	}
	for kind, want := range kinds {
		t.Run(kind, func(t *testing.T) {
			inv := domain.Invoice{
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
				AmountPaidCents: 10000, PaymentAnomalyKind: kind,
				PaymentAnomalyPaymentIntentID: "pi_extra", PaymentAnomalyCapturedCents: 9652,
			}
			att := classifyInvoiceAttention(inv, attentionContext{})
			if att == nil || att.Reason != domain.AttentionReasonPaymentAnomaly {
				t.Fatalf("got %+v", att)
			}
			if att.Message != want {
				t.Errorf("message = %q, want %q", att.Message, want)
			}
			checkShape(t, att.Message)
			if !strings.Contains(att.Detail, "pi_extra") {
				t.Errorf("detail = %q, want the payment id", att.Detail)
			}
			if got := actionCodes(att); !sameActions(got, domain.AttentionActionReconcilePayment) {
				t.Errorf("actions = %v, want [reconcile_payment]", got)
			}
			// No id: nothing to open, so no button.
			inv.PaymentAnomalyPaymentIntentID = ""
			if att := classifyInvoiceAttention(inv, attentionContext{}); len(att.Actions) != 0 {
				t.Errorf("anomaly without a payment id offers %v, want no action", actionCodes(att))
			}
		})
	}
}

// TestTaxCopy_Shape: every tax branch keeps the one-sentence shape, and the
// two branches that name a retry count keep the real numbers.
func TestTaxCopy_Shape(t *testing.T) {
	for _, code := range []string{"customer_data_invalid", "jurisdiction_unsupported", "provider_outage", "provider_auth", "provider_not_configured", "unknown"} {
		for _, exhausted := range []bool{false, true} {
			for _, connected := range []bool{false, true} {
				inv := domain.Invoice{Status: domain.InvoiceFinalized, TaxFacts: domain.TaxFacts{TaxStatus: domain.InvoiceTaxPending, TaxErrorCode: code}}
				inv.TaxRetryCount = 3
				if exhausted {
					inv.TaxRetryCount = domain.MaxTaxRetryAttempts
				}
				att := classifyInvoiceAttention(inv, attentionContext{StripeConnected: connected})
				if att == nil {
					t.Fatalf("%s: no attention", code)
				}
				checkShape(t, att.Message)
				retryable := domain.TaxErrorCodeRetryable(code) && code != "provider_not_configured"
				if retryable && !exhausted && !strings.Contains(att.Message, "(3 of 8 attempts used)") {
					t.Errorf("%s: retrying copy must state attempts used, got %q", code, att.Message)
				}
				if exhausted && strings.Contains(att.Message, "retries automatically") {
					t.Errorf("%s: exhausted copy promises automatic retries: %q", code, att.Message)
				}
			}
		}
	}
}

// TestPausedCollectionCopy_OpenRunKeepsRetrying (sweep A2, 2026-10-21): a
// collection pause keeps NEW invoices as drafts, but a dunning run already
// open on an invoice keeps retrying it, as Stripe keeps retrying invoices
// created before a pause. The paused notices said "adding a card won't charge
// this invoice" and "won't be charged automatically" whatever the run was
// doing, so an operator was told a charge would not happen that the next
// retry made. With an open run, both notices say the retries continue and the
// Details say how to stop them; without one, the old copy stays (it is true).
func TestPausedCollectionCopy_OpenRunKeepsRetrying(t *testing.T) {
	const (
		noCardRetrying   = "Collection is paused, but payment retries for this invoice continue, so the next retry charges a card the customer adds."
		noCardStopped    = "Collection is paused on the subscription, so adding a card won't charge this invoice until you resume it."
		withCardRetrying = "Collection is paused, but payment retries for this invoice continue, so the next retry charges the card on file."
		withCardStopped  = "Collection is paused on the subscription, so this invoice won't be charged automatically; use Charge now to collect it anyway."
		howToStop        = "To stop the retries, void this invoice or mark it uncollectible from the ⋯ menu above."
	)
	cases := []struct {
		name     string
		hasCard  bool
		retrying bool
		reason   domain.AttentionReason
		line1    string
		stopHint bool
	}{
		{"no card, run retrying", false, true, domain.AttentionReasonNoPaymentMethod, noCardRetrying, true},
		{"no card, no open run", false, false, domain.AttentionReasonNoPaymentMethod, noCardStopped, false},
		{"card on file, run retrying", true, true, domain.AttentionReasonCollectionPaused, withCardRetrying, true},
		{"card on file, no open run", true, false, domain.AttentionReasonCollectionPaused, withCardStopped, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := classifyInvoiceAttention(noPMInvoice(), attentionContext{
				HasPaymentMethod: tc.hasCard, CollectionPaused: true, DunningRetrying: tc.retrying,
				CustomerHasEmail: true, StripeConnected: true,
			})
			if att == nil || att.Reason != tc.reason {
				t.Fatalf("got %+v, want %s", att, tc.reason)
			}
			if got := strings.Split(att.Message, "\n")[0]; got != tc.line1 {
				t.Errorf("line 1 =\n%q\nwant\n%q", got, tc.line1)
			}
			checkShape(t, att.Message)
			if has := strings.Contains(att.Detail, howToStop); has != tc.stopHint {
				t.Errorf("detail says how to stop the retries = %v, want %v:\n%s", has, tc.stopHint, att.Detail)
			}
		})
	}
}

// With no HOSTED_INVOICE_BASE_URL the hosted invoice page takes no payment
// and the payment email has no link. The authentication_required notice
// must not tell the operator to email that link: it says to set the address,
// and offers no email action.
func TestNeedsConfirmationAttention_HostedPayOff(t *testing.T) {
	failed := domain.Invoice{
		ID: "inv_1", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		LastPaymentError: "This payment requires authentication.",
		UpdatedAt:        time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
	}
	for _, retrying := range []bool{true, false} {
		att := classifyInvoiceAttention(failed, attentionContext{
			HasPaymentMethod: true, RetryBlockingDeclineCode: domain.AuthenticationRequiredCode,
			DunningRetrying: retrying, HostedPayOff: true,
		})
		if att == nil || att.Code != "payment.authentication_required" {
			t.Fatalf("retrying=%v: attention %+v, want payment.authentication_required", retrying, att)
		}
		checkShape(t, att.Message)
		if !strings.Contains(att.Message, "HOSTED_INVOICE_BASE_URL") || !strings.Contains(att.Detail, "HOSTED_INVOICE_BASE_URL") {
			t.Errorf("retrying=%v: message/detail do not name the setting:\n%s\n%s", retrying, att.Message, att.Detail)
		}
		if strings.Contains(att.Message, "email them the payment link") {
			t.Errorf("retrying=%v: message still sends the operator to a link that doesn't exist: %q", retrying, att.Message)
		}
		if len(att.Actions) != 0 {
			t.Errorf("retrying=%v: actions = %v, want none (the payment email would carry no link)", retrying, actionCodes(att))
		}
	}
}

// The card_refused notice offers the hosted invoice page only when it can
// take payment.
func TestRefusedCardAttention_HostedPayOff(t *testing.T) {
	failed := domain.Invoice{
		ID: "inv_1", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		LastPaymentError: "Your card was declined.",
		UpdatedAt:        time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC),
	}
	for _, retrying := range []bool{true, false} {
		on := classifyInvoiceAttention(failed, attentionContext{HasPaymentMethod: true, RetryBlockingDeclineCode: "stolen_card", DunningRetrying: retrying})
		off := classifyInvoiceAttention(failed, attentionContext{HasPaymentMethod: true, RetryBlockingDeclineCode: "stolen_card", DunningRetrying: retrying, HostedPayOff: true})
		if on == nil || off == nil || on.Code != "payment.card_refused" || off.Code != "payment.card_refused" {
			t.Fatalf("retrying=%v: got %+v / %+v, want payment.card_refused", retrying, on, off)
		}
		if !strings.Contains(on.Detail, "hosted invoice page") {
			t.Errorf("retrying=%v: with the page working, detail no longer offers it: %q", retrying, on.Detail)
		}
		if strings.Contains(off.Detail, "hosted invoice page") {
			t.Errorf("retrying=%v: with no hosted address, detail still offers the page: %q", retrying, off.Detail)
		}
		if strings.HasSuffix(off.Detail, " ") {
			t.Errorf("retrying=%v: detail ends with a space: %q", retrying, off.Detail)
		}
	}
}

// With no CUSTOMER_PORTAL_URL the customer page can't make a setup link to
// copy, so the no-payment-method notice doesn't offer one.
func TestNoPaymentMethodCopy_SetupLinkCopyOff(t *testing.T) {
	const copyFrom = "copy one from the customer page"
	cases := []struct {
		name               string
		hasEmail, emailOff bool
		line2              string
	}{
		{"email, sending works", true, false, "A setup link is emailed to the customer once; use Resend setup link to send it again."},
		{"no email", false, false, "Setup links are emailed only to customers with an email address, and this server can't make one to copy."},
		{"email off", true, true, "This server can't email setup links or make one to copy, so none was sent."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := classifyInvoiceAttention(noPMInvoice(), attentionContext{
				StripeConnected: true, CustomerHasEmail: tc.hasEmail, SetupLinkEmailOff: tc.emailOff, SetupLinkCopyOff: true,
			})
			if att == nil || att.Reason != domain.AttentionReasonNoPaymentMethod {
				t.Fatalf("got %+v, want no_payment_method", att)
			}
			checkShape(t, att.Message)
			lines := strings.Split(att.Message, "\n")
			if got := lines[len(lines)-1]; got != tc.line2 {
				t.Errorf("line 2 = %q, want %q", got, tc.line2)
			}
			if strings.Contains(att.Message, copyFrom) || strings.Contains(att.Detail, "can also copy") {
				t.Errorf("notice still offers a copied link:\n%s\n%s", att.Message, att.Detail)
			}
			if !strings.Contains(att.Detail, "CUSTOMER_PORTAL_URL") {
				t.Errorf("detail does not say how to make copying work: %q", att.Detail)
			}
		})
	}
}

func belowMinimumInvoice() domain.Invoice {
	at := time.Date(2026, 10, 22, 6, 46, 57, 0, time.UTC)
	return domain.Invoice{
		ID: "hc_inv_min", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		AmountDueCents: 30, Currency: "USD", UpdatedAt: at.Add(time.Hour),
		TaxFacts:         domain.TaxFacts{TaxStatus: domain.InvoiceTaxOK},
		LastPaymentError: "Amount must be at least $0.50 usd",
		BelowMinimumAt:   &at,
	}
}

// An invoice Stripe refused as below its minimum charge reads as that, for
// the operator to act on, and not as a decline, a scheduled charge or a
// missing card: each of those promises a charge that cannot happen.
func TestClassify_AmountBelowMinimum(t *testing.T) {
	inv := belowMinimumInvoice()
	for name, atc := range map[string]attentionContext{
		"card on file":       {HasPaymentMethod: true},
		"no card on file":    {},
		"collection paused":  {HasPaymentMethod: true, CollectionPaused: true},
		"retry still queued": {HasPaymentMethod: true},
	} {
		t.Run(name, func(t *testing.T) {
			in := inv
			if name == "retry still queued" {
				in.AutoChargePending = true
			}
			att := classifyInvoiceAttention(in, atc)
			if att == nil || att.Reason != domain.AttentionReasonAmountBelowMinimum || att.Code != "payment.amount_too_small" ||
				att.Severity != domain.AttentionSeverityWarning {
				t.Fatalf("attention %+v, want warning amount_below_minimum / payment.amount_too_small", att)
			}
			checkShape(t, att.Message)
			if !sameActions(actionCodes(att), domain.AttentionActionVoidInvoice) {
				t.Errorf("actions %v, want only void_invoice", actionCodes(att))
			}
			if att.ProviderResponse != inv.LastPaymentError {
				t.Errorf("provider_response %q, want Stripe's sentence", att.ProviderResponse)
			}
			if att.Since == nil || !att.Since.Equal(*inv.BelowMinimumAt) {
				t.Errorf("since %v, want the refusal's time %v", att.Since, inv.BelowMinimumAt)
			}
			for _, w := range []string{"void", "record a payment"} {
				if !strings.Contains(att.Message, w) {
					t.Errorf("message %q does not name %q", att.Message, w)
				}
			}
			if !strings.Contains(att.Detail, "next invoice") {
				t.Errorf("detail %q does not say how to bill it on the next invoice", att.Detail)
			}
			// Collect payment applies a credit balance before it charges, so
			// it can still pay the invoice: the detail says so.
			if !strings.Contains(att.Detail, "credit balance") {
				t.Errorf("detail %q does not say Collect payment applies a credit balance", att.Detail)
			}
			// The minimum depends on the account's settlement currency, so
			// Hardcount names no figure; Stripe's sentence in provider_response
			// does.
			if strings.Contains(att.Detail, "$0.50") {
				t.Errorf("detail %q names a minimum; it is true only for an account that settles in USD", att.Detail)
			}
		})
	}

	t.Run("a later decline or a paid invoice reads as itself", func(t *testing.T) {
		failed := belowMinimumInvoice()
		failed.PaymentStatus = domain.PaymentFailed
		if att := classifyInvoiceAttention(failed, attentionContext{HasPaymentMethod: true}); att == nil || att.Reason != domain.AttentionReasonPaymentFailed {
			t.Errorf("failed: %+v, want payment_failed", att)
		}
		paid := belowMinimumInvoice()
		paid.Status, paid.PaymentStatus = domain.InvoicePaid, domain.PaymentSucceeded
		if att := classifyInvoiceAttention(paid, attentionContext{}); att != nil {
			t.Errorf("paid: %+v, want none", att)
		}
	})
}

// TestUnconfirmedWithAPaymentIntentKeepsTheWaitingCopy: a charge that is
// being confirmed resolves itself, so the notice says to wait and offers no
// write-off.
func TestUnconfirmedWithAPaymentIntentKeepsTheWaitingCopy(t *testing.T) {
	confirming := domain.Invoice{
		Status:                domain.InvoiceFinalized,
		PaymentStatus:         domain.PaymentUnknown,
		StripePaymentIntentID: "pi_live_123",
		AmountDueCents:        5000,
	}

	att := classifyPaymentUnconfirmed(confirming)
	if att == nil {
		t.Fatal("want an attention")
	}
	if att.Severity != domain.AttentionSeverityInfo {
		t.Errorf("severity = %v, want info — this one really is resolving itself", att.Severity)
	}
	if !strings.Contains(att.Message, "re-checks automatically") {
		t.Error("a resolvable charge should still tell the operator to wait")
	}
	if len(att.Actions) != 0 {
		t.Errorf("a charge that resolves itself must not offer a write-off, got %+v", att.Actions)
	}
	if !strings.Contains(att.Detail, "Check the payment in Stripe") {
		t.Errorf("detail must keep the stuck-for-hours advice, got %q", att.Detail)
	}
}

// TestClassifyInvoiceAttention_PaymentAnomalyPiercesTerminal locks the
// ADR-068 exception: the payment-anomaly banner fires on PAID and VOIDED
// invoices — exactly the rows the terminal early-return hides, and exactly
// where a duplicate charge lands. Mutation seam: move the anomaly check
// below the terminal return and both terminal cases yield nil.
func TestClassifyInvoiceAttention_PaymentAnomalyPiercesTerminal(t *testing.T) {
	paidDup := domain.Invoice{
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded,
		StripePaymentIntentID:         "pi_A",
		PaymentAnomalyKind:            domain.EventPaymentDuplicateCharge,
		PaymentAnomalyPaymentIntentID: "pi_B",
	}
	att := classifyInvoiceAttention(paidDup, attentionContext{})
	if att == nil {
		t.Fatal("duplicate-charge anomaly on a PAID invoice produced no attention — the terminal early-return swallowed it")
	}
	if att.Reason != domain.AttentionReasonPaymentAnomaly || att.Severity != domain.AttentionSeverityCritical {
		t.Fatalf("attention = %s/%s, want payment_anomaly/critical", att.Reason, att.Severity)
	}
	// The ids are for support and the Stripe search, so they sit in Detail;
	// the sentence an operator reads first stays plain.
	if strings.Contains(att.Message, "pi_") {
		t.Errorf("message must not carry raw payment ids: %q", att.Message)
	}
	if !strings.Contains(att.Detail, "pi_B") || !strings.Contains(att.Detail, "pi_A") {
		t.Errorf("detail must name both PaymentIntents: %q", att.Detail)
	}
	if len(att.Actions) != 1 || att.Actions[0].Code != domain.AttentionActionReconcilePayment {
		t.Errorf("actions = %+v, want [reconcile_payment] (open in Stripe)", att.Actions)
	}

	voidedPay := domain.Invoice{
		Status:                        domain.InvoiceVoided,
		PaymentAnomalyKind:            domain.EventPaymentReceivedOnVoidedInvoice,
		PaymentAnomalyPaymentIntentID: "pi_Z",
	}
	if att := classifyInvoiceAttention(voidedPay, attentionContext{}); att == nil || att.Code != domain.EventPaymentReceivedOnVoidedInvoice {
		t.Fatalf("voided-payment anomaly attention = %+v, want Critical with the per-cause code", att)
	}

	// No anomaly: paid invoices stay banner-free (the early-return contract).
	clean := domain.Invoice{Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded}
	if att := classifyInvoiceAttention(clean, attentionContext{}); att != nil {
		t.Fatalf("clean paid invoice grew a banner: %+v", att)
	}
}

// A card payment on an invoice already paid outside Hardcount: the detail names
// the extra charge and says how the first payment was made. It has no
// PaymentIntent to name, and must not pretend to.
func TestClassifyInvoiceAttention_DuplicateChargeAfterOfflinePayment(t *testing.T) {
	inv := domain.Invoice{
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, PaidOutOfBand: true,
		PaymentAnomalyKind:            domain.EventPaymentDuplicateCharge,
		PaymentAnomalyPaymentIntentID: "pi_B",
	}
	att := classifyInvoiceAttention(inv, attentionContext{})
	if att == nil {
		t.Fatal("no attention for a second payment on an offline-paid invoice")
	}
	want := "Extra payment: pi_B. Original payment: recorded as paid outside Hardcount."
	if !strings.HasPrefix(att.Detail, want) {
		t.Errorf("detail = %q, want it to start %q", att.Detail, want)
	}
}

// TestNoPaymentMethodCopy_NoStripe: while Stripe is not connected for the
// invoice's mode no card can be added online, so the collector emails the
// customer the invoice instead of a setup link (ADR-060 amendment
// 2026-10-30). The notice says so, says what happened to the invoice email,
// and offers no Resend setup link (there is no link); the customer page stays.
// Once recovery has ended it says retries stopped. Mutation: drop the
// !StripeConnected branch (the notice promises a charge once a card is added
// and offers Resend setup link).
func TestNoPaymentMethodCopy_NoStripe(t *testing.T) {
	const (
		line1   = "Stripe isn't connected in this mode, so no card can be added online."
		sent    = "The invoice is emailed to the customer once."
		noEmail = "The invoice is emailed only to customers with an email address; add one on the customer page."
		off     = "This server can't send email, so the invoice was not emailed to the customer."
	)
	cases := []struct {
		name                      string
		pause                     bool
		hasEmail, emailOff, ended bool
		reason                    domain.AttentionReason
		message                   string
	}{
		{"email", false, true, false, false, domain.AttentionReasonNoPaymentMethod, line1 + "\n" + sent},
		{"no email", false, false, false, false, domain.AttentionReasonNoPaymentMethod, line1 + "\n" + noEmail},
		{"email off", false, true, true, false, domain.AttentionReasonNoPaymentMethod, line1 + "\n" + off},
		{"no email and email off", false, false, true, false, domain.AttentionReasonNoPaymentMethod, line1 + "\n" + noEmail},
		{"paused, email", true, true, false, false, domain.AttentionReasonNoPaymentMethod, line1 + "\n" + sent},
		{"recovery ended", false, true, false, true, domain.AttentionReasonDunningExhausted, "Retries stopped; " + line1 + "\n" + sent},
		{"recovery ended, paused", true, true, false, true, domain.AttentionReasonDunningExhausted, "Retries stopped and collection is paused; " + line1 + "\n" + sent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := classifyInvoiceAttention(noPMInvoice(), attentionContext{
				StripeConnected: false, CustomerHasEmail: tc.hasEmail, InvoiceEmailOff: tc.emailOff,
				CollectionPaused: tc.pause, DunningEscalated: tc.ended,
			})
			if att == nil || att.Reason != tc.reason {
				t.Fatalf("got %+v, want %s", att, tc.reason)
			}
			if att.Message != tc.message {
				t.Errorf("message =\n%q\nwant\n%q", att.Message, tc.message)
			}
			checkShape(t, att.Message)
			for _, banned := range []string{"setup link", "charged automatically", "adds a card"} {
				if strings.Contains(att.Message, banned) || strings.Contains(att.Detail, banned) {
					t.Errorf("notice mentions %q without Stripe:\n%s\n%s", banned, att.Message, att.Detail)
				}
			}
			if !strings.Contains(att.Detail, "connect Stripe under Settings") {
				t.Errorf("detail %q does not say how to collect by card", att.Detail)
			}
			if got := actionCodes(att); !sameActions(got, domain.AttentionActionAddPaymentMethod) {
				t.Errorf("actions = %v, want only Open customer page", got)
			}
		})
	}

	// With Stripe connected the setup-link copy is unchanged.
	att := classifyInvoiceAttention(noPMInvoice(), attentionContext{StripeConnected: true, CustomerHasEmail: true})
	if att == nil || !strings.Contains(att.Message, "A setup link is emailed to the customer once") {
		t.Fatalf("connected: got %+v, want the setup-link copy", att)
	}
}

// P4 of the Stripe card-flow review (2026-10-22): an off-session charge the
// bank wants the customer to confirm read "The customer's card was declined"
// and offered Retry payment. The notice says what the bank asked, offers the
// payment link (the hosted page runs 3-D Secure), never Retry or Update
// payment method, and promises retries only while one is scheduled.
func TestNeedsConfirmationAttention(t *testing.T) {
	failed := domain.Invoice{
		ID: "inv_1", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		LastPaymentError: "This payment requires authentication. If the customer is off session, ask them to return to your application and complete the payment.",
		UpdatedAt:        time.Date(2026, 10, 22, 21, 0, 0, 0, time.UTC),
	}
	for _, retrying := range []bool{true, false} {
		att := classifyInvoiceAttention(failed, attentionContext{
			HasPaymentMethod: true, RetryBlockingDeclineCode: domain.AuthenticationRequiredCode, DunningRetrying: retrying,
		})
		if att == nil || att.Code != "payment.authentication_required" || att.Reason != domain.AttentionReasonPaymentFailed {
			t.Fatalf("retrying=%v: attention %+v, want payment_failed / payment.authentication_required", retrying, att)
		}
		checkShape(t, att.Message)
		if !strings.Contains(att.Message, "bank needs them to confirm") || strings.Contains(strings.ToLower(att.Message), "declin") {
			t.Errorf("retrying=%v: message %q, want the bank's request and no decline", retrying, att.Message)
		}
		if !sameActions(actionCodes(att), domain.AttentionActionSendReminder) {
			t.Errorf("retrying=%v: actions %v, want only send_reminder", retrying, actionCodes(att))
		}
		if att.Severity != domain.AttentionSeverityWarning {
			t.Errorf("retrying=%v: severity %s, want warning", retrying, att.Severity)
		}
		// Stripe's developer text stays labelled as the provider's, never
		// Hardcount's own words.
		if att.ProviderResponse != failed.LastPaymentError || strings.Contains(att.Detail, "off session") {
			t.Errorf("retrying=%v: provider response %q / detail %q", retrying, att.ProviderResponse, att.Detail)
		}
		// 3-D Secure can pass without a challenge, so the bank may not ask.
		if strings.Contains(att.Detail, "bank asks them") || !strings.Contains(att.Detail, "bank may ask them to confirm") {
			t.Errorf("retrying=%v: detail must say the bank may ask, not that it will:\n%s", retrying, att.Detail)
		}
		promises := strings.Contains(att.Detail, "keeps its schedule")
		if promises != retrying {
			t.Errorf("retrying=%v: detail promises retries = %v:\n%s", retrying, promises, att.Detail)
		}
		if !retrying && !strings.Contains(att.Detail, "No automatic retry is scheduled") {
			t.Errorf("retrying=false: detail does not say no retry is scheduled:\n%s", att.Detail)
		}
	}

	// Controls: a hard decline is still "needs a different card", and an
	// ordinary decline still offers Retry.
	if att := classifyInvoiceAttention(failed, attentionContext{HasPaymentMethod: true, RetryBlockingDeclineCode: "stolen_card"}); att == nil || att.Code != "payment.card_refused" {
		t.Errorf("hard decline: attention %+v, want payment.card_refused", att)
	}
	att := classifyInvoiceAttention(failed, attentionContext{HasPaymentMethod: true})
	if att == nil || att.Code != "payment.declined" || !sameActions(actionCodes(att), domain.AttentionActionUpdatePaymentMethod, domain.AttentionActionRetryPayment) {
		t.Errorf("ordinary decline: attention %+v, want payment.declined with Update payment method and Retry payment", att)
	}
}

func refusedCardInvoice() domain.Invoice {
	return domain.Invoice{
		ID: "inv_1", Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed,
		LastPaymentError: "Your card was declined.",
	}
}

func TestClassifyAttention_RefusedCard_AsksForADifferentCard(t *testing.T) {
	att := classifyInvoiceAttention(refusedCardInvoice(), attentionContext{
		HasPaymentMethod: true, RetryBlockingDeclineCode: "lost_card",
	})
	if att == nil {
		t.Fatal("no attention on a failed invoice")
	}
	if att.Reason != domain.AttentionReasonPaymentFailed || att.Severity != domain.AttentionSeverityCritical || att.Code != "payment.card_refused" {
		t.Errorf("attention = %s/%s/%s, want payment_failed/critical/payment.card_refused", att.Reason, att.Severity, att.Code)
	}
	if !strings.Contains(att.Message, "different card") {
		t.Errorf("message %q does not tell the operator the customer needs a different card", att.Message)
	}
	if len(strings.Fields(strings.Split(att.Message, "\n")[0])) > 25 {
		t.Errorf("message %q is longer than 25 words", att.Message)
	}
	if !strings.Contains(att.Detail, "lost_card") {
		t.Errorf("detail %q does not name Stripe's decline code", att.Detail)
	}
	// Retry would charge the card the bank refused: not offered.
	if len(att.Actions) != 1 || att.Actions[0].Code != domain.AttentionActionUpdatePaymentMethod {
		t.Errorf("actions = %+v, want only update_payment_method", att.Actions)
	}
	if att.ProviderResponse != "Your card was declined." {
		t.Errorf("provider response %q, want Stripe's message", att.ProviderResponse)
	}
}

// Control: without a refused card the ordinary decline notice is unchanged,
// Retry included — the next retry will charge the card.
func TestClassifyAttention_OrdinaryDecline_Unchanged(t *testing.T) {
	att := classifyInvoiceAttention(refusedCardInvoice(), attentionContext{HasPaymentMethod: true})
	if att == nil || att.Code != "payment.declined" {
		t.Fatalf("attention = %+v, want payment.declined", att)
	}
	if len(att.Actions) != 2 || att.Actions[1].Code != domain.AttentionActionRetryPayment {
		t.Errorf("actions = %+v, want update_payment_method then retry_payment", att.Actions)
	}
}

func lostDispute(closedAt time.Time) *domain.InvoiceDispute {
	return &domain.InvoiceDispute{
		StripeDisputeID: "du_lost_1", StripePaymentIntentID: "pi_lost_1", Status: domain.DisputeLost,
		Reason: "fraudulent", AmountCents: 4500, Currency: "USD", ClosedAt: &closedAt,
	}
}

// TestClassifyDisputeLost: a lost dispute raises dispute_lost on the paid
// invoice, through the terminal-status early return that hides every other
// payment notice there. Critical, no action button (the decision is the
// operator's), the amount in the sentence and the Stripe dispute id in
// Detail. Without a lost dispute a paid invoice stays quiet. (An anomaly on
// the same invoice, and an invoice that bought prepaid credit, have their own
// tests below.)
func TestClassifyDisputeLost(t *testing.T) {
	closedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	paid := domain.Invoice{ID: "hc_inv_d", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD"}

	if att := classifyInvoiceAttention(paid, attentionContext{}); att != nil {
		t.Fatalf("control: paid invoice without a lost dispute has attention %+v", att)
	}
	for _, status := range []domain.InvoiceStatus{domain.InvoicePaid, domain.InvoiceVoided, domain.InvoiceUncollectible, domain.InvoiceFinalized} {
		inv := paid
		inv.Status = status
		att := classifyInvoiceAttention(inv, attentionContext{LostDispute: lostDispute(closedAt)})
		if att == nil || att.Reason != domain.AttentionReasonDisputeLost {
			t.Fatalf("%s invoice with a lost dispute: %+v, want dispute_lost", status, att)
		}
		if att.Severity != domain.AttentionSeverityCritical || len(att.Actions) != 0 || att.Code != "payment.dispute_lost" {
			t.Errorf("%s: severity %s, actions %v, code %q; want critical, none, payment.dispute_lost", status, att.Severity, att.Actions, att.Code)
		}
		checkShape(t, att.Message)
		if !strings.Contains(att.Message, domain.FormatMoneyMinor(4500, "USD")) {
			t.Errorf("message %q does not state the amount taken back", att.Message)
		}
		if !strings.Contains(att.Detail, "du_lost_1") || !strings.Contains(att.Detail, "not as a card refund") {
			t.Errorf("detail %q must name the Stripe dispute and warn against a card refund", att.Detail)
		}
		if att.Since == nil || !att.Since.Equal(closedAt) {
			t.Errorf("since = %v, want the close %s", att.Since, closedAt)
		}
	}

}

// TestClassifyDisputeLost_WithPaymentAnomaly: an invoice charged twice whose
// original payment was then disputed and lost. The anomaly keeps its reason
// and its "Open in Stripe" button, but it no longer says to refund the extra
// charge: the bank already gave the original back, so that refund would
// return the customer's money a second time. The notice states the loss and
// Detail names the dispute and the disputed payment beside the anomaly's own
// facts.
func TestClassifyDisputeLost_WithPaymentAnomaly(t *testing.T) {
	closedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	inv := domain.Invoice{
		ID: "hc_inv_d", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
		StripePaymentIntentID:         "pi_lost_1",
		PaymentAnomalyKind:            domain.EventPaymentDuplicateCharge,
		PaymentAnomalyPaymentIntentID: "pi_extra_1",
	}
	control := classifyInvoiceAttention(inv, attentionContext{})
	if control == nil || !strings.Contains(control.Message, "refund the extra charge") {
		t.Fatalf("control: the duplicate-charge notice without a dispute must ask for the refund, got %+v", control)
	}

	att := classifyInvoiceAttention(inv, attentionContext{LostDispute: lostDispute(closedAt)})
	if att == nil || att.Reason != domain.AttentionReasonPaymentAnomaly || att.Severity != domain.AttentionSeverityCritical {
		t.Fatalf("anomaly + lost dispute: %+v, want a critical payment_anomaly", att)
	}
	if got := actionCodes(att); len(got) != 1 || got[0] != domain.AttentionActionReconcilePayment {
		t.Errorf("actions = %v, want only reconcile_payment (open the payments in Stripe)", got)
	}
	checkShape(t, att.Message)
	if strings.Contains(att.Message, "refund the extra charge") {
		t.Errorf("message %q still asks for the refund the chargeback may already have paid", att.Message)
	}
	if !strings.Contains(att.Message, domain.FormatMoneyMinor(4500, "USD")) || !strings.Contains(att.Message, "lost dispute") {
		t.Errorf("message %q must state the amount the bank took back after a lost dispute", att.Message)
	}
	for _, want := range []string{"du_lost_1", "pi_lost_1", "Extra payment: pi_extra_1"} {
		if !strings.Contains(att.Detail, want) {
			t.Errorf("detail %q does not name %q", att.Detail, want)
		}
	}
}

// TestClassifyDisputeLost_PrepaidCommit: a lost dispute on a paid invoice that
// bought prepaid credit. The bank has the money back but the credit is still
// spendable, and an ordinary credit note is refused on such an invoice. So
// the notice says how much can still be spent and names the one action that
// retires it (commit relief, as money returned outside Hardcount); once the
// credit is all spent there is nothing to retire and no credit note to point
// to.
func TestClassifyDisputeLost_PrepaidCommit(t *testing.T) {
	closedAt := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	inv := domain.Invoice{
		ID: "hc_inv_c", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
		BillingReason: domain.BillingReasonManual,
	}

	t.Run("credit left", func(t *testing.T) {
		att := classifyInvoiceAttention(inv, attentionContext{
			LostDispute: lostDispute(closedAt), CommitGrantDrawableCents: 6000, CommitGrantConsumedCents: 4000,
		})
		if att == nil || att.Reason != domain.AttentionReasonDisputeLost || att.Severity != domain.AttentionSeverityCritical {
			t.Fatalf("attention %+v, want critical dispute_lost", att)
		}
		checkShape(t, att.Message)
		if !strings.Contains(att.Message, domain.FormatMoneyMinor(6000, "USD")) {
			t.Errorf("message %q must say how much prepaid credit is still spendable", att.Message)
		}
		if !strings.Contains(att.Message, "pause the customer's key") || strings.Contains(att.Message, "commit relief") {
			t.Errorf("message %q must say to pause the key, and not name commit relief, which is refused after a loss", att.Message)
		}
		for _, want := range []string{"refused after a lost dispute", "du_lost_1"} {
			if !strings.Contains(att.Detail, want) {
				t.Errorf("detail %q does not name %q", att.Detail, want)
			}
		}
	})

	t.Run("credit all spent", func(t *testing.T) {
		att := classifyInvoiceAttention(inv, attentionContext{
			LostDispute: lostDispute(closedAt), CommitGrantDrawableCents: 0, CommitGrantConsumedCents: 10000,
		})
		if att == nil || att.Reason != domain.AttentionReasonDisputeLost {
			t.Fatalf("attention %+v, want dispute_lost", att)
		}
		checkShape(t, att.Message)
		if strings.Contains(att.Message, "credit note") {
			t.Errorf("message %q points to a credit note, which this invoice refuses and which has nothing to retire", att.Message)
		}
		if !strings.Contains(att.Detail, "already spent") {
			t.Errorf("detail %q must say the credit is already spent", att.Detail)
		}
	})
}

// TestClassifyRefundedInStripe: a flagged refund made in Stripe pierces the
// paid-invoice early return as a Warning, names the refund and the way to
// record or dismiss it, and says why it was not recorded.
func TestClassifyRefundedInStripe(t *testing.T) {
	inv := domain.Invoice{ID: "inv_1", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD"}
	if att := classifyInvoiceAttention(inv, attentionContext{}); att != nil {
		t.Fatalf("a paid invoice with nothing to report raised %+v", att)
	}
	reasons := map[domain.StripeRefundReviewReason]string{
		domain.StripeRefundReviewInvoiceNotPaid:         "not paid",
		domain.StripeRefundReviewCurrencyMismatch:       "EUR",
		domain.StripeRefundReviewPrepaidCommit:          "commit relief",
		domain.StripeRefundReviewSeenLate:               "more than a day",
		domain.StripeRefundReviewCreditNoteRefundFailed: "card refund that failed",
		domain.StripeRefundReviewExceedsCardRefundable:  "card payment",
		domain.StripeRefundReviewExceedsCreditable:      "left to credit",
	}
	for reason, word := range reasons {
		r := domain.InvoiceStripeRefund{
			StripeRefundID: "re_1", StripePaymentIntentID: "pi_1", AmountCents: 4000, Currency: "EUR",
			Outcome: domain.StripeRefundNeedsReview, ReviewReason: reason, RefundCreatedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC),
		}
		att := classifyInvoiceAttention(inv, attentionContext{StripeRefundReview: &r})
		if att == nil {
			t.Fatalf("%s: no attention on a paid invoice", reason)
		}
		if att.Reason != domain.AttentionReasonRefundedInStripe || att.Severity != domain.AttentionSeverityWarning {
			t.Errorf("%s: %s/%s, want refunded_in_stripe/warning", reason, att.Reason, att.Severity)
		}
		if att.Code != "payment.refunded_in_stripe."+string(reason) {
			t.Errorf("%s: code %q", reason, att.Code)
		}
		if !strings.Contains(att.Message, word) || !strings.Contains(att.Message, "40.00 EUR") {
			t.Errorf("%s: message %q should name %q and the amount", reason, att.Message, word)
		}
		if !strings.Contains(att.Detail, "re_1") || !strings.Contains(att.Detail, "/v1/invoices/inv_1/stripe-refunds/re_1/dismiss") {
			t.Errorf("%s: detail %q should name the refund and the dismiss route", reason, att.Detail)
		}
		if att.Since == nil || !att.Since.Equal(r.RefundCreatedAt) {
			t.Errorf("%s: since %v, want the refund's creation", reason, att.Since)
		}
	}
	// A lost dispute is Critical and wins.
	lost := domain.InvoiceDispute{StripeDisputeID: "du_1", Status: domain.DisputeLost, AmountCents: 100, Currency: "USD"}
	r := domain.InvoiceStripeRefund{Outcome: domain.StripeRefundNeedsReview, ReviewReason: domain.StripeRefundReviewSeenLate, Currency: "USD", AmountCents: 100}
	if att := classifyInvoiceAttention(inv, attentionContext{LostDispute: &lost, StripeRefundReview: &r}); att == nil || att.Reason != domain.AttentionReasonDisputeLost {
		t.Errorf("lost dispute and flagged refund: %+v, want dispute_lost", att)
	}
}

// TestClassifyRefundedInStripe_OnAnAnomalyInvoice: an invoice with a
// payment anomaly keeps the anomaly's notice, and the notice also names the
// flagged refund made in Stripe, first, so the refund is never hidden behind
// the anomaly.
func TestClassifyRefundedInStripe_OnAnAnomalyInvoice(t *testing.T) {
	inv := domain.Invoice{ID: "inv_1", Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, Currency: "USD",
		PaymentAnomalyKind: "amount_mismatch", PaymentAnomalyPaymentIntentID: "pi_1"}
	plain := classifyInvoiceAttention(inv, attentionContext{})
	if plain == nil || plain.Reason != domain.AttentionReasonPaymentAnomaly {
		t.Fatalf("anomaly invoice attention = %+v, want payment_anomaly", plain)
	}
	r := domain.InvoiceStripeRefund{StripeRefundID: "re_1", StripePaymentIntentID: "pi_1", AmountCents: 11000, Currency: "USD",
		Outcome: domain.StripeRefundNeedsReview, ReviewReason: domain.StripeRefundReviewExceedsCardRefundable, RefundCreatedAt: time.Now()}
	att := classifyInvoiceAttention(inv, attentionContext{StripeRefundReview: &r})
	if att == nil || att.Reason != domain.AttentionReasonPaymentAnomaly || att.Code != plain.Code {
		t.Fatalf("attention = %+v, want the anomaly's reason and code", att)
	}
	if !strings.HasPrefix(att.Message, "A refund of 110.00 USD was made in Stripe and not recorded here") || !strings.Contains(att.Message, "also flagged a payment") {
		t.Errorf("message = %q, want the refund first, then the anomaly", att.Message)
	}
	if !strings.Contains(att.Detail, "re_1") || !strings.Contains(att.Detail, plain.Detail) {
		t.Errorf("detail = %q, want the refund's and the anomaly's", att.Detail)
	}
	if len(att.Actions) != len(plain.Actions) {
		t.Errorf("actions = %v, want the anomaly's %v", att.Actions, plain.Actions)
	}
}
