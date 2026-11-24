package api

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/payment"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// policyFlow is a server and a test-mode key for one account with no dunning
// policy (testutil.CreateTestTenant seeds none).
func policyFlow(t *testing.T, name string) (call func(method, path string, req flowReq) flowResp, key func(livemode bool) string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, name)
	keys := map[bool]string{false: createAPIKeyWithMode(t, db, tenantID, false)}
	key = func(livemode bool) string {
		t.Helper()
		if _, ok := keys[livemode]; !ok {
			keys[livemode] = createAPIKeyWithMode(t, db, tenantID, livemode)
		}
		return keys[livemode]
	}
	call = func(method, path string, req flowReq) flowResp {
		t.Helper()
		if req.bearer == "" {
			req.bearer = keys[false]
		}
		return flowDo(t, ts, method, path, req)
	}
	return call, key
}

// TestE2E_DunningPolicy_GetWithoutAPolicyIsNotFound: an account with no
// policy in the mode answers 404 not_found, and nothing is retried there.
func TestE2E_DunningPolicy_GetWithoutAPolicyIsNotFound(t *testing.T) {
	call, _ := policyFlow(t, "Dunning Policy None Co")
	if res := call("GET", "/v1/dunning/policy", flowReq{}).want(t, 404, "get with no policy"); res.errCode() != "not_found" {
		t.Errorf("code %q, want not_found", res.errCode())
	}
}

// TestE2E_DunningPolicy_PutCreatesThenReplaces: the first PUT creates the
// account's policy and GET answers it; a second PUT replaces every field of
// the same policy. The answer carries no id, and each PUT writes one update
// row to the audit log.
func TestE2E_DunningPolicy_PutCreatesThenReplaces(t *testing.T) {
	call, _ := policyFlow(t, "Dunning Policy Put Co")

	created := call("PUT", "/v1/dunning/policy", flowReq{body: map[string]any{
		"enabled": true, "retry_days": []int{2, 3}, "final_subscription_action": "pause", "final_invoice_action": "none",
	}}).want(t, 200, "create the policy").body
	if _, ok := created["id"]; ok {
		t.Errorf("answer carries id %v; the policy is found by its account and mode", created["id"])
	}
	got := call("GET", "/v1/dunning/policy", flowReq{}).want(t, 200, "get the created policy").body
	if got["enabled"] != true || fmt.Sprint(got["retry_days"]) != "[2 3]" ||
		got["final_subscription_action"] != "pause" || got["final_invoice_action"] != "none" {
		t.Fatalf("created policy = %v, want enabled, [2 3], pause, none", got)
	}

	call("PUT", "/v1/dunning/policy", flowReq{body: map[string]any{
		"retry_days": []int{5}, "final_subscription_action": "cancel", "final_invoice_action": "mark_uncollectible",
	}}).want(t, 200, "replace the policy")
	got = call("GET", "/v1/dunning/policy", flowReq{}).want(t, 200, "get the replaced policy").body
	if got["enabled"] != false || fmt.Sprint(got["retry_days"]) != "[5]" ||
		got["final_subscription_action"] != "cancel" || got["final_invoice_action"] != "mark_uncollectible" {
		t.Fatalf("replaced policy = %v, want every field from the second PUT (enabled left out is false)", got)
	}
	if got["created_at"] != created["created_at"] {
		t.Errorf("created_at %v after the replace, want %v: the same policy, not a new one", got["created_at"], created["created_at"])
	}

	var updates int
	for _, e := range flowList(call("GET", "/v1/audit-log?resource_type=dunning_policy", flowReq{}).want(t, 200, "audit log").body, "data") {
		if flowStr(e, "action") == domain.AuditActionUpdate {
			updates++
		}
	}
	if updates != 2 {
		t.Errorf("audit update rows for the policy = %d, want 2 (one per PUT)", updates)
	}
}

// TestE2E_DunningPolicy_PutRefusesABadBody: a body that fails validation,
// one with a field the policy does not have and one with a field the server
// sets are refused with 422 naming the field, and nothing is written.
func TestE2E_DunningPolicy_PutRefusesABadBody(t *testing.T) {
	call, _ := policyFlow(t, "Dunning Policy Refuse Co")
	for name, tc := range map[string]struct {
		body  map[string]any
		param string
	}{
		"a wait over 30 days":     {map[string]any{"enabled": true, "retry_days": []int{3, 45}}, "retry_days"},
		"an unknown field":        {map[string]any{"enabled": true, "retry_days": []int{3}, "name": "Strict"}, "name"},
		"a field the server sets": {map[string]any{"enabled": true, "retry_days": []int{3}, "id": "hc_dpol_other"}, "id"},
	} {
		res := call("PUT", "/v1/dunning/policy", flowReq{body: tc.body}).want(t, 422, name)
		if got := flowStr(res.body, "error", "param"); got != tc.param {
			t.Errorf("%s: param %q, want %q", name, got, tc.param)
		}
	}
	call("GET", "/v1/dunning/policy", flowReq{}).want(t, 404, "no policy after the refused bodies")
}

