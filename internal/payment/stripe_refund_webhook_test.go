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

// fakeExternalRefunds captures the snapshots handleRefundUpdated hands the
// recorder of refunds made in Stripe (ADR-134).
type fakeExternalRefunds struct {
	snaps []domain.StripeRefundSnapshot
	err   error
}

func (f *fakeExternalRefunds) RecordStripeRefund(_ context.Context, _ string, s domain.StripeRefundSnapshot) (domain.StripeRefundRecordResult, error) {
	f.snaps = append(f.snaps, s)
	if f.err != nil {
		return domain.StripeRefundRecordResult{}, f.err
	}
	return domain.StripeRefundRecordResult{Recorded: true, Applied: true}, nil
}

// refundRaw is a refund event body: data.object is a Stripe Refund
// (https://docs.stripe.com/api/refunds/object).
func refundRaw(t *testing.T, obj map[string]any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"data": map[string]any{"object": obj}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(b)
}

func dashboardRefund(created int64, md map[string]string) map[string]any {
	return map[string]any{
		"id": "re_dash", "object": "refund", "amount": 4000, "currency": "usd", "status": "succeeded",
		"charge": "ch_dash", "payment_intent": "pi_dash", "reason": "requested_by_customer",
		"created": created, "metadata": md,
	}
}

func refundAdapter(updater RefundStatusUpdater, rec ExternalRefundRecorder) *Stripe {
	s := NewStripe(&mockStripeClient{}, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
	s.SetRefundStatusUpdater(updater)
	if rec != nil {
		s.SetExternalRefundRecorder(rec)
	}
	return s
}

// TestEndpointAuthoritativeEvent_RefundCreated: a Refund carries its own
// metadata, never hardcount_tenant_id, so refund.created must be admitted on
// the endpoint's tenant, as the other refund events are, or every refund made
// in the Stripe dashboard is skipped (ADR-134).
func TestEndpointAuthoritativeEvent_RefundCreated(t *testing.T) {
	for _, typ := range []string{"refund.created", "refund.updated", "refund.failed", "charge.refund.updated"} {
		if !endpointAuthoritativeEvent(typ) {
			t.Errorf("%s must be admitted without hardcount_tenant_id metadata", typ)
		}
	}
	if endpointAuthoritativeEvent("charge.refunded") {
		t.Error("charge.refunded is not handled (it names no refund) and must still require metadata")
	}
}

// TestRefundWebhook_DashboardRefundIsRecorded: a refund with no
// hardcount_cn_id goes to the recorder with the whole snapshot, and never to
// the credit-note status writer by refund id alone.
func TestRefundWebhook_DashboardRefundIsRecorded(t *testing.T) {
	updater := &fakeRefundUpdater{}
	rec := &fakeExternalRefunds{}
	s := refundAdapter(updater, rec)
	created := time.Now().Add(-2 * time.Hour).Truncate(time.Second).UTC()
	at := created.Add(time.Minute)
	err := s.HandleWebhook(context.Background(), "t1", domain.StripeWebhookEvent{
		StripeEventID: "evt_rc_1", EventType: "refund.created", OccurredAt: at,
		Payload: map[string]any{"raw": refundRaw(t, dashboardRefund(created.Unix(), map[string]string{}))},
	})
	if err != nil {
		t.Fatalf("HandleWebhook: %v", err)
	}
	if len(updater.calls) != 0 {
		t.Errorf("status writer calls = %d, want 0: the recorder moves a note's status itself", len(updater.calls))
	}
	if len(rec.snaps) != 1 {
		t.Fatalf("recorder calls = %d, want 1", len(rec.snaps))
	}
	want := domain.StripeRefundSnapshot{
		StripeEventID: "evt_rc_1", EventAt: at, StripeRefundID: "re_dash", StripePaymentIntentID: "pi_dash",
		StripeChargeID: "ch_dash", AmountCents: 4000, Currency: "usd", Status: domain.StripeRefundSucceeded,
		Reason: "requested_by_customer", CreatedAt: created, Young: false,
	}
	if rec.snaps[0] != want {
		t.Errorf("snapshot = %+v\nwant       %+v", rec.snaps[0], want)
	}
}

// TestRefundWebhook_HardcountRefundNeverRecorded: a refund carrying
// hardcount_cn_id is Hardcount's own, whatever the database says. Its status
// goes to the credit note by id; when no note holds the id yet (the refund
// call runs after the issue commits), a young refund asks for redelivery and
// an old one is acknowledged. The recorder is never called.
func TestRefundWebhook_HardcountRefundNeverRecorded(t *testing.T) {
	md := map[string]string{"hardcount_cn_id": "hc_cn_own"}
	for _, tc := range []struct {
		name    string
		created int64
		wantErr bool
	}{
		{"young", time.Now().Unix(), true},
		{"old", time.Now().Add(-48 * time.Hour).Unix(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updater := &fakeRefundUpdater{err: errs.ErrNotFound}
			rec := &fakeExternalRefunds{}
			s := refundAdapter(updater, rec)
			err := s.handleRefundUpdated(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID: "evt_own", EventType: "refund.created", OccurredAt: time.Now(),
				Payload: map[string]any{"raw": refundRaw(t, dashboardRefund(tc.created, md))},
			})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if len(rec.snaps) != 0 {
				t.Errorf("recorder calls = %d, want 0: a refund with hardcount_cn_id is never a refund made in Stripe", len(rec.snaps))
			}
			if len(updater.calls) != 1 || updater.calls[0].refundID != "re_dash" {
				t.Errorf("status writer calls = %+v, want one for re_dash", updater.calls)
			}
		})
	}
}

