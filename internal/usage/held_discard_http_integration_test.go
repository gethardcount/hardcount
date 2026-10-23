package usage_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// Review finding (PR #1376, round 1): the discard body's `meter_key` was
// renamed `event_name`. A caller still sending `meter_key` (an old script, a
// dashboard tab loaded before the deploy) had it dropped by a lenient
// decode, so the event name read as empty and every held row of the customer
// was discarded, for every meter. A discarded row is never billed, and a
// LiteLLM resend of it is not billed either. The body decodes strictly, so
// an unknown field is a 422 naming it and nothing is discarded.
//
// Mutation-verify: decode the discard body with json.NewDecoder again → the
// `meter_key` POST answers 200 {"discarded":2} and this fails.
func TestHeldDiscard_HTTP_UnknownFieldDiscardsNothing(t *testing.T) {
	f := newHeldFixture(t, "Held Discard Strict")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for _, row := range []struct{ eventName, key string }{
		{"tokens", "strict-tokens"},
		{"images", "strict-images"},
	} {
		if _, err := f.svc.Hold(f.ctx, f.tenantID, usage.HoldInput{
			Source:             domain.HeldUsageSourceLiteLLM,
			ExternalCustomerID: "cus_x",
			EventName:          row.eventName,
			Quantity:           decimal.NewFromInt(10),
			IdempotencyKey:     row.key,
			EventAt:            &at,
			Reason:             domain.HeldReasonCustomerNotFound,
		}); err != nil {
			t.Fatalf("hold %s: %v", row.key, err)
		}
	}

	h := usage.NewHandler(f.svc, nil, nil)
	post := func(body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/held/discard", strings.NewReader(body))
		req = req.WithContext(auth.WithTenantID(f.ctx, f.tenantID))
		rec := httptest.NewRecorder()
		h.Routes().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := post(`{"external_customer_id":"cus_x","meter_key":"tokens"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "meter_key") {
		t.Errorf("discard with the old field meter_key: %d %s, want 422 naming meter_key", code, body)
	}
	for _, key := range []string{"strict-tokens", "strict-images"} {
		if status, _ := f.heldStatus(t, key); status != "held" {
			t.Errorf("after the refused discard, %s is %s, want held", key, status)
		}
	}

	code, body = post(`{"external_customer_id":"cus_x","event_name":"tokens"}`)
	if code != http.StatusOK || !strings.Contains(body, `"discarded":1`) {
		t.Fatalf("discard by event_name: %d %s, want 200 with discarded 1", code, body)
	}
	if status, _ := f.heldStatus(t, "strict-tokens"); status != "discarded" {
		t.Errorf("tokens row is %s, want discarded", status)
	}
	if status, _ := f.heldStatus(t, "strict-images"); status != "held" {
		t.Errorf("images row is %s, want held", status)
	}
}
