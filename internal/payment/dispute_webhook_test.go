package payment

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
)

// fakeDisputeRecorder captures the snapshots handleDispute hands the store.
type fakeDisputeRecorder struct {
	snaps []domain.DisputeSnapshot
	err   error
}

func (f *fakeDisputeRecorder) RecordStripeDispute(_ context.Context, _ string, s domain.DisputeSnapshot) (domain.DisputeRecordResult, error) {
	f.snaps = append(f.snaps, s)
	if f.err != nil {
		return domain.DisputeRecordResult{}, f.err
	}
	return domain.DisputeRecordResult{Dispute: domain.InvoiceDispute{InvoiceID: "inv_1", Status: s.Status}, Applied: true}, nil
}

// capturingWebhookStore keeps every event it ingests, to check the audit row.
type capturingWebhookStore struct {
	ingested []domain.StripeWebhookEvent
}

func (c *capturingWebhookStore) WasProcessed(_ context.Context, _, id string) (bool, error) {
	for _, e := range c.ingested {
		if e.StripeEventID == id {
			return true, nil
		}
	}
	return false, nil
}

func (c *capturingWebhookStore) IngestEvent(_ context.Context, _ string, e domain.StripeWebhookEvent) (domain.StripeWebhookEvent, bool, error) {
	c.ingested = append(c.ingested, e)
	return e, true, nil
}

func (c *capturingWebhookStore) ListByInvoice(context.Context, string, string) ([]domain.StripeWebhookEvent, error) {
	return nil, nil
}

// disputeObject is a Stripe Dispute as data.object carries it
// (https://docs.stripe.com/api/disputes/object): its own metadata is empty,
// and it names the disputed payment in payment_intent.
func disputeObject(status string, paymentIntent any) map[string]any {
	return map[string]any{
		"id": "du_test_1", "object": "dispute", "amount": 4500, "currency": "usd",
		"charge": "ch_test_1", "payment_intent": paymentIntent, "reason": "fraudulent",
		"status": status, "created": 1790000000, "livemode": true, "metadata": map[string]string{},
		"is_charge_refundable": false, "balance_transactions": []any{},
		"evidence_details": map[string]any{"due_by": 1791000000, "has_evidence": false, "past_due": false, "submission_count": 0},
	}
}

