package invoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// The activity line standard (ADR-128) on the invoice timeline's wire: every
// row carries a `line`, and the legacy description / detail / error / status
// fields are filled from it, so today's Activity card already shows the
// corrected words and dot colours. Each case below is one of the standard's
// fixes, asserted on the JSON an operator's browser receives.

// auditRowsFake returns canned audit rows per resource ("invoice:<id>",
// "credit_note:<id>", "dunning_run:<id>").
type auditRowsFake struct {
	rows map[string][]domain.AuditEntry
}

func (f *auditRowsFake) ListByInvoice(_ context.Context, _, invoiceID string) ([]domain.AuditEntry, error) {
	return f.rows["invoice:"+invoiceID], nil
}

func (f *auditRowsFake) ListByResource(_ context.Context, _, resourceType, resourceID string) ([]domain.AuditEntry, error) {
	return f.rows[resourceType+":"+resourceID], nil
}

// attemptsStore is the in-memory store with canned charge attempts.
type attemptsStore struct {
	*memStore
	attempts []domain.InvoiceChargeAttempt
}

func (s *attemptsStore) ListChargeAttemptsByInvoice(context.Context, string, string) ([]domain.InvoiceChargeAttempt, error) {
	return s.attempts, nil
}

type disputesFake struct{ disputes []domain.InvoiceDispute }

func (f *disputesFake) ListDisputesByInvoice(context.Context, string, string) ([]domain.InvoiceDispute, error) {
	return f.disputes, nil
}

func (f *disputesFake) LostDisputesByInvoices(context.Context, string, []string) (map[string]domain.InvoiceDispute, error) {
	return nil, nil
}

// wireCase is one invoice and the side lanes around it.
type wireCase struct {
	inv      domain.Invoice
	attempts []domain.InvoiceChargeAttempt
	runs     []domain.InvoiceDunningRun
	events   []domain.InvoiceDunningEvent
	cns      []domain.CreditNote
	emails   []EmailEventRow
	audit    map[string][]domain.AuditEntry
	disputes []domain.InvoiceDispute
}

// fetchWire seeds the case and returns the timeline's events as JSON maps.
func fetchWire(t *testing.T, c wireCase) []map[string]any {
	t.Helper()
	h, invoiceID := wireHandler(t, c)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", invoiceID)
	ctx := auth.WithTenantID(req.Context(), "t1")
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	rr := httptest.NewRecorder()
	h.paymentTimeline(rr, req.WithContext(ctx))
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
	}
	var resp timelineResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Events
}

// wireHandler seeds the case and returns a handler over it and the seeded
// invoice's id.
func wireHandler(t *testing.T, c wireCase) (*Handler, string) {
	t.Helper()
	mem := newMemStore()
	inv := c.inv
	if inv.CustomerID == "" {
		inv.CustomerID = "cus_1"
	}
	if inv.Currency == "" {
		inv.Currency = "USD"
	}
	created, err := mem.Create(context.Background(), "t1", inv)
	if err != nil {
		t.Fatalf("seed invoice: %v", err)
	}
	// Create may not keep terminal fields; put the seeded row back whole.
	inv.ID, inv.TenantID, inv.CreatedAt, inv.UpdatedAt = created.ID, created.TenantID, created.CreatedAt, created.UpdatedAt
	if !c.inv.CreatedAt.IsZero() {
		inv.CreatedAt = c.inv.CreatedAt
	}
	mem.invoices[created.ID] = inv
	svc := NewService(&attemptsStore{memStore: mem, attempts: c.attempts}, nil, nil)
	if c.disputes != nil {
		svc.SetDisputeReader(&disputesFake{disputes: c.disputes})
	}
	h := &Handler{svc: svc}
	if c.runs != nil {
		h.dunningTimeline = &dunningEventsLaneFake{runs: c.runs, events: c.events}
	}
	if c.cns != nil {
		for i := range c.cns {
			c.cns[i].InvoiceID = created.ID
		}
		h.creditNotes = &cnLaneFake{cns: c.cns}
	}
	if c.emails != nil {
		h.emailEvents = &emailRowsFake{rows: c.emails}
	}
	audit := map[string][]domain.AuditEntry{}
	for k, v := range c.audit {
		audit[strings.ReplaceAll(k, "{inv}", created.ID)] = v
	}
	h.auditStamps = &auditRowsFake{rows: audit}
	return h, created.ID
}

