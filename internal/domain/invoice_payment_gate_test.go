package domain

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestPaymentBlocksAction_Matrix makes the whole rule visible in one place, so a
// change to any cell is a deliberate edit rather than a side effect discovered
// later on a customer's invoice.
//
// The distinction the matrix exists to protect: PARKED and IN-FLIGHT look alike
// (both are IsInFlight) but call for opposite advice. One resolves itself and
// waiting is correct; the other never will, and telling someone to wait is the
// bug that took a four-agent sweep to find.
func TestPaymentBlocksAction_Matrix(t *testing.T) {
	settled := Invoice{PaymentStatus: PaymentPending}
	parked := Invoice{PaymentStatus: PaymentUnknown, StripePaymentIntentID: ""}
	resolving := Invoice{PaymentStatus: PaymentProcessing, StripePaymentIntentID: "pi_1"}
	unknownWithPI := Invoice{PaymentStatus: PaymentUnknown, StripePaymentIntentID: "pi_1"}

	every := []InvoiceAction{
		ActionVoid, ActionMarkUncollectible, ActionRecordOfflinePayment,
		ActionIssueCreditNote, ActionCollectPayment, ActionResendSetupLink, ActionEmailInvoice,
	}

	t.Run("a settled payment blocks nothing", func(t *testing.T) {
		for _, a := range every {
			if got := PaymentBlocksAction(settled, a); got.Blocked {
				t.Errorf("%s blocked on a not-in-flight invoice: %s", a, got.Message)
			}
		}
	})

	t.Run("parked allows ONLY mark-uncollectible", func(t *testing.T) {
		for _, a := range every {
			got := PaymentBlocksAction(parked, a)
			if a == ActionMarkUncollectible {
				if got.Blocked {
					t.Fatalf("mark-uncollectible is the ONLY exit from a parked invoice; blocking it leaves the invoice with no terminal state at all: %s", got.Message)
				}
				continue
			}
			if !got.Blocked {
				t.Errorf("%s allowed on a parked invoice — its charge may have succeeded", a)
			}
			if got.Code != "payment_unidentifiable" {
				t.Errorf("%s code = %q, want payment_unidentifiable", a, got.Code)
			}
		}
	})

	t.Run("parked refusals always name the way out", func(t *testing.T) {
		// A refusal that names no alternative is how an operator ends up stuck
		// with an invoice and no next step — which is exactly what shipped.
		for _, a := range every {
			if a == ActionMarkUncollectible {
				continue
			}
			msg := PaymentBlocksAction(parked, a).Message
			if !strings.Contains(msg, "uncollectible") {
				t.Errorf("%s refusal names no way out: %q", a, msg)
			}
			if strings.Contains(msg, "wait for") || strings.Contains(msg, "cancel it") {
				t.Errorf("%s refusal tells the operator to wait or cancel — neither is possible for a parked invoice: %q", a, msg)
			}
			// What happens next without anyone (REC-5): a payment Stripe
			// shows is recorded, and with none after 24 hours the invoice is
			// charged again. "Will not resolve on its own" is no longer true.
			if strings.Contains(msg, "will not resolve on its own") || !strings.Contains(msg, "24 hours") {
				t.Errorf("%s refusal does not say what Hardcount does next (records a payment that appears, charges again after 24 hours): %q", a, msg)
			}
		}
	})

	t.Run("a genuinely resolving charge says wait, not write off", func(t *testing.T) {
		for _, inv := range []Invoice{resolving, unknownWithPI} {
			for _, a := range every {
				got := PaymentBlocksAction(inv, a)
				if !got.Blocked {
					t.Errorf("%s allowed while a charge is being confirmed", a)
					continue
				}
				if got.Code != "payment_in_flight" {
					t.Errorf("%s code = %q, want payment_in_flight", a, got.Code)
				}
				if strings.Contains(got.Message, "uncollectible") {
					t.Errorf("%s tells the operator to write off a charge that is still resolving: %q", a, got.Message)
				}
			}
		}
	})
}

