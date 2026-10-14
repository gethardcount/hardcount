package pricing

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// TestGetPlan_LiveSubscriptionCount: GET /v1/plans/{id} carries the count
// the price lock uses, so the plan page can disable the controls the server
// would refuse. Zero is sent, not omitted: "no subscriptions" unlocks them.
func TestGetPlan_LiveSubscriptionCount(t *testing.T) {
	for _, live := range []int{0, 2} {
		svc := NewService(newMemStore())
		svc.SetSubscriptionPlanUsageReader(&fakePlanUsage{count: live})
		p, err := svc.CreatePlan(context.Background(), "tnt_test", CreatePlanInput{
			Code: "pro", Name: "Pro", Currency: "USD", BillingInterval: domain.BillingMonthly,
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		rr := doJSON(t, NewHandler(svc).getPlan, http.MethodGet, "/", "", map[string]string{"id": p.ID})
		if rr.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		got, ok := body["live_subscription_count"].(float64)
		if !ok || int(got) != live {
			t.Errorf("live=%d: live_subscription_count = %v (present %v)", live, body["live_subscription_count"], ok)
		}
		if body["code"] != "pro" {
			t.Errorf("plan fields must stay at the top level: %v", body)
		}
	}
}
