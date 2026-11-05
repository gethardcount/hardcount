package api

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/gethardcount/hardcount/internal/integrations/litellm"
	"github.com/gethardcount/hardcount/internal/usage"
)

// wantWireShape marshals v as the server does and holds it to the spec
// schema name with the example checker (openapi_examples_test.go): every
// required key is sent, every key sent is a property, and every value has
// its property's type and enum, in nested objects too. It returns the
// object it checked.
func wantWireShape(t *testing.T, name string, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	// JSON is YAML. Read as YAML, a whole number is an int, as in the spec's
	// examples: the checker's integer type needs one.
	var value any
	if err := yaml.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	doc := specDoc(t)
	if mapAt(doc, "components", "schemas", name) == nil {
		t.Fatalf("%s is not a schema of api/openapi.yaml", name)
	}
	c := &exampleChecker{doc: doc}
	c.check(name, map[string]any{"$ref": "#/components/schemas/" + name}, value)
	for _, e := range c.errs {
		t.Errorf("%s (sent %s)", e, raw)
	}
	obj, _ := value.(map[string]any)
	return obj
}

// TestOpenAPISpec_UsageBillingStatus: the billing_status enum in the spec is
// exactly the statuses the classes answer with, so a status the server
// sends is never rejected by a generated client, and the spec lists none the
// server never sends.
func TestOpenAPISpec_UsageBillingStatus(t *testing.T) {
	var sent []string
	for c := usage.Outside; c <= usage.InOpen; c++ {
		if s := c.BillingStatus(); s != "" {
			sent = append(sent, s)
		}
	}
	slices.Sort(sent)
	var enum []string
	for _, e := range asList(mapAt(specDoc(t), "components", "schemas", "UsageBillingStatus")["enum"]) {
		enum = append(enum, fmt.Sprint(e))
	}
	slices.Sort(enum)
	if !slices.Equal(sent, enum) {
		t.Errorf("UsageBillingStatus enum = %v, the server sends %v", enum, sent)
	}
}

// TestWireShape_UnbilledCounts: the LiteLLM answer, the held-usage release
// answer and a batch event's answer each send the keys their spec schema
// requires and nothing else, with the spec's types and enum values.
func TestWireShape_UnbilledCounts(t *testing.T) {
	counts := usage.UnbilledCounts{AfterClose: 1, NoPrice: 2, NotOnPlan: 3, OutsideSubscription: 4}
	spend := wantWireShape(t, "LitellmSpendResult", litellm.SpendResponse{Accepted: 10, UnbilledCounts: counts})
	for k, want := range map[string]int{"after_close": 1, "no_price": 2, "not_on_plan": 3, "outside_subscription": 4} {
		if spend[k] != want {
			t.Errorf("LiteLLM %s = %v, want %d", k, spend[k], want)
		}
	}
	release := wantWireShape(t, "HeldUsageReleaseResult", usage.ReleaseResult{CustomerID: "cus", Released: 3, NoPrice: 2, NotOnPlan: 1})
	for k, want := range map[string]int{"no_price": 2, "not_on_plan": 1} {
		if release[k] != want {
			t.Errorf("release %s = %v, want %d", k, release[k], want)
		}
	}
	wantWireShape(t, "UsageEventBatchEventResult", usage.BatchEventResult{Status: usage.BatchIngested, BillingStatus: usage.BillingStatusNoPrice})
	wantWireShape(t, "UsageEventBatchEventResult", usage.BatchEventResult{Status: usage.BatchDeduplicated})
}