// wireRow is the part of one row the cases assert on.
type wireRow struct {
	EventType, Description, Status, Detail, Error string
	Amount                                        int64
	Title, Tone, Problem                          string
}

func toWireRow(e map[string]any) wireRow {
	s := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	r := wireRow{
		EventType: s(e, "event_type"), Description: s(e, "description"), Status: s(e, "status"),
		Detail: s(e, "detail"), Error: s(e, "error"),
	}
	if a, ok := e["amount_cents"].(float64); ok {
		r.Amount = int64(a)
	}
	if line, ok := e["line"].(map[string]any); ok {
		r.Title, r.Tone, r.Problem = s(line, "title"), s(line, "tone"), s(line, "problem")
	}
	return r
}

func rowsOf(events []map[string]any, eventType string) []wireRow {
	var out []wireRow
	for _, e := range events {
		if r := toWireRow(e); r.EventType == eventType {
			out = append(out, r)
		}
	}
	return out
}

func oneRow(t *testing.T, events []map[string]any, eventType string) wireRow {
	t.Helper()
	rows := rowsOf(events, eventType)
	if len(rows) != 1 {
		var all []string
		for _, e := range events {
			r := toWireRow(e)
			all = append(all, r.EventType+": "+r.Description)
		}
		t.Fatalf("%d %q rows, want 1; rows: %v", len(rows), eventType, all)
	}
	return rows[0]
}

// wantRow checks the legacy fields and the line together: the legacy title
// is the line's title, the legacy status is the line's tone in the words the
// dot map reads, and the legacy error is the line's problem.
func wantRow(t *testing.T, got wireRow, title, tone, problem, detail string) {
	t.Helper()
	status := map[string]string{"neutral": "neutral", "success": "succeeded", "warning": "warning", "failure": "failed"}[tone]
	if got.Description != title || got.Title != title {
		t.Errorf("title: description %q, line.title %q; want %q", got.Description, got.Title, title)
	}
	if got.Tone != tone || got.Status != status {
		t.Errorf("%s: line.tone %q, status %q; want %q / %q", title, got.Tone, got.Status, tone, status)
	}
	if got.Problem != problem || got.Error != problem {
		t.Errorf("%s: line.problem %q, error %q; want %q", title, got.Problem, got.Error, problem)
	}
	if detail != "*" && got.Detail != detail {
		t.Errorf("%s: detail %q, want %q", title, got.Detail, detail)
	}
}

