package api

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"
)

// A field marked `x-show-once: true` in api/openapi.yaml carries a secret
// shown only in that answer, so the idempotency layer must never keep the
// answer for a replay. Every operation whose 2xx answer has such a field is
// either outside mw.Idempotency or marked showsSecretOnce() in routeTable, and
// every route so marked answers with such a field. A new show-once field
// therefore fails here until its route is covered.
func TestOpenAPISpec_ShowOnceSecretsAreNeverKept(t *testing.T) {
	marked := specShowOnceOperations(t)
	if len(marked) == 0 {
		t.Fatal("found 0 operations with an x-show-once field: the walk is broken, not the spec")
	}

	kept := make(map[string]bool) // routeShape → mw.Idempotency keeps its answer
	srv := newTestServer()
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, mws ...func(http.Handler) http.Handler) error {
		behind := false
		for _, m := range mws {
			if isIdempotencyMiddleware(m) {
				behind = true
				break
			}
		}
		kept[routeShape(method, route)] = behind && !routeTable[routeKey{Method: method, Pattern: canonicalRoute(route)}].showOnce
		return nil
	})
	if err != nil {
		t.Fatalf("chi.Walk over the live router: %v", err)
	}

	var keptSecrets []string
	for shape, path := range marked {
		if kept[shape] {
			keptSecrets = append(keptSecrets, shape+" ("+path+")")
		}
	}
	if len(keptSecrets) > 0 {
		sort.Strings(keptSecrets)
		t.Errorf("%d operation(s) answer with an x-show-once field but the idempotency layer keeps the answer. Mark them .showsSecretOnce() in routeTable (route_table.go):\n  %s",
			len(keptSecrets), strings.Join(keptSecrets, "\n  "))
	}
	for key, entry := range routeTable {
		if _, ok := marked[routeShape(key.Method, key.Pattern)]; entry.showOnce && !ok {
			t.Errorf("routeTable marks %s %s showsSecretOnce(), but its answer in api/openapi.yaml has no x-show-once field. Remove the mark, or mark the field that carries the secret", key.Method, key.Pattern)
		}
	}
}

// specShowOnceOperations returns, keyed by routeShape, the operations whose
// 2xx answer schema has a property marked `x-show-once: true`, following
// $ref, allOf, oneOf, anyOf, items and properties.
func specShowOnceOperations(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	responses, _ := components["responses"].(map[string]any)

	var hasShowOnce func(node any, seen map[string]bool) bool
	hasShowOnce = func(node any, seen map[string]bool) bool {
		n, ok := node.(map[string]any)
		if !ok {
			return false
		}
		if ref, ok := n["$ref"].(string); ok {
			name := ref[strings.LastIndex(ref, "/")+1:]
			if seen[name] {
				return false
			}
			seen[name] = true
			return hasShowOnce(schemas[name], seen)
		}
		if v, ok := n["x-show-once"].(bool); ok && v {
			return true
		}
		for _, k := range []string{"allOf", "oneOf", "anyOf"} {
			list, _ := n[k].([]any)
			for _, sub := range list {
				if hasShowOnce(sub, seen) {
					return true
				}
			}
		}
		if hasShowOnce(n["items"], seen) {
			return true
		}
		props, _ := n["properties"].(map[string]any)
		for _, p := range props {
			if hasShowOnce(p, seen) {
				return true
			}
		}
		return false
	}

	out := make(map[string]string)
	paths, _ := doc["paths"].(map[string]any)
	for path, item := range paths {
		ops, _ := item.(map[string]any)
		for method, op := range ops {
			if !openAPIMethods[method] {
				continue
			}
			resps, _ := op.(map[string]any)["responses"].(map[string]any)
			for code, resp := range resps {
				if !strings.HasPrefix(code, "2") {
					continue
				}
				r, _ := resp.(map[string]any)
				if ref, ok := r["$ref"].(string); ok {
					r, _ = responses[ref[strings.LastIndex(ref, "/")+1:]].(map[string]any)
				}
				content, _ := r["content"].(map[string]any)
				for _, media := range content {
					m, _ := media.(map[string]any)
					if hasShowOnce(m["schema"], map[string]bool{}) {
						out[routeShape(strings.ToUpper(method), path)] = path
					}
				}
			}
		}
	}
	return out
}