func disputeRaw(t *testing.T, obj map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"data": map[string]any{"object": obj}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

// TestEndpointAuthoritativeEvent_Disputes: a Dispute carries its own (empty)
// metadata, so the three dispute events Hardcount handles must be admitted on the
// endpoint's tenant alone, or every dispute is skipped as "no hardcount metadata".
// The funds events are not handled, so they stay out.
func TestEndpointAuthoritativeEvent_Disputes(t *testing.T) {
	for _, typ := range []string{"charge.dispute.created", "charge.dispute.updated", "charge.dispute.closed"} {
		if !endpointAuthoritativeEvent(typ) {
			t.Errorf("%s must be admitted without hardcount_tenant_id metadata", typ)
		}
	}
	for _, typ := range []string{"charge.dispute.funds_withdrawn", "charge.dispute.funds_reinstated", "payment_intent.succeeded"} {
		if endpointAuthoritativeEvent(typ) {
			t.Errorf("%s must still require hardcount_tenant_id metadata", typ)
		}
	}
}

// TestStripeWebhook_DisputeWithoutMetadataIsRecorded drives a signed
// charge.dispute.created through the real handler: it is processed, not
// skipped, the store gets the whole snapshot, and the audit row names the
// disputed PaymentIntent rather than the dispute's own id.
func TestStripeWebhook_DisputeWithoutMetadataIsRecorded(t *testing.T) {
	webhooks := &capturingWebhookStore{}
	adapter := NewStripe(nil, newMockInvoiceUpdaterH(), webhooks, nil)
	rec := &fakeDisputeRecorder{}
	adapter.SetDisputeRecorder(rec)
	const secret = "whsec_dispute_test"
	handler := NewHandler(adapter, &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"hc_spc_dsp": {ID: "hc_spc_dsp", TenantID: "t1", Livemode: true, WebhookSecret: secret},
	}})

	created := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	body, _ := json.Marshal(map[string]any{
		"id": "evt_dsp_1", "object": "event", "type": "charge.dispute.created",
		"created": created.Unix(), "livemode": true,
		"data": map[string]any{"object": disputeObject("needs_response", "pi_disputed")},
	})
	req := httptest.NewRequest("POST", "/stripe/hc_spc_dsp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	w := httptest.NewRecorder()
	handler.Routes().ServeHTTP(w, req)

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"processed"`) {
		t.Fatalf("response %d %s, want 200 processed (not skipped for missing metadata)", w.Code, w.Body.String())
	}
	if len(rec.snaps) != 1 {
		t.Fatalf("recorder calls: %d, want 1", len(rec.snaps))
	}
	due := time.Unix(1791000000, 0).UTC()
	want := domain.DisputeSnapshot{
		StripeEventID: "evt_dsp_1", EventAt: created, StripeDisputeID: "du_test_1",
		StripeChargeID: "ch_test_1", StripePaymentIntentID: "pi_disputed",
		Status: domain.DisputeNeedsResponse, Reason: "fraudulent", AmountCents: 4500, Currency: "usd",
		EvidenceDueBy: &due, OpenedAt: time.Unix(1790000000, 0).UTC(),
	}
	got := rec.snaps[0]
	if got.EvidenceDueBy == nil || !got.EvidenceDueBy.Equal(due) {
		t.Errorf("evidence due by = %v, want %s", got.EvidenceDueBy, due)
	}
	got.EvidenceDueBy, want.EvidenceDueBy = nil, nil
	if !got.EventAt.Equal(want.EventAt) || !got.OpenedAt.Equal(want.OpenedAt) {
		t.Errorf("times: event %s opened %s, want %s / %s", got.EventAt, got.OpenedAt, want.EventAt, want.OpenedAt)
	}
	got.EventAt, want.EventAt, got.OpenedAt, want.OpenedAt = time.Time{}, time.Time{}, time.Time{}, time.Time{}
	if got != want {
		t.Errorf("snapshot\n got %+v\nwant %+v", got, want)
	}
	if len(webhooks.ingested) != 1 || webhooks.ingested[0].PaymentIntentID != "pi_disputed" {
		t.Errorf("audit rows %+v, want one naming pi_disputed", webhooks.ingested)
	}
}

// TestHandleDispute_Outcomes pins how each result reaches Stripe: recorded
// and ack'd; not a Hardcount payment → ack'd with its audit row; a store failure
// or a status Hardcount does not know → an error, so Stripe redelivers and no
// dedup row is written; an expanded payment_intent object is read the same
// as a bare id.
func TestHandleDispute_Outcomes(t *testing.T) {
	cases := []struct {
		name       string
		obj        map[string]any
		recErr     error
		wantErr    bool
		wantCalls  int
		wantIngest bool
	}{
		{"recorded", disputeObject("lost", "pi_x"), nil, false, 1, true},
		{"expanded payment intent", disputeObject("won", map[string]any{"id": "pi_x", "object": "payment_intent"}), nil, false, 1, true},
		{"no Hardcount invoice holds the payment", disputeObject("needs_response", "pi_foreign"), errs.ErrNotFound, false, 1, true},
		{"no payment intent at all", disputeObject("needs_response", nil), nil, false, 0, true},
		{"store failure", disputeObject("lost", "pi_x"), errors.New("db down"), true, 1, false},
		{"status Hardcount does not know", disputeObject("under_arbitration", "pi_x"), nil, true, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			webhooks := newMockWebhookStore()
			s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), webhooks, nil)
			rec := &fakeDisputeRecorder{err: tc.recErr}
			s.SetDisputeRecorder(rec)
			err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID: "evt_" + strings.ReplaceAll(tc.name, " ", "_"),
				EventType:     "charge.dispute.closed",
				OccurredAt:    time.Now().UTC(),
				Payload:       map[string]any{"raw": disputeRaw(t, tc.obj)},
			})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if len(rec.snaps) != tc.wantCalls {
				t.Errorf("recorder calls %d, want %d", len(rec.snaps), tc.wantCalls)
			}
			if len(rec.snaps) == 1 && rec.snaps[0].StripePaymentIntentID == "" {
				t.Errorf("snapshot has no payment intent: %+v", rec.snaps[0])
			}
			if got := webhooks.ingestCalls == 1; got != tc.wantIngest {
				t.Errorf("dedup row written = %v, want %v", got, tc.wantIngest)
			}
		})
	}
}

// TestHandleDispute_YoungDisputeOnAnUnknownPaymentIsRedelivered: a dispute
// can reach Hardcount before Hardcount has recorded the payment it disputes (a
// hosted-page payment whose settle has not committed, an off-session charge
// whose attempt row is written after Stripe answers; Stripe's test dispute
// cards open the dispute right after the charge). A dispute created within
// the race window that no invoice holds yet fails the delivery, so Stripe
// sends it again, and writes no dedup row. An older one is acknowledged as
// not Hardcount's, as before.
func TestHandleDispute_YoungDisputeOnAnUnknownPaymentIsRedelivered(t *testing.T) {
	cases := []struct {
		name       string
		age        time.Duration
		wantErr    bool
		wantIngest bool
	}{
		{"created a minute ago", time.Minute, true, false},
		{"created an hour ago", time.Hour, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			webhooks := newMockWebhookStore()
			s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), webhooks, nil)
			rec := &fakeDisputeRecorder{err: errs.ErrNotFound}
			s.SetDisputeRecorder(rec)
			obj := disputeObject("needs_response", "pi_not_recorded_yet")
			obj["created"] = time.Now().Add(-tc.age).Unix()
			err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID: "evt_dsp_race_" + strings.ReplaceAll(tc.name, " ", "_"),
				EventType:     "charge.dispute.created",
				OccurredAt:    time.Now().UTC(),
				Payload:       map[string]any{"raw": disputeRaw(t, obj)},
			})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if len(rec.snaps) != 1 {
				t.Errorf("recorder calls %d, want 1", len(rec.snaps))
			}
			if got := webhooks.ingestCalls == 1; got != tc.wantIngest {
				t.Errorf("dedup row written = %v, want %v", got, tc.wantIngest)
			}
		})
	}
}

// TestHandleDispute_RedeliveryAfterIngestSkips: once the dedup row is
// written, the same event id does not reach the store again.
func TestHandleDispute_RedeliveryAfterIngestSkips(t *testing.T) {
	webhooks := newMockWebhookStore()
	s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), webhooks, nil)
	rec := &fakeDisputeRecorder{}
	s.SetDisputeRecorder(rec)
	ev := domain.StripeWebhookEvent{
		StripeEventID: "evt_dsp_redeliver", EventType: "charge.dispute.created", OccurredAt: time.Now().UTC(),
		Payload: map[string]any{"raw": disputeRaw(t, disputeObject("needs_response", "pi_x"))},
	}
	for i := 0; i < 2; i++ {
		if err := s.HandleWebhook(context.Background(), "t1", ev); err != nil {
			t.Fatalf("delivery %d: %v", i+1, err)
		}
	}
	if len(rec.snaps) != 1 {
		t.Errorf("recorder calls %d, want 1", len(rec.snaps))
	}
}