func TestActivityLines_InvoiceWire(t *testing.T) {
	t.Parallel()
	at := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	finalized := domain.Invoice{Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentFailed, AmountDueCents: 4900, TotalAmountCents: 4900, IssuedAt: &at}

	// B1: the renderer switched on policy tokens the writer stopped storing at
	// ADR-112, so every escalation read "Escalated for manual review".
	t.Run("B1 retries ran out names what exhaustion did", func(t *testing.T) {
		for _, tc := range []struct {
			name, reason string
			writtenOff   bool
			problem      string
		}{
			{"pause", "subscription_paused", false, "Collection paused"},
			{"cancel", "subscription_canceled", false, "Subscription canceled"},
			{"write-off alone: the uncollectible row beside it says so", "invoice_marked_uncollectible", true, ""},
			{"cancel and write-off", "subscription_canceled+invoice_marked_uncollectible", true, "Subscription canceled"},
			{"pause and write-off", "subscription_paused+invoice_marked_uncollectible", true, "Collection paused"},
			{"nothing changed", "", false, "Nothing else changes; the invoice stays open."},
		} {
			t.Run(tc.name, func(t *testing.T) {
				inv := finalized
				if tc.writtenOff {
					inv.Status, inv.UncollectibleAt = domain.InvoiceUncollectible, &at
				}
				events := fetchWire(t, wireCase{
					inv:    inv,
					runs:   []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningEscalated, Resolution: domain.ResolutionRetriesExhausted}},
					events: []domain.InvoiceDunningEvent{{ID: "e1", EventType: domain.DunningEventEscalated, AttemptCount: 3, Reason: tc.reason, CreatedAt: at}},
				})
				wantRow(t, oneRow(t, events, "escalated"), "Payment retries ran out", "warning", tc.problem, "Automatic")
			})
		}
	})

	// B3: a declined retry had status processing (a blue dot) and the UI hid
	// its decline reason, which it shows only for failed rows.
	t.Run("B3 a declined retry is a failure and shows its decline", func(t *testing.T) {
		events := fetchWire(t, wireCase{
			inv:  finalized,
			runs: []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningActive}},
			events: []domain.InvoiceDunningEvent{{
				ID: "e2", EventType: domain.DunningEventRetryAttempted, AttemptCount: 2, CreatedAt: at,
				Reason: "payment failed: Your card has insufficient funds.", Metadata: map[string]any{"payment_intent_id": "pi_2"},
			}},
			attempts: []domain.InvoiceChargeAttempt{{
				ID: "a2", StripePaymentIntentID: "pi_2", Trigger: domain.ChargeTriggerDunningRetry, Outcome: domain.ChargeAttemptFailed,
				ProviderReason: "Your card has insufficient funds.", DeclineCode: "insufficient_funds", AmountCents: 4900, OccurredAt: at,
			}},
		})
		row := oneRow(t, events, "retry_attempted")
		wantRow(t, row, "Payment retry 2 failed", "failure", "Card declined: insufficient funds", "Automatic")
		if row.Amount != 4900 {
			t.Errorf("retry amount %d, want 4900 (the charge it tried)", row.Amount)
		}
		if n := len(rowsOf(events, "charge_attempt.failed")); n != 0 {
			t.Errorf("%d separate attempt rows, want 0: the retry row tells that charge", n)
		}
	})

	t.Run("a retry with no attempt row keeps the provider's words without Velox's wrapper", func(t *testing.T) {
		events := fetchWire(t, wireCase{
			inv:    finalized,
			runs:   []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningActive}},
			events: []domain.InvoiceDunningEvent{{ID: "e1", EventType: domain.DunningEventRetryAttempted, AttemptCount: 1, CreatedAt: at, Reason: "payment failed: Your card was declined."}},
		})
		wantRow(t, oneRow(t, events, "retry_attempted"), "Payment retry 1 failed", "failure", "Your card was declined.", "Automatic")
	})

	t.Run("retries that charged nothing read not attempted", func(t *testing.T) {
		for _, tc := range []struct{ reason, problem string }{
			{string(domain.DunningCauseNoPaymentMethod), "No payment method on file"},
			{domain.ErrNoPaymentMethodOnRetry.Error(), "No payment method on file"},
			{domain.DunningRetryReasonCardRefused, "The bank refused this card for good. The customer needs to add a different card."},
		} {
			events := fetchWire(t, wireCase{
				inv:    finalized,
				runs:   []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningActive}},
				events: []domain.InvoiceDunningEvent{{ID: "e1", EventType: domain.DunningEventRetryAttempted, AttemptCount: 1, CreatedAt: at, Reason: tc.reason}},
			})
			wantRow(t, oneRow(t, events, "retry_attempted"), "Payment retry 1 not attempted", "warning", tc.problem, "Automatic")
		}
	})

	t.Run("payment recovery started", func(t *testing.T) {
		for _, tc := range []struct{ cause, tone, problem string }{
			{string(domain.DunningCausePaymentFailed), "neutral", ""},
			{string(domain.DunningCauseNoPaymentMethod), "warning", "No payment method on file. Reminders go out until the customer adds one."},
		} {
			events := fetchWire(t, wireCase{
				inv:    finalized,
				runs:   []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningActive}},
				events: []domain.InvoiceDunningEvent{{ID: "e0", EventType: domain.DunningEventStarted, CreatedAt: at, Reason: tc.cause}},
			})
			wantRow(t, oneRow(t, events, "dunning_started"), "Payment recovery started", tc.tone, tc.problem, "Automatic")
		}
	})

	// B16: a run closed with the legacy manually_resolved read "Invoice
	// voided"; the one kind of row that still holds it is a run whose invoice
	// never changed state.
	t.Run("B16 a run closed with the legacy value reads Closed unpaid", func(t *testing.T) {
		events := fetchWire(t, wireCase{
			inv:    finalized,
			runs:   []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningResolved, Resolution: domain.ResolutionManuallyResolved}},
			events: []domain.InvoiceDunningEvent{{ID: "e9", EventType: domain.DunningEventResolved, CreatedAt: at, Reason: "manually_resolved"}},
		})
		wantRow(t, oneRow(t, events, "resolved"), "Payment recovery ended", "neutral", "", "Closed unpaid")
	})

	// B8: the timeline showed raw reason codes; the credit note total is the
	// one amount, and the parts say where it went.
	t.Run("B8 one credit-note title, its reason in plain words, its parts", func(t *testing.T) {
		events := fetchWire(t, wireCase{
			inv: domain.Invoice{Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, AmountPaidCents: 4900, PaidAt: &at, StripePaymentIntentID: "pi_card"},
			cns: []domain.CreditNote{{
				ID: "cn_1", CreditNoteNumber: "CN-000012", Status: domain.CreditNoteIssued, IssuedAt: &at, Currency: "USD",
				Reason: "subscription_downgrade", TotalCents: 2300, RefundAmountCents: 1500, CreditAmountCents: 500, OutOfBandAmountCents: 300,
				RefundStatus: domain.RefundSucceeded,
			}},
			audit: map[string][]domain.AuditEntry{"credit_note:cn_1": {{Action: "credit_note.issued", ActorType: "user", ActorName: "alice@example.com", CreatedAt: at}}},
		})
		row := oneRow(t, events, "credit_note.issued")
		wantRow(t, row, "Credit note issued", "success", "",
			"CN-000012 · Plan downgrade · $15.00 refunded to card · $5.00 to credit balance · $3.00 refunded outside Velox · by alice@example.com")
		if row.Amount != 2300 {
			t.Errorf("credit note amount %d, want the total 2300", row.Amount)
		}
	})

	t.Run("credit note reasons and refund states", func(t *testing.T) {
		for _, tc := range []struct {
			name         string
			reason       string
			status       domain.RefundStatus
			audit        []domain.AuditEntry
			tone, prob   string
			detailPrefix string
		}{
			{"commit relief", "commit_relief", domain.RefundSucceeded, nil, "success", "", "CN-1 · Unused prepaid credits refunded"},
			{"operator text kept", "Billing error", domain.RefundSucceeded, nil, "success", "", "CN-1 · Billing error"},
			{"failed, with the provider's reason", "Billing error", domain.RefundFailed, []domain.AuditEntry{
				{Action: domain.AuditActionUpdate, Metadata: map[string]any{"action": "refund_status_changed", "refund_status": "failed", "provider_failure_reason": "expired_or_canceled_card"}, CreatedAt: at.Add(time.Minute)},
				{Action: "credit_note.issued", ActorType: "system", CreatedAt: at},
			}, "failure", "Card refund failed: expired or canceled card", "CN-1 · Billing error"},
			{"failed, no reason recorded", "Billing error", domain.RefundFailed, nil, "failure", "Card refund failed.", "CN-1"},
			{"pending", "Billing error", domain.RefundPending, nil, "warning", "The card refund is waiting on the payment provider.", "CN-1"},
			{"unknown", "Billing error", domain.RefundUnknown, nil, "warning", "The payment provider did not say whether the card refund went through.", "CN-1"},
			{"never ran", "Billing error", domain.RefundNone, nil, "warning", "No refund was sent to the payment provider.", "CN-1"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := wireCase{
					inv: domain.Invoice{Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, AmountPaidCents: 1000, PaidAt: &at},
					cns: []domain.CreditNote{{
						ID: "cn_1", CreditNoteNumber: "CN-1", Status: domain.CreditNoteIssued, IssuedAt: &at, Currency: "USD",
						Reason: tc.reason, TotalCents: 1000, RefundAmountCents: 1000, RefundStatus: tc.status,
					}},
				}
				if tc.audit != nil {
					c.audit = map[string][]domain.AuditEntry{"credit_note:cn_1": tc.audit}
				}
				row := oneRow(t, fetchWire(t, c), "credit_note.issued")
				wantRow(t, row, "Credit note issued", tc.tone, tc.prob, "*")
				if !strings.HasPrefix(row.Detail, tc.detailPrefix) || strings.Contains(row.Detail, tc.reason) && tc.reason == "commit_relief" {
					t.Errorf("detail %q, want it to start %q with no raw code", row.Detail, tc.detailPrefix)
				}
			})
		}
	})

	t.Run("email words: the email's name and what happened to it", func(t *testing.T) {
		events := fetchWire(t, wireCase{
			inv: finalized,
			emails: []EmailEventRow{
				{ID: "m1", EmailType: "invoice", Status: "dispatched", DeliveryState: "delivered", To: "billing@acme.test", CreatedAt: at},
				{ID: "m2", EmailType: "payment_receipt", Status: "dispatched", DeliveryState: "bounced", To: "billing@acme.test", CreatedAt: at.Add(time.Minute)},
				{ID: "m3", EmailType: "payment_failed", Status: "pending", To: "billing@acme.test", CreatedAt: at.Add(2 * time.Minute)},
				{ID: "m4", EmailType: "payment_setup_request", Status: "skipped", Trigger: "finalize_no_pm", To: "billing@acme.test", CreatedAt: at.Add(3 * time.Minute)},
				{ID: "m5", EmailType: "payment_setup_request", Status: "dispatched", Trigger: "auto_charge_retry_no_pm", To: "billing@acme.test", CreatedAt: at.Add(4 * time.Minute)},
				{ID: "m6", EmailType: "credit_note", Status: "dispatched", DeliveryState: "complained", To: "billing@acme.test", CreatedAt: at.Add(5 * time.Minute)},
				{ID: "m7", EmailType: "invoice", Status: "failed", LastError: "send invoice email: smtp: 550 mailbox unavailable", To: "billing@acme.test", CreatedAt: at.Add(6 * time.Minute)},
			},
		})
		byID := map[string]wireRow{}
		for _, e := range events {
			if id, _ := e["id"].(string); strings.HasPrefix(id, "email:") {
				byID[strings.TrimPrefix(id, "email:")] = toWireRow(e)
			}
		}
		wantRow(t, byID["m1"], "Invoice email delivered", "neutral", "", "To billing@acme.test")
		wantRow(t, byID["m2"], "Receipt email bounced", "failure", "The customer's mail server rejected it.", "To billing@acme.test · Automatic")
		wantRow(t, byID["m3"], "Failed payment notice queued", "neutral", "", "To billing@acme.test · Automatic")
		wantRow(t, byID["m4"], "Payment setup email not sent", "neutral", "",
			"The invoice was already closed · No payment method when the invoice was finalized · To billing@acme.test · Automatic")
		wantRow(t, byID["m5"], "Payment setup email sent", "neutral", "", "A charge found no payment method · To billing@acme.test · Automatic")
		wantRow(t, byID["m6"], "Credit note email marked as spam", "failure", "", "To billing@acme.test")
		// The raw send error stays in the server's records, not on screen.
		wantRow(t, byID["m7"], "Invoice email couldn't be sent", "failure", "The customer did not get this email.", "To billing@acme.test")
	})

	t.Run("a failed charge's decline is written about the cardholder", func(t *testing.T) {
		for _, tc := range []struct {
			code, provider, outcome, title, tone, problem string
		}{
			{"expired_card", "Your card has expired.", "failed", "Payment failed", "failure", "Card declined: expired card"},
			{"incorrect_cvc", "Your card's security code is incorrect.", "failed", "Payment failed", "failure", "Card declined: incorrect security code"},
			{"stolen_card", "Your card was declined.", "failed", "Payment failed", "failure", "Card declined: reported lost or stolen"},
			{"generic_decline", "Your card was declined.", "failed", "Payment failed", "failure", "Card declined by the bank"},
			{"some_new_code", "Your card was declined for a new reason.", "failed", "Payment failed", "failure", "Your card was declined for a new reason."},
			{"", "", "unknown", "Payment result unknown", "warning", "The payment provider has not confirmed whether this charge went through."},
			{"", "", "succeeded", "Payment succeeded", "warning", "The invoice is not marked paid yet."},
		} {
			events := fetchWire(t, wireCase{
				inv: finalized,
				attempts: []domain.InvoiceChargeAttempt{{
					ID: "a1", StripePaymentIntentID: "pi_1", Trigger: domain.ChargeTriggerAutoCharge, Outcome: domain.ChargeAttemptOutcome(tc.outcome),
					ProviderReason: tc.provider, DeclineCode: tc.code, AmountCents: 4900, OccurredAt: at,
				}},
			})
			row := oneRow(t, events, "charge_attempt."+tc.outcome)
			wantRow(t, row, tc.title, tc.tone, tc.problem, "*")
			if row.Amount != 4900 {
				t.Errorf("%s: amount %d, want 4900", tc.title, row.Amount)
			}
		}
	})

	// The dashboard built "after N retry attempts" itself; the backend now
	// says it in the paid row, and the recovering retry folds into it.
	t.Run("paid after retries: the paid row says so, once", func(t *testing.T) {
		paidAt := at.Add(7 * 24 * time.Hour)
		events := fetchWire(t, wireCase{
			inv: domain.Invoice{
				Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, AmountPaidCents: 4900, PaidAt: &paidAt,
				StripePaymentIntentID: "pi_ok", PaymentCardBrand: "visa", PaymentCardLast4: "4242", IssuedAt: &at,
			},
			runs: []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningResolved, Resolution: domain.ResolutionPaymentRecovered}},
			events: []domain.InvoiceDunningEvent{
				{ID: "e1", EventType: domain.DunningEventRetryAttempted, AttemptCount: 1, CreatedAt: at.Add(3 * 24 * time.Hour), Reason: "payment failed: Your card was declined."},
				{ID: "e2", EventType: domain.DunningEventRetryAttempted, AttemptCount: 2, CreatedAt: paidAt, Reason: "succeeded"},
				{ID: "e3", EventType: domain.DunningEventResolved, CreatedAt: paidAt, Reason: "payment_recovered"},
			},
		})
		paid := oneRow(t, events, "invoice.paid")
		wantRow(t, paid, "Invoice paid", "success", "", "Visa •••• 4242 · After 2 retries")
		if paid.Amount != 4900 {
			t.Errorf("paid amount %d, want 4900", paid.Amount)
		}
		retries := rowsOf(events, "retry_attempted")
		if len(retries) != 1 || retries[0].Description != "Payment retry 1 failed" {
			t.Errorf("retry rows %v, want only the failed retry 1: the recovering retry folds into Invoice paid", retries)
		}
	})

	t.Run("lifecycle rows: words, amounts and who did it", func(t *testing.T) {
		created := at.Add(-time.Hour)
		voided := at.Add(time.Hour)
		events := fetchWire(t, wireCase{
			inv: domain.Invoice{
				Status: domain.InvoiceVoided, AmountDueCents: 4900, TotalAmountCents: 4900, IssuedAt: &at, VoidedAt: &voided,
				BillingReason: domain.BillingReasonManual, CreatedAt: created,
			},
			audit: map[string][]domain.AuditEntry{"invoice:{inv}": {
				{Action: domain.AuditActionVoid, ActorType: "api_key", ActorName: "billing-sync", CreatedAt: voided},
				{Action: domain.AuditActionFinalize, ActorType: "user", ActorName: "alice@example.com", CreatedAt: at},
				{Action: domain.AuditActionCreate, ActorType: "user", ActorName: "alice@example.com", CreatedAt: created},
			}},
		})
		wantRow(t, oneRow(t, events, "invoice.created"), "Invoice created", "neutral", "", "by alice@example.com")
		fin := oneRow(t, events, "invoice.finalized")
		wantRow(t, fin, "Invoice finalized", "neutral", "", "by alice@example.com")
		void := oneRow(t, events, "invoice.voided")
		wantRow(t, void, "Invoice voided", "neutral", "", "by API key billing-sync")
		if fin.Amount != 4900 || void.Amount != 4900 {
			t.Errorf("finalized amount %d, voided amount %d; want 4900 each (asked, then canceled)", fin.Amount, void.Amount)
		}
	})

	t.Run("an engine invoice says why it exists", func(t *testing.T) {
		events := fetchWire(t, wireCase{inv: domain.Invoice{
			Status: domain.InvoiceFinalized, AmountDueCents: 4900, TotalAmountCents: 4900, IssuedAt: &at,
			BillingReason: domain.BillingReasonSubscriptionCreate,
		}})
		wantRow(t, oneRow(t, events, "invoice.created"), "Invoice created", "neutral", "", "First invoice")
	})

	t.Run("offline payment names the reference and who entered it", func(t *testing.T) {
		events := fetchWire(t, wireCase{
			inv: domain.Invoice{Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, AmountPaidCents: 3900, PaidAt: &at, PaidOutOfBand: true},
			audit: map[string][]domain.AuditEntry{"invoice:{inv}": {
				{Action: domain.AuditActionUpdate, ActorType: "user", ActorName: "alice@example.com", CreatedAt: at,
					Metadata: map[string]any{"action": "payment_recorded", "note": "Wire 1042"}},
			}},
		})
		row := oneRow(t, events, "invoice.paid")
		wantRow(t, row, "Offline payment received", "success", "", "Wire 1042 · by alice@example.com")
		if row.Amount != 3900 {
			t.Errorf("amount %d, want 3900", row.Amount)
		}
	})

	t.Run("marked uncollectible by payment recovery", func(t *testing.T) {
		events := fetchWire(t, wireCase{
			inv:    domain.Invoice{Status: domain.InvoiceUncollectible, AmountDueCents: 4900, IssuedAt: &at, UncollectibleAt: &at},
			runs:   []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningEscalated, Resolution: domain.ResolutionRetriesExhausted}},
			events: []domain.InvoiceDunningEvent{{ID: "e1", EventType: domain.DunningEventEscalated, AttemptCount: 3, CreatedAt: at, Reason: "invoice_marked_uncollectible"}},
			audit: map[string][]domain.AuditEntry{"invoice:{inv}": {
				{Action: domain.AuditActionUpdate, ActorType: "system", ActorID: "system", CreatedAt: at,
					Metadata: map[string]any{"action": "marked_uncollectible", "amount_due_cents": float64(4900)}},
			}},
		})
		row := oneRow(t, events, "invoice.marked_uncollectible")
		wantRow(t, row, "Invoice marked uncollectible", "warning", "", "Payment retries ran out · Automatic")
		if row.Amount != 4900 {
			t.Errorf("amount %d, want the 4900 written off", row.Amount)
		}
	})

	t.Run("disputes read under the same rules", func(t *testing.T) {
		closed := at.Add(30 * 24 * time.Hour)
		due := at.Add(14 * 24 * time.Hour)
		base := domain.InvoiceDispute{StripePaymentIntentID: "pi_1", Reason: "product_not_received", AmountCents: 4500, Currency: "USD", OpenedAt: at}
		open := base
		open.ID, open.StripeDisputeID, open.Status, open.EvidenceDueBy = "d_open", "du_open", domain.DisputeNeedsResponse, &due
		mk := func(id string, s domain.DisputeStatus) domain.InvoiceDispute {
			d := base
			d.ID, d.StripeDisputeID, d.Status, d.ClosedAt = id, "du_"+id, s, &closed
			return d
		}
		events := fetchWire(t, wireCase{
			inv:      domain.Invoice{Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, AmountPaidCents: 4500, PaidAt: &at},
			disputes: []domain.InvoiceDispute{open, mk("lost", domain.DisputeLost), mk("won", domain.DisputeWon), mk("inq", domain.DisputeWarningClosed), mk("prev", domain.DisputePrevented)},
		})
		byID := map[string]wireRow{}
		for _, e := range events {
			if id, _ := e["id"].(string); strings.HasPrefix(id, "dispute:") {
				byID[id] = toWireRow(e)
			}
		}
		opened := byID["dispute:opened:d_open"]
		wantRow(t, opened, "Payment disputed", "warning", "", "Stripe dispute du_open · Reason: Product not received · Respond in Stripe by Feb 15, 2027 04:30 UTC")
		wantRow(t, byID["dispute:closed:lost"], "Dispute closed", "failure", "Lost: the bank took the payment back.", "Stripe dispute du_lost")
		wantRow(t, byID["dispute:closed:won"], "Dispute closed", "success", "", "Won · Stripe dispute du_won")
		wantRow(t, byID["dispute:closed:inq"], "Dispute closed", "neutral", "", "Inquiry closed without a chargeback · Stripe dispute du_inq")
		wantRow(t, byID["dispute:closed:prev"], "Dispute closed", "neutral", "", "Prevented before a chargeback · Stripe dispute du_prev")
		if opened.Amount != 4500 || byID["dispute:closed:lost"].Amount != 4500 || byID["dispute:closed:won"].Amount != 4500 {
			t.Errorf("amounts: opened %d, lost %d, won %d; want 4500 each (money asked back or moved)", opened.Amount, byID["dispute:closed:lost"].Amount, byID["dispute:closed:won"].Amount)
		}
		if byID["dispute:closed:inq"].Amount != 0 || byID["dispute:closed:prev"].Amount != 0 {
			t.Errorf("an inquiry or prevented dispute moved no money, so it shows no amount")
		}
	})

	t.Run("every row carries a line whose fields the legacy ones repeat", func(t *testing.T) {
		paidAt := at.Add(time.Hour)
		events := fetchWire(t, wireCase{
			inv:      domain.Invoice{Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, AmountPaidCents: 4900, TotalAmountCents: 4900, IssuedAt: &at, PaidAt: &paidAt, StripePaymentIntentID: "pi_ok"},
			attempts: []domain.InvoiceChargeAttempt{{ID: "a1", StripePaymentIntentID: "pi_bad", Outcome: domain.ChargeAttemptFailed, DeclineCode: "insufficient_funds", AmountCents: 4900, OccurredAt: at}},
			runs:     []domain.InvoiceDunningRun{{ID: "run_1", State: domain.DunningResolved, Resolution: domain.ResolutionPaymentRecovered}},
			events:   []domain.InvoiceDunningEvent{{ID: "e0", EventType: domain.DunningEventStarted, CreatedAt: at, Reason: "payment_failed"}},
			emails:   []EmailEventRow{{ID: "m1", EmailType: "payment_receipt", Status: "dispatched", CreatedAt: paidAt}},
		})
		if len(events) < 5 {
			t.Fatalf("%d rows, want at least 5", len(events))
		}
		for _, e := range events {
			line, ok := e["line"].(map[string]any)
			if !ok {
				t.Fatalf("row %v has no line", e["id"])
			}
			if line["id"] != e["id"] || line["title"] != e["description"] {
				t.Errorf("line id/title %v/%v, row %v/%v", line["id"], line["title"], e["id"], e["description"])
			}
			occurred, _ := time.Parse(time.RFC3339, line["occurred_at"].(string))
			ts, _ := time.Parse(time.RFC3339, e["timestamp"].(string))
			if !occurred.Truncate(time.Second).Equal(ts) {
				t.Errorf("%v: line occurred_at %v, timestamp %v", e["id"], occurred, ts)
			}
			if _, ok := line["detail"].([]any); !ok {
				t.Errorf("%v: line.detail is %T, want a list", e["id"], line["detail"])
			}
			if k, _ := line["kind"].(string); k == "" {
				t.Errorf("%v: line has no kind", e["id"])
			}
			if _, ok := line["recorded_at"]; ok {
				t.Errorf("%v: a real-clock invoice's line carries recorded_at", e["id"])
			}
		}
	})
}
