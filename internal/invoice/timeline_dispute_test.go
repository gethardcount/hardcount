package invoice

import (
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
)

// TestDisputeTimelineEvents: each dispute renders an opened row at Stripe's
// creation time, and a closed row at Stripe's close time once it is closed.
// The closings share one title, "Dispute closed" (ADR-128 rule 2), and the
// outcome is in the tone, which drives the dot, and the meta or problem
// line: red for lost, green for won, grey for an inquiry or a prevented
// dispute that went no further, amber while open. The respond-by time shows
// only while the dispute is open, when the operator can still act on it.
func TestDisputeTimelineEvents(t *testing.T) {
	opened := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	closed := opened.Add(30 * 24 * time.Hour)
	due := opened.Add(14 * 24 * time.Hour)
	open := domain.InvoiceDispute{
		ID: "hc_dsp_open", StripeDisputeID: "du_open", StripePaymentIntentID: "pi_1",
		Status: domain.DisputeNeedsResponse, Reason: "fraudulent", AmountCents: 4500, Currency: "USD",
		OpenedAt: opened, EvidenceDueBy: &due,
	}
	lost := open
	lost.ID, lost.StripeDisputeID, lost.Status, lost.ClosedAt = "hc_dsp_lost", "du_lost", domain.DisputeLost, &closed
	won := lost
	won.ID, won.StripeDisputeID, won.Status = "hc_dsp_won", "du_won", domain.DisputeWon
	inquiry := lost
	inquiry.ID, inquiry.StripeDisputeID, inquiry.Status = "hc_dsp_inq", "du_inq", domain.DisputeWarningClosed

	rows := disputeTimelineEvents([]domain.InvoiceDispute{open, lost, won, inquiry})
	if len(rows) != 7 {
		t.Fatalf("%d rows, want 7 (one for the open dispute, two for each closed one)", len(rows))
	}
	for i := range rows {
		rows[i].fillFromLine()
	}
	type want struct {
		id, eventType, status, desc string
		amount                      int64
	}
	for i, w := range []want{
		{"dispute:opened:hc_dsp_open", domain.EventPaymentDisputeOpened, "warning", "Payment disputed", 4500},
		{"dispute:opened:hc_dsp_lost", domain.EventPaymentDisputeOpened, "warning", "Payment disputed", 4500},
		{"dispute:closed:hc_dsp_lost", domain.EventPaymentDisputeClosed, "failed", "Dispute closed", 4500},
		{"dispute:opened:hc_dsp_won", domain.EventPaymentDisputeOpened, "warning", "Payment disputed", 4500},
		{"dispute:closed:hc_dsp_won", domain.EventPaymentDisputeClosed, "succeeded", "Dispute closed", 4500},
		{"dispute:opened:hc_dsp_inq", domain.EventPaymentDisputeOpened, "warning", "Payment disputed", 4500},
		// An inquiry that closed moved no money, so it shows no amount.
		{"dispute:closed:hc_dsp_inq", domain.EventPaymentDisputeClosed, "neutral", "Dispute closed", 0},
	} {
		r := rows[i]
		if r.ID != w.id || r.EventType != w.eventType || r.Status != w.status || r.Description != w.desc || r.Source != "dispute" {
			t.Errorf("row %d = {%s %s %s %q %s}, want {%s %s %s %q dispute}", i, r.ID, r.EventType, r.Status, r.Description, r.Source,
				w.id, w.eventType, w.status, w.desc)
		}
		var amount int64
		if r.AmountCents != nil {
			amount = *r.AmountCents
		}
		if amount != w.amount || r.PaymentIntentID != "pi_1" || r.IsSimulated || r.Line.RecordedAt != nil {
			t.Errorf("row %d: amount %d, pi %q, simulated %v; want %d, pi_1, a real-world row", i, amount, r.PaymentIntentID, r.IsSimulated, w.amount)
		}
	}
	if !rows[0].sortAt.Equal(opened) || !rows[2].sortAt.Equal(closed) {
		t.Errorf("rows sit at %s and %s, want Stripe's opened %s and closed %s", rows[0].sortAt, rows[2].sortAt, opened, closed)
	}
	if rows[0].Detail != "Stripe dispute du_open · Reason: Fraudulent · Respond in Stripe by Oct 15, 2026 09:00 UTC" {
		t.Errorf("open dispute detail %q, want the dispute id, the reason in words and the respond-by time", rows[0].Detail)
	}
	if strings.Contains(rows[1].Detail, "Respond in Stripe") {
		t.Errorf("closed dispute detail %q still asks for a response", rows[1].Detail)
	}
	if rows[2].Error != "Lost: the bank took the payment back." {
		t.Errorf("lost dispute problem %q", rows[2].Error)
	}

	// Same instant as the payment it disputes: the dispute sorts after it.
	at := opened
	events := []TimelineEvent{
		{EventType: domain.EventPaymentDisputeOpened, sortAt: at, tieRank: rankDisputeOpened},
		{EventType: "invoice.paid", sortAt: at, tieRank: rankLifecycleTerminal},
	}
	sortInvoiceTimeline(events)
	if events[0].EventType != "invoice.paid" {
		t.Errorf("same-instant order %s, %s; want the payment before its dispute", events[0].EventType, events[1].EventType)
	}
}
