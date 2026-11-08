package payment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stripe/stripe-go/v82"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
)

// The restore catch-up reads events from Stripe's List Events API and hands
// them to the same parse and HandleWebhook as a webhook delivery (ADR-135).
// These tests check that an event read through the API parses to the same
// event as its webhook body: the same fields, and the same data.object bytes
// in the payload that the handlers read again.

const fidelityTenant = "hc_ten_fidelity"

// fidelityObjects holds one data.object for each event type processEvent
// handles, in the shape Stripe renders it, with fields Hardcount does not
// read as well.
var fidelityObjects = map[string]string{
	"payment_intent.succeeded":      `{"id":"pi_1","object":"payment_intent","amount":5000,"amount_received":5000,"currency":"usd","customer":"cus_1","status":"succeeded","latest_charge":"ch_1","created":1792378800,"livemode":true,"payment_method":"pm_1","metadata":{"hardcount_tenant_id":"hc_ten_fidelity","hardcount_invoice_id":"hc_inv_1","hardcount_customer_id":"hc_cus_1","hardcount_anchor_at":"2026-10-19T03:00:00Z"}}`,
	"payment_intent.payment_failed": `{"id":"pi_2","object":"payment_intent","amount":5000,"amount_received":0,"currency":"usd","customer":"cus_1","status":"requires_payment_method","created":1792378800,"livemode":true,"last_payment_error":{"type":"card_error","code":"card_declined","decline_code":"insufficient_funds","message":"Your card has insufficient funds.","payment_method":{"id":"pm_1","object":"payment_method","type":"card"}},"metadata":{"hardcount_tenant_id":"hc_ten_fidelity","hardcount_invoice_id":"hc_inv_1"}}`,
	"checkout.session.completed":    `{"id":"cs_1","object":"checkout.session","mode":"payment","status":"complete","payment_status":"paid","customer":"cus_1","payment_intent":"pi_3","amount_total":5000,"currency":"usd","created":1792378800,"livemode":true,"metadata":{"hardcount_tenant_id":"hc_ten_fidelity","hardcount_invoice_id":"hc_inv_1","hardcount_customer_id":"hc_cus_1","hardcount_purpose":"hosted_invoice_pay"}}`,
	"setup_intent.succeeded":        `{"id":"seti_1","object":"setup_intent","customer":"cus_1","payment_method":"pm_9","status":"succeeded","usage":"off_session","created":1792378800,"livemode":true,"metadata":{}}`,
	"setup_intent.setup_failed":     `{"id":"seti_2","object":"setup_intent","customer":"cus_1","payment_method":null,"status":"requires_payment_method","created":1792378800,"livemode":true,"last_setup_error":{"type":"card_error","code":"card_declined","message":"Your card was declined."},"metadata":{}}`,
	"refund.created":                `{"id":"re_1","object":"refund","amount":1000,"currency":"usd","charge":"ch_1","payment_intent":"pi_1","status":"succeeded","reason":"requested_by_customer","created":1792378800,"metadata":{}}`,
	"refund.updated":                `{"id":"re_2","object":"refund","amount":1000,"currency":"usd","charge":"ch_1","payment_intent":"pi_1","status":"succeeded","reason":null,"created":1792378800,"metadata":{"hardcount_cn_id":"hc_cn_1"}}`,
	"refund.failed":                 `{"id":"re_3","object":"refund","amount":1000,"currency":"usd","charge":"ch_1","payment_intent":"pi_1","status":"failed","failure_reason":"expired_or_canceled_card","reason":null,"created":1792378800,"metadata":{}}`,
	"charge.refund.updated":         `{"id":"re_4","object":"refund","amount":1000,"currency":"usd","charge":{"id":"ch_1","object":"charge"},"payment_intent":{"id":"pi_1","object":"payment_intent"},"status":"pending","reason":null,"created":1792378800,"metadata":{}}`,
	"charge.dispute.created":        `{"id":"du_1","object":"dispute","amount":5000,"currency":"usd","charge":"ch_1","payment_intent":"pi_1","reason":"fraudulent","status":"needs_response","created":1792378800,"livemode":true,"evidence_details":{"due_by":1793000000,"has_evidence":false},"metadata":{}}`,
	"charge.dispute.updated":        `{"id":"du_1","object":"dispute","amount":5000,"currency":"usd","charge":"ch_1","payment_intent":"pi_1","reason":"fraudulent","status":"under_review","created":1792378800,"livemode":true,"evidence_details":{"due_by":1793000000,"has_evidence":true},"metadata":{}}`,
	"charge.dispute.closed":         `{"id":"du_1","object":"dispute","amount":5000,"currency":"usd","charge":"ch_1","payment_intent":"pi_1","reason":"fraudulent","status":"won","created":1792378800,"livemode":true,"evidence_details":{"due_by":1793000000,"has_evidence":true},"metadata":{}}`,
}

// webhookBody is a snapshot event as a webhook delivery carries it.
func webhookBody(id, eventType, object string) []byte {
	return []byte(fmt.Sprintf(`{"id":%q,"object":"event","api_version":"2025-03-31.basil","created":1792378860,"data":{"object":%s},"livemode":true,"pending_webhooks":1,"request":{"id":null,"idempotency_key":null},"type":%q}`,
		id, object, eventType))
}