// TestRefundWebhook_RecorderAnswers: what the recorder's answer does to the
// delivery. No invoice holds the payment: retry while young, acknowledge when
// old. Any other error: retry. A status Hardcount does not know fails the
// delivery before the recorder.
func TestRefundWebhook_RecorderAnswers(t *testing.T) {
	young, old := time.Now().Unix(), time.Now().Add(-48*time.Hour).Unix()
	for _, tc := range []struct {
		name      string
		created   int64
		status    string
		recErr    error
		wantErr   bool
		wantCalls int
	}{
		{"not found, young: retry", young, "succeeded", errs.ErrNotFound, true, 1},
		{"not found, old: acknowledged", old, "succeeded", errs.ErrNotFound, false, 1},
		{"store error: retry", old, "succeeded", errors.New("db down"), true, 1},
		{"recorded", old, "pending", nil, false, 1},
		{"unknown status: retry, not recorded", old, "reversed", nil, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeExternalRefunds{err: tc.recErr}
			s := refundAdapter(&fakeRefundUpdater{}, rec)
			obj := dashboardRefund(tc.created, nil)
			obj["status"] = tc.status
			err := s.handleRefundUpdated(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID: "evt_x", EventType: "refund.updated", OccurredAt: time.Now(),
				Payload: map[string]any{"raw": refundRaw(t, obj)},
			})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && errors.Is(err, errs.ErrNotFound) {
				t.Errorf("err = %v: a retry must not wrap ErrNotFound, which HandleWebhook acknowledges", err)
			}
			if len(rec.snaps) != tc.wantCalls {
				t.Errorf("recorder calls = %d, want %d", len(rec.snaps), tc.wantCalls)
			}
		})
	}
}

// TestRefundWebhook_NoPaymentIntentOrNoRecorder: a refund on a charge with no
// PaymentIntent is not of a Hardcount payment (Hardcount charges only through
// PaymentIntents), and without a recorder wired a refund only reaches a note
// that holds its id. Both take the status writer, as before ADR-134.
func TestRefundWebhook_NoPaymentIntentOrNoRecorder(t *testing.T) {
	t.Run("no payment intent", func(t *testing.T) {
		updater := &fakeRefundUpdater{}
		rec := &fakeExternalRefunds{}
		s := refundAdapter(updater, rec)
		obj := dashboardRefund(1000, nil)
		obj["payment_intent"] = nil
		if err := s.handleRefundUpdated(context.Background(), "t1", domain.StripeWebhookEvent{
			StripeEventID: "evt_nopi", EventType: "refund.created", Payload: map[string]any{"raw": refundRaw(t, obj)},
		}); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(rec.snaps) != 0 || len(updater.calls) != 1 {
			t.Errorf("recorder calls %d, status writer calls %d; want 0 and 1", len(rec.snaps), len(updater.calls))
		}
	})
	t.Run("no recorder", func(t *testing.T) {
		updater := &fakeRefundUpdater{}
		s := refundAdapter(updater, nil)
		if err := s.handleRefundUpdated(context.Background(), "t1", domain.StripeWebhookEvent{
			StripeEventID: "evt_norec", EventType: "refund.created",
			Payload: map[string]any{"raw": refundRaw(t, dashboardRefund(1000, nil))},
		}); err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(updater.calls) != 1 {
			t.Errorf("status writer calls = %d, want 1", len(updater.calls))
		}
	})
}

