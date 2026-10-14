package domain

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// The attention notice's shape (operator-messages work, 2026-10-15): Message
// is one plain sentence of 25 words or fewer; a notice that must show a
// second fact up front adds it as a second line after "\n"; the mechanism and
// longer instructions go in Detail. These tests pin every branch's wording to
// the real signal it depends on (collection paused, customer email, email
// sending, simulated time), so a branch that stops branching fails here.

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

func actionCodes(att *Attention) []AttentionAction {
	out := make([]AttentionAction, 0, len(att.Actions))
	for _, a := range att.Actions {
		out = append(out, a.Code)
	}
	return out
}

func sameActions(got []AttentionAction, want ...AttentionAction) bool {
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

func noPMInvoice(simulated bool) Invoice {
	return Invoice{
		ID: "vlx_inv_copy", Status: InvoiceFinalized, PaymentStatus: PaymentPending,
		AutoChargePending: true, IsSimulated: simulated, UpdatedAt: time.Now(),
		TaxFacts: TaxFacts{TaxStatus: InvoiceTaxOK},
	}
}

// TestNoPaymentMethodCopy_EveryBranch: line 1 branches on paused, then on
// simulated; line 2 branches on customer email, then on email sending; the
// Resend action exists only when an email can actually be sent.
func TestNoPaymentMethodCopy_EveryBranch(t *testing.T) {
	const (
		wall   = "This invoice is charged automatically once the customer adds a card."
		sim    = "This invoice is charged on the next clock advance once the customer adds a card."
		paused = "Collection is paused on the subscription, so adding a card won't charge this invoice until you resume it."

		sent    = "A setup link is emailed to the customer once; use Resend setup link to send it again."
		noEmail = "Setup links are emailed only to customers with an email address, so copy one from the customer page and send it."
		off     = "This server can't email setup links, so none was sent; copy one from the customer page instead."
	)
	cases := []struct {
		name               string
		simulated, pause   bool
		hasEmail, emailOff bool
		line1, line2       string
		resend             bool
	}{
		{"wall, email", false, false, true, false, wall, sent, true},
		{"wall, no email", false, false, false, false, wall, noEmail, false},
		{"wall, email off", false, false, true, true, wall, off, false},
		{"sim, email", true, false, true, false, sim, sent, true},
		{"sim, no email", true, false, false, false, sim, noEmail, false},
		{"sim, email off", true, false, true, true, sim, off, false},
		{"paused, email", false, true, true, false, paused, sent, true},
		{"paused sim, no email", true, true, false, false, paused, noEmail, false},
		{"paused, email off", false, true, true, true, paused, off, false},
		// No email AND email off: the missing address is the first thing to fix.
		{"no email and email off", false, false, false, true, wall, noEmail, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := ClassifyInvoiceAttention(noPMInvoice(tc.simulated), AttentionContext{
				CustomerHasEmail: tc.hasEmail, SetupLinkEmailOff: tc.emailOff, CollectionPaused: tc.pause,
			})
			if att == nil || att.Reason != AttentionReasonNoPaymentMethod {
				t.Fatalf("got %+v, want no_payment_method", att)
			}
			if want := tc.line1 + "\n" + tc.line2; att.Message != want {
				t.Errorf("message =\n%q\nwant\n%q", att.Message, want)
			}
			checkShape(t, att.Message)
			if att.Detail == "" {
				t.Error("detail is empty; the email explanation belongs there")
			}
			if att.Severity != AttentionSeverityWarning {
				t.Errorf("severity = %s, want warning", att.Severity)
			}
			want := []AttentionAction{AttentionActionAddPaymentMethod}
			if tc.resend {
				want = []AttentionAction{AttentionActionSendReminder, AttentionActionAddPaymentMethod}
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
		simulated, pause   bool
		hasEmail, emailOff bool
		msg                string
		resend             bool
	}{
		{"paused", false, true, true, false,
			"Retries stopped and collection is paused; ask the customer to add a card, then resume collection.", true},
		{"paused sim, no email", true, true, false, false,
			"Retries stopped and collection is paused; ask the customer to add a card, then resume collection.\n" + noEmailLine, false},
		{"not paused", false, false, true, false,
			"Retries stopped; this invoice is charged automatically once the customer adds a card.", true},
		{"not paused sim", true, false, true, false,
			"Retries stopped; this invoice is charged on the next clock advance once the customer adds a card.", true},
		{"not paused, email off", false, false, true, true,
			"Retries stopped; this invoice is charged automatically once the customer adds a card.\n" + emailOffLine, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := ClassifyInvoiceAttention(noPMInvoice(tc.simulated), AttentionContext{
				DunningEscalated: true, CollectionPaused: tc.pause,
				CustomerHasEmail: tc.hasEmail, SetupLinkEmailOff: tc.emailOff,
			})
			if att == nil || att.Reason != AttentionReasonDunningExhausted {
				t.Fatalf("got %+v, want dunning_exhausted", att)
			}
			if att.Message != tc.msg {
				t.Errorf("message = %q, want %q", att.Message, tc.msg)
			}
			checkShape(t, att.Message)
			if !strings.Contains(att.Detail, "ended without collecting") {
				t.Errorf("detail = %q, want the recovery-ended explanation", att.Detail)
			}
			want := []AttentionAction{AttentionActionAddPaymentMethod}
			if tc.resend {
				want = []AttentionAction{AttentionActionSendReminder, AttentionActionAddPaymentMethod}
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
	withCard := AttentionContext{HasPaymentMethod: true, Now: now}
	queued := func(sim bool) Invoice {
		inv := noPMInvoice(sim)
		inv.UpdatedAt = now
		return inv
	}
	cases := []struct {
		name    string
		inv     Invoice
		atc     AttentionContext
		reason  AttentionReason
		sev     AttentionSeverity
		msg     string
		detail  string // substring; "" means Detail must be empty
		actions []AttentionAction
	}{
		{"scheduled wall", queued(false), withCard, AttentionReasonPaymentScheduled, AttentionSeverityInfo,
			"The card on file is charged automatically, usually within an hour; use Charge now to collect it right away.",
			"hourly by default", []AttentionAction{AttentionActionChargeNow}},
		{"scheduled sim", queued(true), withCard, AttentionReasonPaymentScheduled, AttentionSeverityInfo,
			"The card on file is charged on the next clock advance; use Charge now to collect it right away.",
			"only when the test clock is advanced", []AttentionAction{AttentionActionChargeNow}},
		{"collection paused, card on file", queued(false), AttentionContext{HasPaymentMethod: true, CollectionPaused: true},
			AttentionReasonCollectionPaused, AttentionSeverityInfo,
			"Collection is paused on the subscription, so this invoice won't be charged automatically; use Charge now to collect it anyway.",
			"Resume collection", []AttentionAction{AttentionActionChargeNow}},
		{"awaiting (pre-ADR-116 row)", func() Invoice { i := queued(false); i.AutoChargePending = false; return i }(), withCard,
			AttentionReasonAwaitingPayment, AttentionSeverityInfo,
			"This invoice hasn't been charged yet; use Charge now, or email the customer a payment link.",
			"", []AttentionAction{AttentionActionChargeNow, AttentionActionSendReminder}},
		{"payment page open", queued(false), AttentionContext{HasPaymentMethod: true, PaymentPageOpen: true},
			AttentionReasonPaymentProcessing, AttentionSeverityInfo,
			"The customer has the payment page open or just paid on it, so Velox holds charges until that finishes, usually within an hour.",
			"", nil},
		{"processing fresh", func() Invoice { i := queued(false); i.PaymentStatus = PaymentProcessing; return i }(), withCard,
			AttentionReasonPaymentProcessing, AttentionSeverityInfo,
			"Stripe is confirming this payment, and this page updates automatically when it's done.",
			"", nil},
		{"processing stale", func() Invoice {
			i := queued(false)
			i.PaymentStatus = PaymentProcessing
			i.UpdatedAt = now.Add(-7 * time.Hour)
			return i
		}(), withCard, AttentionReasonPaymentProcessing, AttentionSeverityWarning,
			"Stripe still hasn't confirmed this payment after 6 hours, so check it in Stripe.",
			"usually confirmed within seconds", nil},
		{"unconfirmed with a payment id", func() Invoice {
			i := queued(false)
			i.PaymentStatus = PaymentUnknown
			i.StripePaymentIntentID = "pi_x"
			return i
		}(), withCard, AttentionReasonPaymentUnconfirmed, AttentionSeverityInfo,
			"Stripe hasn't confirmed this charge yet, and Velox re-checks automatically until it does.",
			"3-D Secure", nil},
		{"parked (no payment id)", func() Invoice { i := queued(false); i.PaymentStatus = PaymentUnknown; return i }(), withCard,
			AttentionReasonPaymentUnconfirmed, AttentionSeverityCritical,
			"Velox can't tell if this charge went through, so it won't charge again; check Stripe, and if nothing was taken, mark it uncollectible.",
			"will not resolve on its own", []AttentionAction{AttentionActionMarkUncollectible}},
		{"declined", func() Invoice {
			i := queued(false)
			i.PaymentStatus = PaymentFailed
			i.LastPaymentError = "Your card was declined."
			return i
		}(), withCard,
			AttentionReasonPaymentFailed, AttentionSeverityCritical,
			"The customer's card was declined.",
			"", []AttentionAction{AttentionActionUpdatePaymentMethod, AttentionActionRetryPayment}},
		{"recovery charge in flight on a written-off invoice", func() Invoice {
			i := queued(false)
			i.Status = InvoiceUncollectible
			i.PaymentStatus = PaymentProcessing
			return i
		}(), withCard, AttentionReasonPaymentProcessing, AttentionSeverityInfo,
			"Stripe is still confirming a payment on this written-off invoice, so don't charge it again.",
			"stays written off", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := ClassifyInvoiceAttention(tc.inv, tc.atc)
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
		EventPaymentDuplicateCharge:         "A second payment succeeded after this invoice was already paid, so refund the extra charge in Stripe.",
		EventPaymentAmountMismatch:          "Stripe captured 96.52 USD but this invoice recorded 100.00 USD as paid, so reconcile the difference.",
		EventPaymentReceivedOnVoidedInvoice: "A payment succeeded on this voided invoice, so the customer paid money they don't owe; refund it in Stripe.",
		EventPaymentCurrencyMismatch:        "A payment was charged in a different currency than this invoice, so review it in Stripe and refund or correct it.",
		"payment.something_new":             "Velox found a payment on this invoice that doesn't match its records, so review it in Stripe.",
	}
	for kind, want := range kinds {
		t.Run(kind, func(t *testing.T) {
			inv := Invoice{
				Status: InvoicePaid, PaymentStatus: PaymentSucceeded, Currency: "USD",
				AmountPaidCents: 10000, PaymentAnomalyKind: kind,
				PaymentAnomalyPaymentIntentID: "pi_extra", PaymentAnomalyCapturedCents: 9652,
			}
			att := ClassifyInvoiceAttention(inv, AttentionContext{})
			if att == nil || att.Reason != AttentionReasonPaymentAnomaly {
				t.Fatalf("got %+v", att)
			}
			if att.Message != want {
				t.Errorf("message = %q, want %q", att.Message, want)
			}
			checkShape(t, att.Message)
			if !strings.Contains(att.Detail, "pi_extra") {
				t.Errorf("detail = %q, want the payment id", att.Detail)
			}
			if got := actionCodes(att); !sameActions(got, AttentionActionReconcilePayment) {
				t.Errorf("actions = %v, want [reconcile_payment]", got)
			}
			// No id: nothing to open, so no button.
			inv.PaymentAnomalyPaymentIntentID = ""
			if att := ClassifyInvoiceAttention(inv, AttentionContext{}); len(att.Actions) != 0 {
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
				inv := Invoice{Status: InvoiceFinalized, TaxFacts: TaxFacts{TaxStatus: InvoiceTaxPending, TaxErrorCode: code}}
				inv.TaxRetryCount = 3
				if exhausted {
					inv.TaxRetryCount = MaxTaxRetryAttempts
				}
				att := ClassifyInvoiceAttention(inv, AttentionContext{StripeConnected: connected})
				if att == nil {
					t.Fatalf("%s: no attention", code)
				}
				checkShape(t, att.Message)
				retryable := TaxErrorCodeRetryable(code) && code != "provider_not_configured"
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

// TestCommitExposureCopy: the money line keeps exact amounts and stays in
// Message (never only in Details); standalone it is one sentence, folded it
// is a labelled second line.
func TestCommitExposureCopy(t *testing.T) {
	cases := []struct {
		name            string
		drawable, spent int64
		standalone      string
	}{
		{"unspent", 10000000, 0,
			"The customer can already spend 100,000.00 USD from this invoice, and voiding it cancels that credit."},
		{"partly spent", 5620000, 4380000,
			"The customer can still spend 56,200.00 USD (43,800.00 USD already spent), and voiding cancels the unspent 56,200.00 USD."},
		{"fully spent", 0, 10000000,
			"The customer already spent all 100,000.00 USD this invoice bought, so voiding cannot recover it."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := commitInv()
			inv.Status = InvoiceUncollectible // chain returns nil → standalone
			att := ClassifyInvoiceAttention(inv, liveGrant(tc.drawable, tc.spent))
			if att == nil || att.Message != tc.standalone {
				t.Fatalf("standalone message = %q, want %q", att.Message, tc.standalone)
			}
			checkShape(t, att.Message)
			if att.Detail == "" {
				t.Error("standalone detail is empty")
			}

			inv.Status = InvoiceFinalized
			inv.PaymentStatus = PaymentFailed // cause wins, exposure folds
			inv.LastPaymentError = "Your card was declined."
			folded := ClassifyInvoiceAttention(inv, liveGrant(tc.drawable, tc.spent))
			want := "The customer's card was declined.\nUnpaid prepaid credit: t" + strings.TrimPrefix(tc.standalone, "T")
			if folded.Message != want {
				t.Errorf("folded message =\n%q\nwant\n%q", folded.Message, want)
			}
			checkShape(t, folded.Message)
		})
	}
}

// TestCommitExposureCopy_ThreeLinesOnNoPaymentMethod: the longest message the
// API returns. A no-card commit invoice carries the card line, the setup-link
// email line and the prepaid-credit line, each one sentence, in that order.
// The public docs promise "up to three lines"; this pins that bound.
func TestCommitExposureCopy_ThreeLinesOnNoPaymentMethod(t *testing.T) {
	inv := noPMInvoice(false)
	inv.Currency = "USD"
	atc := liveGrant(10000000, 0)
	atc.CustomerHasEmail = true

	att := ClassifyInvoiceAttention(inv, atc)
	if att == nil || att.Reason != AttentionReasonNoPaymentMethod {
		t.Fatalf("got %+v, want no_payment_method (the cause keeps its reason)", att)
	}
	want := []string{
		"This invoice is charged automatically once the customer adds a card.",
		"A setup link is emailed to the customer once; use Resend setup link to send it again.",
		"Unpaid prepaid credit: the customer can already spend 100,000.00 USD from this invoice, and voiding it cancels that credit.",
	}
	got := strings.Split(att.Message, "\n")
	if len(got) != len(want) {
		t.Fatalf("message has %d lines, want %d:\n%q", len(got), len(want), att.Message)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i+1, got[i], want[i])
		}
	}
	checkShape(t, att.Message)
	if codes := actionCodes(att); !sameActions(codes,
		AttentionActionSendReminder, AttentionActionAddPaymentMethod, AttentionActionVoidInvoice) {
		t.Errorf("actions = %v, want Resend, Open customer page, Void", codes)
	}

	// Recovery ended with no email: also three lines, the last one still money.
	atc.CustomerHasEmail = false
	atc.DunningEscalated = true
	att = ClassifyInvoiceAttention(inv, atc)
	if att == nil || att.Reason != AttentionReasonDunningExhausted {
		t.Fatalf("got %+v, want dunning_exhausted", att)
	}
	lines := strings.Split(att.Message, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[2], "Unpaid prepaid credit: ") {
		t.Errorf("escalated message = %q, want three lines ending with the prepaid credit", att.Message)
	}
	checkShape(t, att.Message)
}