// TestE2E_DunningPolicy_ModesHoldSeparatePolicies: test mode and live mode
// each have their own policy. A PUT in one mode leaves the other as it was.
func TestE2E_DunningPolicy_ModesHoldSeparatePolicies(t *testing.T) {
	call, key := policyFlow(t, "Dunning Policy Modes Co")
	test, live := flowReq{bearer: key(false)}, flowReq{bearer: key(true)}

	test.body = map[string]any{"enabled": true, "retry_days": []int{2}}
	call("PUT", "/v1/dunning/policy", test).want(t, 200, "put in test mode")
	call("GET", "/v1/dunning/policy", flowReq{bearer: live.bearer}).want(t, 404, "live mode has none after a test-mode put")

	live.body = map[string]any{"enabled": true, "retry_days": []int{7, 7}}
	call("PUT", "/v1/dunning/policy", live).want(t, 200, "put in live mode")
	if got := call("GET", "/v1/dunning/policy", flowReq{bearer: test.bearer}).want(t, 200, "get in test mode").body; fmt.Sprint(got["retry_days"]) != "[2]" {
		t.Errorf("test-mode retry_days = %v after a live-mode put, want [2]", got["retry_days"])
	}
	if got := call("GET", "/v1/dunning/policy", flowReq{bearer: live.bearer}).want(t, 200, "get in live mode").body; fmt.Sprint(got["retry_days"]) != "[7 7]" {
		t.Errorf("live-mode retry_days = %v, want [7 7]", got["retry_days"])
	}
}

// TestDunning_FailedPaymentStartsARunOnTheAccountPolicy: a declined charge
// starts a run on the account's policy for the mode. Its first retry is the
// policy's first wait after the failure, and the run lists the policy's
// retry count.
func TestDunning_FailedPaymentStartsARunOnTheAccountPolicy(t *testing.T) {
	r := newHardDeclineRig(t, "Failed Payment Run Co")
	runs := dunning.NewPostgresStore(r.db)
	svc := dunning.NewService(runs, nil, nil)
	if _, err := svc.PutPolicy(r.ctx, r.tenantID, domain.DunningPolicy{
		Enabled: true, RetryDays: []int{2, 5},
		FinalSubscriptionAction: domain.SubActionNone, FinalInvoiceAction: domain.InvActionNone,
	}); err != nil {
		t.Fatalf("put policy: %v", err)
	}
	// The charger as router.go wires it, with dunning started inline on a
	// decline.
	charger := payment.NewStripe(r.charges, r.invoices, payment.NewPostgresWebhookStore(r.db),
		&compositePaymentSetupStore{customers: r.customers, pms: r.pmSvc}, svc)
	custID := r.customerWithCard("run", "pm_run")
	inv := r.finalizedInvoice(custID, "INV-RUN-1")
	r.charges.answers = []scriptedCharge{{err: stripeDeclined("pi_run_1", "insufficient_funds", "Your card has insufficient funds.")}}

	hold, err := r.invoices.ClaimChargeForManualCollect(r.ctx, r.tenantID, inv.ID)
	if err != nil || !hold.Held() {
		t.Fatalf("claim: held=%v err=%v", hold.Held(), err)
	}
	before := time.Now().UTC()
	_, _ = charger.ChargeInvoice(r.ctx, r.tenantID, inv, hold, "cus_stripe_run", "pm_run")

	listed, _, err := runs.ListRuns(r.ctx, dunning.RunListFilter{TenantID: r.tenantID, InvoiceID: inv.ID})
	if err != nil || len(listed) != 1 {
		t.Fatalf("runs on the declined invoice = %d (%v), want 1", len(listed), err)
	}
	run := listed[0]
	if run.Reason != string(domain.DunningCausePaymentFailed) || run.MaxRetryAttempts != 2 {
		t.Errorf("run reason %q retries %d, want payment_failed and the policy's 2", run.Reason, run.MaxRetryAttempts)
	}
	if run.NextActionAt == nil {
		t.Fatal("the run has no first retry")
	}
	if wait := run.NextActionAt.Sub(before); wait < 48*time.Hour || wait > 48*time.Hour+time.Minute {
		t.Errorf("first retry %v after the failure, want the policy's 2 days", wait)
	}
}