// TestStripeWebhook_RefundCreatedThroughHandler drives a signed
// refund.created with empty metadata through the real handler: it is
// processed, not skipped for missing metadata, and the recorder gets it.
func TestStripeWebhook_RefundCreatedThroughHandler(t *testing.T) {
	webhooks := &capturingWebhookStore{}
	adapter := NewStripe(nil, newMockInvoiceUpdaterH(), webhooks, nil)
	adapter.SetRefundStatusUpdater(&fakeRefundUpdater{})
	rec := &fakeExternalRefunds{}
	adapter.SetExternalRefundRecorder(rec)
	const secret = "whsec_refund_test"
	handler := NewHandler(adapter, &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"hc_spc_rf": {ID: "hc_spc_rf", TenantID: "t1", Livemode: true, WebhookSecret: secret},
	}})
	body, _ := json.Marshal(map[string]any{
		"id": "evt_rf_1", "object": "event", "type": "refund.created", "created": time.Now().Unix(), "livemode": true,
		"data": map[string]any{"object": dashboardRefund(time.Now().Add(-time.Hour).Unix(), map[string]string{})},
	})
	req := httptest.NewRequest("POST", "/stripe/hc_spc_rf", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	w := httptest.NewRecorder()
	handler.Routes().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"processed"`) {
		t.Fatalf("response %d %s, want 200 processed", w.Code, w.Body.String())
	}
	if len(rec.snaps) != 1 || rec.snaps[0].StripeRefundID != "re_dash" {
		t.Fatalf("recorder snapshots = %+v, want one for re_dash", rec.snaps)
	}
	if len(webhooks.ingested) != 1 {
		t.Errorf("ingested %d events, want 1 (the dedup row, written after the record)", len(webhooks.ingested))
	}
}

// TestRefundWebhook_OldUnmatchedRefundOfAHardcountPayment: an old refund
// whose PaymentIntent no invoice holds is acknowledged only when Stripe says
// the PaymentIntent is not Hardcount's. One carrying hardcount_invoice_id (its
// record may still be on its way), or one Stripe cannot be asked about,
// fails the delivery so Stripe sends it again.
func TestRefundWebhook_OldUnmatchedRefundOfAHardcountPayment(t *testing.T) {
	old := time.Now().Add(-48 * time.Hour).Unix()
	for _, tc := range []struct {
		name    string
		client  *mockStripeClient
		wantErr bool
	}{
		{"not Hardcount's", &mockStripeClient{}, false},
		{"a Hardcount payment", &mockStripeClient{piStates: map[string]PaymentIntentResult{"pi_dash": {ID: "pi_dash", InvoiceID: "hc_inv_1"}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStripe(tc.client, newMockInvoiceUpdater(), newMockWebhookStore(), nil)
			s.SetRefundStatusUpdater(&fakeRefundUpdater{})
			s.SetExternalRefundRecorder(&fakeExternalRefunds{err: errs.ErrNotFound})
			err := s.handleRefundUpdated(context.Background(), "t1", domain.StripeWebhookEvent{
				StripeEventID: "evt_um", EventType: "refund.updated", OccurredAt: time.Now(),
				Payload: map[string]any{"raw": refundRaw(t, dashboardRefund(old, nil))},
			})
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, want error %v", err, tc.wantErr)
			}
			if err != nil && errors.Is(err, errs.ErrNotFound) {
				t.Errorf("err = %v: a retry must not wrap ErrNotFound, which HandleWebhook acknowledges", err)
			}
		})
	}
}
