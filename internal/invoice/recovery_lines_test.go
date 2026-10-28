package invoice

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/activity"
)

// The Dunning page's recovery timeline (ADR-128 §3.3) carries the lines the
// invoice's Activity card shows for the same payment-recovery events, built
// by the same code from the same sources, without that page's folds.

// recoveryLinesOf builds the Dunning page's lines for the case's one run,
// and the invoice page's lines for the same events, keyed by event id.
func recoveryLinesOf(t *testing.T, c wireCase) (dunning map[string]activity.Line, invoicePage map[string]activity.Line, degraded []string) {
	t.Helper()
	h, invoiceID := wireHandler(t, c)
	run := c.runs[0]
	run.InvoiceID = invoiceID
	dunning, degraded, err := h.RecoveryRunLines(context.Background(), "t1", run, c.events)
	if err != nil {
		t.Fatalf("RecoveryRunLines: %v", err)
	}
	invoicePage = map[string]activity.Line{}
	for _, e := range fetchWire(t, c) {
		id, _ := e["id"].(string)
		if evtID, ok := strings.CutPrefix(id, "dunning:"); ok {
			invoicePage[evtID] = lineOfWire(t, e)
		}
	}
	return dunning, invoicePage, degraded
}

// lineOfWire decodes a timeline row's `line` back into an activity.Line.
func lineOfWire(t *testing.T, e map[string]any) activity.Line {
	t.Helper()
	var l activity.Line
	if err := remarshal(e["line"], &l); err != nil {
		t.Fatalf("decode line: %v", err)
	}
	return l
}

