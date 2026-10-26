package api

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/sagarsuperuser/velox/internal/api/generated"
	"github.com/sagarsuperuser/velox/internal/integrations/litellm"
	"github.com/sagarsuperuser/velox/internal/usage"
)

// specSchema is one component schema of api/openapi.yaml: its required keys,
// its property names and its enum.
type specSchema struct {
	Required   []string            `yaml:"required"`
	Properties map[string]struct{} `yaml:"properties"`
	Enum       []string            `yaml:"enum"`
}

func readSpecSchemas(t *testing.T) map[string]specSchema {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]specSchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	return spec.Components.Schemas
}

// wantWireShape marshals v as the server does and checks it against the
// spec schema: every required key is sent, every key sent is a property,
// and the generated type decodes it with no unknown field.
func wantWireShape(t *testing.T, schemas map[string]specSchema, name string, v any, generatedType any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	schema, ok := schemas[name]
	if !ok {
		t.Fatalf("%s is not a schema of api/openapi.yaml", name)
	}
	for _, k := range schema.Required {
		if _, sent := keys[k]; !sent {
			t.Errorf("%s: required key %q is not sent (%s)", name, k, raw)
		}
	}
	for k := range keys {
		if _, ok := schema.Properties[k]; !ok {
			t.Errorf("%s: key %q is sent but is not a property in api/openapi.yaml", name, k)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(generatedType); err != nil {
		t.Errorf("%s: the generated type does not decode %s: %v", name, raw, err)
	}
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
	enum := slices.Clone(readSpecSchemas(t)["UsageBillingStatus"].Enum)
	slices.Sort(enum)
	if !slices.Equal(sent, enum) {
		t.Errorf("UsageBillingStatus enum = %v, the server sends %v", enum, sent)
	}
	for _, s := range sent {
		if !generated.UsageBillingStatus(s).Valid() {
			t.Errorf("generated UsageBillingStatus does not accept %q: run make gen", s)
		}
	}
}

// TestWireShape_UnbilledCounts: the LiteLLM answer and the held-usage
// release answer send the keys their spec schemas require, and nothing
// else.
func TestWireShape_UnbilledCounts(t *testing.T) {
	schemas := readSpecSchemas(t)
	counts := usage.UnbilledCounts{AfterClose: 1, NoPrice: 2, NotOnPlan: 3, OutsideSubscription: 4}
	var spend generated.LitellmSpendResult
	wantWireShape(t, schemas, "LitellmSpendResult", litellm.SpendResponse{Accepted: 10, UnbilledCounts: counts}, &spend)
	if spend.AfterClose != 1 || spend.NoPrice != 2 || spend.NotOnPlan != 3 || spend.OutsideSubscription != 4 {
		t.Errorf("LiteLLM counts decoded as %+v", spend)
	}
	var release generated.HeldUsageReleaseResult
	wantWireShape(t, schemas, "HeldUsageReleaseResult", usage.ReleaseResult{CustomerID: "cus", Released: 3, NoPrice: 2, NotOnPlan: 1}, &release)
	if release.NoPrice != 2 || release.NotOnPlan != 1 {
		t.Errorf("release counts decoded as %+v", release)
	}
	var result generated.UsageEventBatchEventResult
	wantWireShape(t, schemas, "UsageEventBatchEventResult", usage.BatchEventResult{Status: usage.BatchIngested, BillingStatus: usage.BillingStatusNoPrice}, &result)
	if !result.Status.Valid() || !result.BillingStatus.Valid() {
		t.Errorf("batch event result decoded as %+v", result)
	}
	wantWireShape(t, schemas, "UsageEventBatchEventResult", usage.BatchEventResult{Status: usage.BatchDeduplicated}, &generated.UsageEventBatchEventResult{})
}