// TestNoHandWrittenPaymentInFlightRefusals is the mechanised half, and the
// reason this refactor is worth more than better wording.
//
// This rule was previously re-expressed by hand in eight places. When
// payment_status='unknown' changed meaning, all eight went wrong independently
// and two of them had already drifted apart from each other ("wait for it to
// settle or cancel it" vs "settle or cancel first, or wait for charge
// reconciliation" — same rule, two authors, both naming actions the product
// does not offer).
//
// Nothing stops that recurring except a check. Any new hand-written refusal
// that talks about a charge being in flight fails here and gets pointed at
// PaymentBlocksAction.
func TestNoHandWrittenPaymentInFlightRefusals(t *testing.T) {
	// Phrases that only ever appear in a payment-in-flight refusal.
	banned := regexp.MustCompile(`(?i)(charge is (already )?in flight|payment is in flight|wait for it to settle|charge reconciliation)`)

	// Walk the repo root, not just internal/ — see the scope note below.
	root := "../.."
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		// SCOPE IS THE GUARANTEE. This scanner was .go-only, so the dashboard —
		// named in this package's own doc comment as one of the places the rule
		// had been hand-written — was structurally invisible to it. It kept
		// hand-writing `payment_status !== 'succeeded' && !== 'processing'`,
		// omitting 'unknown', which rendered a green "Collect Payment" button on
		// a parked invoice directly beneath a Critical banner saying no further
		// charge would be attempted. A green scanner reported success the whole
		// time. Widen before trusting.
		isGo := strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go")
		isWeb := strings.HasSuffix(path, ".tsx") || strings.HasSuffix(path, ".ts")
		if !isGo && !isWeb {
			return nil
		}
		if strings.Contains(path, "node_modules") || strings.Contains(path, "/dist/") {
			return nil
		}
		// The single source is allowed to say it; that is its job.
		if strings.HasSuffix(path, "invoice_payment_gate.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		// Scope the scan to REFUSAL constructs. A banner that DESCRIBES an
		// in-flight payment ("Hardcount confirms it automatically") is legitimate
		// and true for a resolving charge — it is only the hand-written
		// refusals that must derive from the single source. The constructor and
		// its message often sit on different lines, so look ahead a little.
		refusal := regexp.MustCompile(`(errs\.InvalidState\(|respond\.Validation\(|respond\.Error\()`)
		if isWeb {
			// Frontend copy has no refusal constructor — any occurrence of the
			// banned phrasing is operator- or customer-facing text, which is
			// exactly what must not restate the rule.
			for i, line := range strings.Split(string(src), "\n") {
				if banned.MatchString(line) {
					offenders = append(offenders, path+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
				}
			}
			return nil
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			if !refusal.MatchString(line) {
				continue
			}
			window := strings.Join(lines[i:min(i+3, len(lines))], " ")
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if banned.MatchString(window) {
				offenders = append(offenders, path+":"+itoa(i+1)+"  "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("hand-written payment-in-flight refusal(s) — derive them from domain.PaymentBlocksAction instead, "+
			"so the next change to what a payment state MEANS cannot leave stale copies behind:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestEmailActionsRefuseAnUnresolvedPayment pins the customer-facing half. Both
// invoice emails carry a pay call-to-action, so sending either while a payment
// is unresolved asks a customer to pay something we may already have taken —
// and for a parked invoice the hosted page will not even offer a Pay button
// when they arrive, so the email leads them to a dead end.
func TestEmailActionsRefuseAnUnresolvedPayment(t *testing.T) {
	for _, tc := range []struct {
		name string
		inv  Invoice
	}{
		{"parked", Invoice{PaymentStatus: PaymentUnknown}},
		{"resolving", Invoice{PaymentStatus: PaymentProcessing, StripePaymentIntentID: "pi_1"}},
	} {
		for _, action := range []InvoiceAction{ActionEmailInvoice, ActionResendSetupLink} {
			got := PaymentBlocksAction(tc.inv, action)
			if !got.Blocked {
				t.Errorf("%s/%s: allowed — this email asks the customer to pay an invoice whose payment is unresolved", tc.name, action)
			}
		}
	}

	// Negative control: a normal unpaid invoice must still be emailable, or
	// this guard would break ordinary collections.
	for _, action := range []InvoiceAction{ActionEmailInvoice, ActionResendSetupLink} {
		if PaymentBlocksAction(Invoice{PaymentStatus: PaymentPending}, action).Blocked {
			t.Errorf("%s blocked on an ordinary unpaid invoice — that is the normal collections path", action)
		}
	}
}

// TestPaymentBlocksActionHeld_Matrix (#975, ADR-127): a live hold refuses all
// three actions; a stale one (a charge whose result was lost) refuses void
// and offline payment but allows write-off, which moves no money. A parked
// invoice consults the hold only while it is live (ADR-127 amendment
// 2026-10-29): a request for it may still reach Stripe, so write-off waits;
// with a stale hold, its normal waiting state, write-off is the exit
// (ADR-107). 'processing' and 'unknown' with a PaymentIntent keep their own
// refusal.
func TestPaymentBlocksActionHeld_Matrix(t *testing.T) {
	actions := []InvoiceAction{ActionVoid, ActionMarkUncollectible, ActionRecordOfflinePayment}
	cases := []struct {
		name     string
		ps       InvoicePaymentStatus
		pi       string
		hold     ChargeHoldState
		action   InvoiceAction
		wantCode string // "" = allowed
	}{
		{"pending live", PaymentPending, "", ChargeHoldLive, "", "payment_in_flight"},
		{"failed live", PaymentFailed, "pi_1", ChargeHoldLive, "", "payment_in_flight"},
		{"pending stale, void", PaymentPending, "", ChargeHoldStale, ActionVoid, "payment_in_flight"},
		{"pending stale, offline", PaymentPending, "", ChargeHoldStale, ActionRecordOfflinePayment, "payment_in_flight"},
		{"pending stale, write-off", PaymentPending, "", ChargeHoldStale, ActionMarkUncollectible, ""},
		{"failed stale, void", PaymentFailed, "pi_1", ChargeHoldStale, ActionVoid, "payment_in_flight"},
		{"failed stale, write-off", PaymentFailed, "pi_1", ChargeHoldStale, ActionMarkUncollectible, ""},
		{"pending free", PaymentPending, "", ChargeHoldNone, "", ""},
		{"failed free", PaymentFailed, "pi_1", ChargeHoldNone, "", ""},
		{"processing free", PaymentProcessing, "pi_1", ChargeHoldNone, "", "payment_in_flight"},
		{"parked live, write-off", PaymentUnknown, "", ChargeHoldLive, ActionMarkUncollectible, "payment_in_flight"},
		{"parked stale, write-off", PaymentUnknown, "", ChargeHoldStale, ActionMarkUncollectible, ""},
		{"parked no hold, write-off", PaymentUnknown, "", ChargeHoldNone, ActionMarkUncollectible, ""},
		{"unknown naming its payment, live", PaymentUnknown, "pi_1", ChargeHoldLive, ActionMarkUncollectible, "payment_in_flight"},
		{"parked live, void", PaymentUnknown, "", ChargeHoldLive, ActionVoid, "payment_unidentifiable"},
		{"parked live, offline", PaymentUnknown, "", ChargeHoldLive, ActionRecordOfflinePayment, "payment_unidentifiable"},
	}
	for _, c := range cases {
		for _, a := range actions {
			if c.action != "" && c.action != a {
				continue
			}
			b := PaymentBlocksActionHeld(Invoice{Status: InvoiceFinalized, PaymentStatus: c.ps, StripePaymentIntentID: c.pi}, a, c.hold)
			if b.Code != c.wantCode || b.Blocked != (c.wantCode != "") {
				t.Errorf("%s / %s: got blocked=%v code=%q, want code %q", c.name, a, b.Blocked, b.Code, c.wantCode)
			}
			if b.Blocked && b.Message == "" {
				t.Errorf("%s / %s: a refusal must say what to do", c.name, a)
			}
		}
	}
}

// TestChargeHoldTiming pins the numbers that make the stale-hold sweep's
// "Stripe has nothing" final (ADR-127): the charge sends only with at least
// ChargeSendMargin left on its hold and bounds its Stripe call by
// ChargeSendBudget, so no request for an attempt can start after its hold
// ends (TTL - margin + budget <= TTL); and the sweep waits ten more minutes
// for Stripe to finish anything it received.
func TestChargeHoldTiming(t *testing.T) {
	if ChargeHoldTTL-ChargeSendMargin+ChargeSendBudget > ChargeHoldTTL {
		t.Errorf("a charge sent at the last allowed moment could still be running at Stripe after its hold ends: TTL %v, margin %v, budget %v",
			ChargeHoldTTL, ChargeSendMargin, ChargeSendBudget)
	}
	if ChargeSendMargin >= ChargeHoldTTL {
		t.Errorf("margin %v >= TTL %v: no charge could ever send", ChargeSendMargin, ChargeHoldTTL)
	}
	if StaleChargeHoldWait != 10*time.Minute {
		t.Errorf("StaleChargeHoldWait = %v, want 10m (the design's wait after a hold ends)", StaleChargeHoldWait)
	}
}

// TestChargeHoldOwner: only the exact hold on the exact invoice is owned.
func TestChargeHoldOwner(t *testing.T) {
	until := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	ctx := WithChargeHoldOwner(context.Background(), "inv_1", ChargeHold{Until: until})
	if !OwnsChargeHold(ctx, "inv_1", until) {
		t.Error("the owner of a hold does not own it")
	}
	if OwnsChargeHold(ctx, "inv_2", until) {
		t.Error("a hold on one invoice is owned on another")
	}
	if OwnsChargeHold(ctx, "inv_1", until.Add(time.Microsecond)) {
		t.Error("another hold value on the same invoice is owned: a later owner's hold must not pass")
	}
	if OwnsChargeHold(WithChargeHoldOwner(context.Background(), "inv_1", ChargeHold{}), "inv_1", until) {
		t.Error("a refused claim (zero hold) owns something")
	}
}
