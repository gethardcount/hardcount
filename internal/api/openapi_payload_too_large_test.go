package api

import (
	"os"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// payloadTooLargeNotListed are the operations with a JSON body that do not
// answer the shared 413 payload_too_large, each with the reason.
var payloadTooLargeNotListed = map[string]string{
	"POST /v1/usage-events/batch":            "answers its own 413 batch_too_large: send smaller batches",
	"POST /v1/integrations/litellm/spend":    "answers its own 413 batch_too_large over its own 32 MB cap",
	"POST /v1/webhooks/stripe/{endpoint_id}": "has its own 64 KB limit and its own 413",
	"POST /v1/webhooks/postmark":             "answers a body over its own limit with 200 skipped, so Postmark does not resend it",
}

// Every operation with a JSON request body lists the shared PayloadTooLarge
// response (413), because the router caps every body and the strict decoder
// answers a body over the cap with it (respond.BodyTooLarge).
func TestOpenAPISpec_EveryJSONBodyListsPayloadTooLarge(t *testing.T) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]any `yaml:"content"`
			} `yaml:"requestBody"`
			Responses map[string]struct {
				Ref string `yaml:"$ref"`
			} `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	const ref = "#/components/responses/PayloadTooLarge"
	var missing, stale []string
	for path, item := range doc.Paths {
		for method, op := range item {
			if !openAPIMethods[method] {
				continue
			}
			if _, ok := op.RequestBody.Content["application/json"]; !ok {
				continue
			}
			name := strings.ToUpper(method) + " " + path
			_, excepted := payloadTooLargeNotListed[name]
			listed := op.Responses["413"].Ref == ref
			if !listed && !excepted {
				missing = append(missing, name)
			}
			if listed && excepted {
				stale = append(stale, name)
			}
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, m := range missing {
		t.Errorf(`%s takes a JSON body but does not list "413": $ref: %q`, m, ref)
	}
	for _, s := range stale {
		t.Errorf("%s lists PayloadTooLarge but is in payloadTooLargeNotListed; remove it from the list", s)
	}
}
