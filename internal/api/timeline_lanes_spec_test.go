package api

import (
	"os"
	"regexp"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestOpenAPISpec_TimelineDegradedLanes: every lane the payment-timeline
// handler can name in `degraded` is a value of InvoicePaymentTimeline.degraded
// in api/openapi.yaml. The enum is closed, so a lane missing from it makes the
// generated Go and TypeScript types reject a response the server really
// sends. The lanes are read from the handler's degrade("…") calls, so a new
// lane fails here until the spec lists it.
func TestOpenAPISpec_TimelineDegradedLanes(t *testing.T) {
	src, err := os.ReadFile("../invoice/handler.go")
	if err != nil {
		t.Fatalf("read the timeline handler: %v", err)
	}
	lanes := map[string]bool{}
	for _, m := range regexp.MustCompile(`degrade\("([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		lanes[m[1]] = true
	}
	if len(lanes) < 5 {
		t.Fatalf("found %d degrade(\"…\") lanes in internal/invoice/handler.go, want at least 5: the pattern no longer matches the handler", len(lanes))
	}

	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Items struct {
						Enum []string `yaml:"enum"`
					} `yaml:"items"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	enum := map[string]bool{}
	for _, v := range spec.Components.Schemas["InvoicePaymentTimeline"].Properties["degraded"].Items.Enum {
		enum[v] = true
	}
	if len(enum) == 0 {
		t.Fatal("InvoicePaymentTimeline.degraded has no enum in api/openapi.yaml")
	}
	for lane := range lanes {
		if !enum[lane] {
			t.Errorf("the payment timeline can report degraded lane %q, but InvoicePaymentTimeline.degraded in api/openapi.yaml does not list it; add it and run make gen", lane)
		}
	}
}