// listedEvent decodes a body the way stripe-go decodes each item of a List
// Events page.
func listedEvent(t *testing.T, body []byte) *stripe.Event {
	t.Helper()
	var ev stripe.Event
	if err := json.Unmarshal(body, &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	return &ev
}

// dataObject returns data.object of an event body in the payload, compacted.
func dataObject(t *testing.T, ev domain.StripeWebhookEvent) string {
	t.Helper()
	raw, ok := ev.Payload["raw"].(string)
	if !ok {
		t.Fatalf("event %s has no raw payload", ev.StripeEventID)
	}
	var body struct {
		Data struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var out bytes.Buffer
	if err := json.Compact(&out, body.Data.Object); err != nil {
		t.Fatalf("compact data.object: %v", err)
	}
	return out.String()
}

func withoutPayload(ev domain.StripeWebhookEvent) domain.StripeWebhookEvent {
	ev.Payload = nil
	return ev
}

func TestCatchUpParse_EveryHandledTypeMatchesItsWebhookBody(t *testing.T) {
	var types []string
	for typ := range fidelityObjects {
		types = append(types, typ)
	}
	sort.Strings(types)
	if !reflect.DeepEqual(types, sortedCopy(catchUpEventTypes)) {
		t.Fatalf("the fixtures cover %v, but the catch-up lists %v: every type processEvent handles needs a fixture", types, sortedCopy(catchUpEventTypes))
	}

	for i, typ := range types {
		t.Run(typ, func(t *testing.T) {
			body := webhookBody(fmt.Sprintf("evt_%d", i), typ, fidelityObjects[typ])
			fromWebhook, _, err := parseStripeEvent(body, fidelityTenant, true)
			if err != nil {
				t.Fatalf("webhook body: %v", err)
			}
			listed, err := catchUpEventBody(listedEvent(t, body))
			if err != nil {
				t.Fatalf("rebuild listed event: %v", err)
			}
			fromList, _, err := parseStripeEvent(listed, fidelityTenant, true)
			if err != nil {
				t.Fatalf("listed event: %v", err)
			}
			if !reflect.DeepEqual(withoutPayload(fromList), withoutPayload(fromWebhook)) {
				t.Errorf("listed event parses to\n%+v\nwant the webhook's\n%+v", withoutPayload(fromList), withoutPayload(fromWebhook))
			}
			if got, want := dataObject(t, fromList), dataObject(t, fromWebhook); got != want {
				t.Errorf("data.object differs:\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// The tenant guards run on the catch-up's events too: on a Stripe account
// that several tenants share, an event for another tenant is not this
// tenant's, and a PaymentIntent without Hardcount's metadata is the tenant's
// own charge.
func TestCatchUpParse_GuardsMatchTheWebhook(t *testing.T) {
	cases := []struct {
		name, eventType, object string
		want                    error
	}{
		{"another tenant", "payment_intent.succeeded",
			`{"id":"pi_x","object":"payment_intent","amount":100,"currency":"usd","status":"succeeded","metadata":{"hardcount_tenant_id":"hc_ten_other","hardcount_invoice_id":"hc_inv_x"}}`,
			errEventOtherTenant},
		{"not Hardcount's", "payment_intent.succeeded",
			`{"id":"pi_y","object":"payment_intent","amount":100,"currency":"usd","status":"succeeded","metadata":{}}`,
			errEventNotHardcount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := webhookBody("evt_guard", tc.eventType, tc.object)
			_, _, webhookErr := parseStripeEvent(body, fidelityTenant, true)
			listed, err := catchUpEventBody(listedEvent(t, body))
			if err != nil {
				t.Fatal(err)
			}
			_, _, listErr := parseStripeEvent(listed, fidelityTenant, true)
			if !errors.Is(webhookErr, tc.want) || !errors.Is(listErr, tc.want) {
				t.Fatalf("webhook: %v, catch-up: %v, want %v for both", webhookErr, listErr, tc.want)
			}
		})
	}
}

// A signed body sent to the webhook endpoint, and the same event read by the
// catch-up, reach HandleWebhook as the same event.
func TestCatchUpParse_SignedDeliveryMatchesListedEvent(t *testing.T) {
	const secret = "whsec_fidelity"
	store := &capturingWebhookStore{}
	h := NewHandler(NewStripe(nil, newMockInvoiceUpdaterH(), store, nil), &stubResolver{rows: map[string]tenantstripe.EndpointLookup{
		"hc_spc_fid": {ID: "hc_spc_fid", TenantID: fidelityTenant, Livemode: true, WebhookSecret: secret},
	}})
	body := webhookBody("evt_signed", "setup_intent.setup_failed", fidelityObjects["setup_intent.setup_failed"])
	req := httptest.NewRequest(http.MethodPost, "/stripe/hc_spc_fid", strings.NewReader(string(body)))
	req.Header.Set("Stripe-Signature", signStripePayload(body, secret))
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || len(store.ingested) != 1 {
		t.Fatalf("delivery answered %d with %d events recorded (%s)", rec.Code, len(store.ingested), rec.Body.String())
	}

	listed, err := catchUpEventBody(listedEvent(t, body))
	if err != nil {
		t.Fatal(err)
	}
	fromList, _, err := parseStripeEvent(listed, fidelityTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(withoutPayload(fromList), withoutPayload(store.ingested[0])) {
		t.Errorf("catch-up event\n%+v\nwant the delivered one\n%+v", withoutPayload(fromList), withoutPayload(store.ingested[0]))
	}
	if got, want := dataObject(t, fromList), dataObject(t, store.ingested[0]); got != want {
		t.Errorf("data.object differs:\n%s\nwant\n%s", got, want)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
