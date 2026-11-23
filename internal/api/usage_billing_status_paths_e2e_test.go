package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Review round 5 on #1390: three ingest paths whose billing_status no other
// test pins. Each flagged or unflagged event is checked against the invoice
// the close builds, as in usage_billing_status_e2e_test.go.

// TestBillingStatus_BatchClassifiesEachEventByItsCustomer: one batch holds
// events of two customers. The first customer has no subscription, the
// second is on a plan that bills the meter. Each event is classified against
// its own customer's billing periods: outside_subscription for the first,
// no status for the second, which the second customer's invoice bills.
//
// Mutation check: classify every event of the batch against the first
// event's customer → results[1] answers outside_subscription.
func TestBillingStatus_BatchClassifiesEachEventByItsCustomer(t *testing.T) {
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	r.newSub("sub", r.planA, lagP0, nil)
	r.must(http.MethodPost, "/v1/customers", lagP0, map[string]any{
		"external_id": "no_sub_cust", "display_name": "No subscription", "email": "nosub@example.test",
	}, http.StatusCreated)

	at := lagP0.AddDate(0, 0, 4)
	noSub := r.labelled("calls", "b-no-sub", at, 3, nil)
	noSub["external_customer_id"] = "no_sub_cust"
	batch := []map[string]any{noSub, r.labelled("calls", "b-billed", at, 5, nil)}
	code, out, _ := r.doH(http.MethodPost, "/v1/usage-events/batch", at.Add(time.Hour), batch)
	if code != http.StatusCreated {
		t.Fatalf("batch: %d %v, want 201", code, out)
	}
	results, _ := out["results"].([]any)
	if len(results) != len(batch) {
		t.Fatalf("batch results = %v, want one per event", out["results"])
	}
	for i, want := range []string{"outside_subscription", ""} {
		res, _ := results[i].(map[string]any)
		if res["status"] != "ingested" {
			t.Errorf("results[%d].status = %v, want ingested", i, res["status"])
		}
		wantStatus(t, fmt.Sprintf("results[%d]", i), res, want)
	}
	if count(out, "outside_subscription") != 1 {
		t.Errorf("batch outside_subscription = %v, want 1", out["outside_subscription"])
	}

	r.run(lagB.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 5 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 5c: the subscribed customer's event only", got)
	}
}

// TestBillingStatus_LiteLLMPreSplitCallIsClassifiedByItsOwnRows: a call whose
// :input a release before the cache-write split stored at the old quantity
// is resent while April is open. Hardcount writes it the old way, [input,
// output], and writes only its :output. The plan prices output tokens and
// no cache writes. The :output is classified as itself: no no_price, and
// April's invoice bills its 500 tokens. In the split mapping the same index
// is a cache write, which no price takes.
//
// Mutation check: classify the written rows by the split mapping's index →
// the :output answers no_price (the 5-minute cache write's row).
func TestBillingStatus_LiteLLMPreSplitCallIsClassifiedByItsOwnRows(t *testing.T) {
	r := newLagRigBare(t, "in_arrears", 0, WithInvoiceGrace(e2eGrace))
	tokens := r.newMeter("tokens", "sum")
	plan := r.planPricing("plan_output", map[string]any{"meter_id": tokens, "dimension_match": map[string]any{"token_type": "output"}})
	r.newSub("llm", plan, lagP0, nil)
	end := lagP0.AddDate(0, 0, 9)
	r.postEvent("/v1/usage-events", "tokens", "ps-open:input", end, end.Add(time.Minute), 101000, http.StatusCreated)

	call := anthropicCall(t, "cache-write-5m-1h", "ps-open", r.custExt, end)
	out := r.must(http.MethodPost, "/v1/integrations/litellm/spend", end.Add(time.Hour), call, http.StatusOK)
	if count(out, "accepted") != 1 || count(out, "deduplicated") != 1 {
		t.Fatalf("resend of a pre-split call = %v, want accepted 1 and deduplicated 1 (written the old way)", out)
	}
	for _, status := range []string{"no_price", "not_on_plan", "outside_subscription", "after_close"} {
		if n := count(out, status); n != 0 {
			t.Errorf("resend of a pre-split call: %s = %d, want 0 (the :output is billed)", status, n)
		}
	}

	r.run(lagB.Add(2 * time.Hour))
	if got := usageBilled(r.invoices()); got != 500 {
		r.logInvoices(r.invoices())
		t.Errorf("usage billed = %dc, want 500c: the :output only", got)
	}
}
