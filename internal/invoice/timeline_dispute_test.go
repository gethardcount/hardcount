package invoice

import (
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestDisputeTimelineEvents: each dispute renders an opened row at Stripe's
// creation time, and a closed row at Stripe's close time once it is closed.
// The status words drive the dot's colour, so they must agree with the
// outcome: red for lost, green for won or an inquiry that went no further,
// amber while open. The respond-by date shows only while the dispute is open,
// when the operator can still act on it.
func TestDisputeTimelineEvents(t *testing.T) {
	opened := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	closed := opened.Add(30 * 24 * time.Hour)
	due := opened.Add(14 * 24 * time.Hour)
	open := domain.InvoiceDispute{
		ID: "vlx_dsp_open", StripeDisputeID: "du_open", StripePaymentIntentID: "pi_1",
		Status: domain.DisputeNeedsResponse, Reason: "fraudulent", AmountCents: 4500, Currency: "USD",
		OpenedAt: opened, EvidenceDueBy: &due,
	}
	lost := open
	lost.ID, lost.StripeDisputeID, lost.Status, lost.ClosedAt = "vlx_dsp_lost", "du_lost", domain.DisputeLost, &closed
	won := lost
	won.ID, won.StripeDisputeID, won.Status = "vlx_dsp_won", "du_won", domain.DisputeWon
	inquiry := lost
	inquiry.ID, inquiry.StripeDisputeID, inquiry.Status = "vlx_dsp_inq", "du_inq", domain.DisputeWarningClosed

	rows := disputeTimelineEvents([]domain.InvoiceDispute{open, lost, won, inquiry})
	if len(rows) != 7 {
		t.Fatalf("%d rows, want 7 (one for the open dispute, two for each closed one)", len(rows))
	}
	type want struct{ id, eventType, status, desc string }
	for i, w := range []want{
		{"dispute:opened:vlx_dsp_open", domain.EventPaymentDisputeOpened, "pending", "Payment disputed"},
		{"dispute:opened:vlx_dsp_lost", domain.EventPaymentDisputeOpened, "pending", "Payment disputed"},
		{"dispute:closed:vlx_dsp_lost", domain.EventPaymentDisputeClosed, "failed", "Dispute lost: the bank took the payment back"},
		{"dispute:opened:vlx_dsp_won", domain.EventPaymentDisputeOpened, "pending", "Payment disputed"},
		{"dispute:closed:vlx_dsp_won", domain.EventPaymentDisputeClosed, "succeeded", "Dispute won"},
		{"dispute:opened:vlx_dsp_inq", domain.EventPaymentDisputeOpened, "pending", "Payment disputed"},
		{"dispute:closed:vlx_dsp_inq", domain.EventPaymentDisputeClosed, "succeeded", "Dispute inquiry closed without a chargeback"},
	} {
		r := rows[i]
		if r.ID != w.id || r.EventType != w.eventType || r.Status != w.status || r.Description != w.desc || r.Source != "dispute" {
			t.Errorf("row %d = {%s %s %s %q %s}, want {%s %s %s %q dispute}", i, r.ID, r.EventType, r.Status, r.Description, r.Source,
				w.id, w.eventType, w.status, w.desc)
		}
		if r.AmountCents == nil || *r.AmountCents != 4500 || r.PaymentIntentID != "pi_1" || r.IsSimulated {
			t.Errorf("row %d: amount %v, pi %q, simulated %v; want 4500, pi_1, a real-world row", i, r.AmountCents, r.PaymentIntentID, r.IsSimulated)
		}
	}
	if !rows[0].sortAt.Equal(opened) || !rows[2].sortAt.Equal(closed) {
		t.Errorf("rows sit at %s and %s, want Stripe's opened %s and closed %s", rows[0].sortAt, rows[2].sortAt, opened, closed)
	}
	if !strings.Contains(rows[0].Detail, "respond in Stripe by Oct 15, 2026 09:00 UTC") || !strings.Contains(rows[0].Detail, "du_open") {
		t.Errorf("open dispute detail %q, want the dispute id and the respond-by date", rows[0].Detail)
	}
	if strings.Contains(rows[1].Detail, "respond in Stripe") {
		t.Errorf("closed dispute detail %q still asks for a response", rows[1].Detail)
	}

	// Same instant as the payment it disputes: the dispute sorts after it.
	at := opened
	events := []timelineEvent{
		{EventType: domain.EventPaymentDisputeOpened, sortAt: at, tieRank: rankDisputeOpened},
		{EventType: "invoice.paid", sortAt: at, tieRank: rankLifecycleTerminal},
	}
	sortInvoiceTimeline(events)
	if events[0].EventType != "invoice.paid" {
		t.Errorf("same-instant order %s, %s; want the payment before its dispute", events[0].EventType, events[1].EventType)
	}
}
