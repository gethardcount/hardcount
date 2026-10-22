package invoice

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The published InvoiceLineItemInput.line_type enum and the type check the
// write paths run must name the same set. The handler decodes into the
// service struct, so the generated enum enforces nothing on its own: if the
// two drift, the spec promises a type the API refuses, or the API takes one
// the spec does not admit.
//
// The probe set is read from the spec: every stored line type (the
// InvoiceLineType read enum) plus every input type, plus one value in
// neither. A line type added to either enum is probed without editing this
// test.
func TestLineTypeInputEnumMatchesBuildLineItem(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Enum       []string `yaml:"enum"`
				Properties map[string]struct {
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	stored := spec.Components.Schemas["InvoiceLineType"].Enum
	input := spec.Components.Schemas["InvoiceLineItemInput"].Properties["line_type"].Enum
	if len(stored) == 0 || len(input) == 0 {
		t.Fatalf("spec enums not found: InvoiceLineType %v, InvoiceLineItemInput.line_type %v", stored, input)
	}
	inSpec := map[string]bool{}
	for _, v := range input {
		inSpec[v] = true
	}
	probes := map[string]bool{"manual": true}
	for _, v := range append(stored, input...) {
		probes[v] = true
	}
	for v := range probes {
		_, err := buildLineItem(AddLineItemInput{
			Description: "Line", LineType: v, Quantity: 1, UnitAmountCents: 100,
		}, "USD")
		if inSpec[v] != (err == nil) {
			t.Errorf("line_type %q: in spec input enum = %v, accepted by buildLineItem = %v (err %v)", v, inSpec[v], err == nil, err)
		}
	}
}
