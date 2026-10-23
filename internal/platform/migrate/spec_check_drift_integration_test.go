package migrate

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestSpecEnumsMatchColumnChecks: each stored enum below has a CHECK in the
// database and an enum in the published spec, and the two list the same
// values. The spec listed values no code could write (subscription
// archived, tax_status failed, dunning state paused and five event types)
// and missed one the CHECK allows (tax_error_code provider_not_configured).
func TestSpecEnumsMatchColumnChecks(t *testing.T) {
	dsn, db := scratchDB(t, "velox_test_deadc_b_migrate_spec")
	ctx := context.Background()
	if err := up(ctx, dsn, migrationFiles()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	raw, err := os.ReadFile("../../../api/openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	type schema struct {
		Enum       []string `yaml:"enum"`
		Properties map[string]struct {
			Enum []string `yaml:"enum"`
			Ref  string   `yaml:"$ref"`
		} `yaml:"properties"`
	}
	var spec struct {
		Components struct {
			Schemas map[string]schema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}
	specEnum := func(name, prop string) []string {
		s := spec.Components.Schemas[name]
		if prop == "" {
			return s.Enum
		}
		p := s.Properties[prop]
		if p.Ref != "" {
			return spec.Components.Schemas[strings.TrimPrefix(p.Ref, "#/components/schemas/")].Enum
		}
		return p.Enum
	}

	literal := regexp.MustCompile(`'([^']*)'`)
	for _, c := range []struct{ constraint, schema, prop string }{
		{"subscriptions_status_check", "Subscription", "status"},
		{"plans_status_check", "Plan", "status"},
		{"invoices_tax_status_check", "Invoice", "tax_status"},
		{"invoices_tax_error_code_check", "Invoice", "tax_error_code"},
		{"invoice_line_items_line_type_check", "InvoiceLineType", ""},
		{"invoice_dunning_runs_state_check", "DunningRun", "state"},
		{"invoice_dunning_runs_resolution_check", "DunningRun", "resolution"},
		{"invoice_dunning_events_event_type_check", "DunningRunEvent", "event_type"},
		{"invoice_dunning_events_state_check", "DunningRunEvent", "state"},
	} {
		var def string
		if err := db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = $1`, c.constraint).Scan(&def); err != nil {
			t.Errorf("%s: %v", c.constraint, err)
			continue
		}
		var check []string
		for _, m := range literal.FindAllStringSubmatch(def, -1) {
			check = append(check, m[1])
		}
		enum := append([]string{}, specEnum(c.schema, c.prop)...)
		sort.Strings(check)
		sort.Strings(enum)
		if strings.Join(check, ",") != strings.Join(enum, ",") {
			t.Errorf("%s allows %v, but the spec's %s.%s enum lists %v", c.constraint, check, c.schema, c.prop, enum)
		}
	}
}
