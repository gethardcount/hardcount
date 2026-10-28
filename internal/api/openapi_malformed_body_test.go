package api

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A body that is not valid JSON is a 400 on every route Hardcount defines the
// body of (respond.DecodeStrict; TestStrictBodies_EveryJSONRouteRefusesAnUnknownKey
// sends one to each). The reference must say so under 400: a client that
// reads it under 422 expects its Idempotency-Key to be freed, but a 400 is
// kept, so the corrected retry under the same key is refused for 24 hours.
func TestOpenAPISpec_MalformedBodyIsDocumentedAs400(t *testing.T) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Description string `yaml:"description"`
			} `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	var bad []string
	for path, item := range doc.Paths {
		for method, op := range item {
			if !openAPIMethods[method] {
				continue
			}
			for status, resp := range op.Responses {
				if status != "400" && strings.Contains(strings.ToLower(resp.Description), "not valid json") {
					bad = append(bad, strings.ToUpper(method)+" "+path+" "+status)
				}
			}
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s says a body that is not valid JSON gets this status; it is a 400", b)
	}
}