func TestRecoveryRunLines_SameWordsAsTheInvoicePageWithoutItsFolds(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	t1, t2 := t0.Add(72*time.Hour), t0.Add(192*time.Hour)
	wall := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	run := domain.InvoiceDunningRun{ID: "hc_dr_1", State: domain.DunningEscalated, Resolution: domain.ResolutionRetriesExhausted}

	// A written-off invoice on a test clock: one decline, its retry declined
	// with a mapped code, then retries ran out, which paused collection and
	// wrote the invoice off.
	writtenOff := domain.Invoice{
		Status: domain.InvoiceUncollectible, PaymentStatus: domain.PaymentFailed, AmountDueCents: 4900, TotalAmountCents: 4900,
		IssuedAt: &t0, UncollectibleAt: &t2, IsSimulated: true, InvoiceNumber: "HC-000123",
	}
	c := wireCase{
		inv:  writtenOff,
		runs: []domain.InvoiceDunningRun{run},
		events: []domain.InvoiceDunningEvent{
			{ID: "evt_start", RunID: run.ID, EventType: domain.DunningEventStarted, Reason: string(domain.DunningCausePaymentFailed), CreatedAt: t0, RecordedAt: &wall},
			{ID: "evt_retry", RunID: run.ID, EventType: domain.DunningEventRetryAttempted, AttemptCount: 1, CreatedAt: t1, RecordedAt: &wall,
				Reason: "payment failed: Your card has insufficient funds.", Metadata: map[string]any{"payment_intent_id": "pi_r1"}},
			{ID: "evt_out", RunID: run.ID, EventType: domain.DunningEventEscalated, AttemptCount: 1, CreatedAt: t2, RecordedAt: &wall,
				Reason: "subscription_paused+invoice_marked_uncollectible"},
		},
		attempts: []domain.InvoiceChargeAttempt{{
			ID: "att_1", StripePaymentIntentID: "pi_r1", Outcome: domain.ChargeAttemptFailed, Trigger: domain.ChargeTriggerDunningRetry,
			AmountCents: 4900, DeclineCode: "insufficient_funds", ProviderReason: "Your card has insufficient funds.", OccurredAt: wall,
		}},
		emails: []EmailEventRow{
			{ID: "em_1", EmailType: "dunning_warning", Status: "dispatched", DeliveryState: "delivered", AttemptNumber: 1, CreatedAt: wall},
			{ID: "em_2", EmailType: "dunning_escalation", Status: "dispatched", DeliveryState: "bounced", CreatedAt: wall},
		},
	}
	dunning, onInvoice, degraded := recoveryLinesOf(t, c)
	if len(degraded) != 0 {
		t.Errorf("degraded %v", degraded)
	}
	want := map[string]struct {
		title, tone, problem, meta string
		amount                     int64
		at                         time.Time
	}{
		"evt_start": {"Payment recovery started", "neutral", "", "Automatic", 0, t0},
		"evt_retry": {"Payment retry 1 failed", "failure", "Card declined: insufficient funds", "Payment reminder email delivered · Automatic", 4900, t1},
		// Nothing folded: the write-off is named here, where no
		// "Invoice marked uncollectible" row stands beside it.
		"evt_out": {"Payment retries ran out", "warning", "Collection paused · Invoice marked uncollectible", "Final notice bounced · Automatic", 0, t2},
	}
	if len(dunning) != len(want) {
		t.Fatalf("%d lines, want %d: %v", len(dunning), len(want), dunning)
	}
	for id, w := range want {
		l := dunning[id]
		var amount int64
		if l.Amount != nil {
			amount = l.Amount.Cents
		}
		if l.Title != w.title || string(l.Tone) != w.tone || l.Problem != w.problem || activity.MetaText(l) != w.meta || amount != w.amount {
			t.Errorf("%s: %q %s %q %q %d; want %q %s %q %q %d", id, l.Title, l.Tone, l.Problem, activity.MetaText(l), amount,
				w.title, w.tone, w.problem, w.meta, w.amount)
		}
		if l.ID != "dunning:"+id || !l.OccurredAt.Equal(w.at) || l.RecordedAt == nil || !l.RecordedAt.Equal(wall) {
			t.Errorf("%s: id %q occurred %v recorded %v; want dunning:%s, %v, Recorded %v", id, l.ID, l.OccurredAt, l.RecordedAt, id, w.at, wall)
		}
	}
	// One wording: every line the invoice page shows for these events is the
	// Dunning page's line, except the write-off the invoice page folds.
	for id, il := range onInvoice {
		dl := dunning[id]
		if id == "evt_out" {
			if il.Problem != "Collection paused" {
				t.Errorf("invoice page retries-ran-out problem %q, want the write-off folded", il.Problem)
			}
			il.Problem = dl.Problem
		}
		if !reflect.DeepEqual(il, dl) {
			t.Errorf("%s reads differently on the two pages\ninvoice: %+v\nDunning: %+v", id, il, dl)
		}
	}
	if len(onInvoice) != 3 {
		t.Errorf("the invoice page shows %d of the run's events, want all 3", len(onInvoice))
	}
}

func TestRecoveryRunLines_TheRunsEndAndItsRecoveringRetryShow(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(72 * time.Hour)

	// Recovered by a retry: the invoice page folds the retry and the run's
	// end into "Invoice paid · After 1 retry"; the Dunning page shows both.
	recovered := domain.InvoiceDunningRun{ID: "hc_dr_2", State: domain.DunningResolved, Resolution: domain.ResolutionPaymentRecovered}
	paid := domain.Invoice{
		Status: domain.InvoicePaid, PaymentStatus: domain.PaymentSucceeded, TotalAmountCents: 4900, AmountPaidCents: 4900,
		IssuedAt: &t0, PaidAt: &t1, StripePaymentIntentID: "pi_ok",
	}
	dunning, onInvoice, _ := recoveryLinesOf(t, wireCase{
		inv:  paid,
		runs: []domain.InvoiceDunningRun{recovered},
		events: []domain.InvoiceDunningEvent{
			{ID: "evt_start", RunID: recovered.ID, EventType: domain.DunningEventStarted, Reason: string(domain.DunningCausePaymentFailed), CreatedAt: t0},
			// As dunning.Service writes it on the success path: no
			// metadata, since RetryPayment returns only an error.
			{ID: "evt_retry", RunID: recovered.ID, EventType: domain.DunningEventRetryAttempted, AttemptCount: 1, Reason: "succeeded", CreatedAt: t1},
			{ID: "evt_end", RunID: recovered.ID, EventType: domain.DunningEventResolved, CreatedAt: t1},
		},
		attempts: []domain.InvoiceChargeAttempt{{
			ID: "att_ok", StripePaymentIntentID: "pi_ok", Outcome: domain.ChargeAttemptSucceeded, Trigger: domain.ChargeTriggerDunningRetry,
			AmountCents: 4900, OccurredAt: t1,
		}},
	})
	for id, want := range map[string]string{
		"evt_retry": "Payment retry 1 succeeded · Automatic · success",
		"evt_end":   "Payment recovery ended · Invoice paid · Automatic · success",
	} {
		l := dunning[id]
		got := l.Title
		if l.Amount != nil {
			got += " · " + activity.FormatMoney(*l.Amount)
		}
		got += " · " + activity.MetaText(l) + " · " + string(l.Tone)
		if got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
		if l.RecordedAt != nil {
			t.Errorf("%s on the real clock has a Recorded time", id)
		}
		if _, folded := onInvoice[id]; folded {
			t.Errorf("%s shows on the invoice page; it folds into Invoice paid", id)
		}
	}

	// An operator's resolve names the operator, from the run's audit row.
	voided := domain.InvoiceDunningRun{ID: "hc_dr_3", State: domain.DunningResolved, Resolution: domain.ResolutionInvoiceVoided}
	dunning, _, _ = recoveryLinesOf(t, wireCase{
		inv:    domain.Invoice{Status: domain.InvoiceVoided, TotalAmountCents: 4900, IssuedAt: &t0, VoidedAt: &t1},
		runs:   []domain.InvoiceDunningRun{voided},
		events: []domain.InvoiceDunningEvent{{ID: "evt_end", RunID: voided.ID, EventType: domain.DunningEventResolved, CreatedAt: t1}},
		audit: map[string][]domain.AuditEntry{"dunning_run:" + voided.ID: {{
			Action: domain.AuditActionUpdate, ActorType: "user", ActorName: "alice@example.com", Metadata: map[string]any{"action": "resolved"},
		}}},
	})
	if got := activity.MetaText(dunning["evt_end"]); got != "Invoice voided · by alice@example.com" {
		t.Errorf("operator resolve meta %q", got)
	}
}

func TestRecoveryRunLines_UnreadableLanesAreNamed(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	run := domain.InvoiceDunningRun{ID: "hc_dr_4", State: domain.DunningActive}
	c := wireCase{
		inv:    domain.Invoice{Status: domain.InvoiceFinalized, TotalAmountCents: 4900, AmountDueCents: 4900, IssuedAt: &t0, InvoiceNumber: "HC-9"},
		runs:   []domain.InvoiceDunningRun{run},
		events: []domain.InvoiceDunningEvent{{ID: "evt_start", RunID: run.ID, EventType: domain.DunningEventStarted, CreatedAt: t0}},
	}
	h, invoiceID := wireHandler(t, c)
	h.emailEvents = &emailLaneFake{err: errors.New("db down")}
	run.InvoiceID = invoiceID
	lines, degraded, err := h.RecoveryRunLines(context.Background(), "t1", run, c.events)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(degraded, []string{"emails"}) {
		t.Errorf("degraded %v, want [emails]", degraded)
	}
	if lines["evt_start"].Title != "Payment recovery started" {
		t.Errorf("a degraded lane still leaves the line: %+v", lines["evt_start"])
	}

	// The run's invoice cannot be read: no line is built, and the caller
	// is told.
	run.InvoiceID = "hc_inv_missing"
	if _, _, err := h.RecoveryRunLines(context.Background(), "t1", run, c.events); err == nil {
		t.Error("no error for a run whose invoice cannot be read")
	}
}

func remarshal(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
