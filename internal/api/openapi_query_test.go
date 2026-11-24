package api

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// specParameter is one entry of an operation's or a path item's parameters:
// a parameter, or a $ref to one under components.parameters.
type specParameter struct {
	Ref  string `yaml:"$ref"`
	Name string `yaml:"name"`
	In   string `yaml:"in"`
}

// specQueryParams reads api/openapi.yaml. It returns every operation keyed by
// routeShape, mapped to the sorted names of its query parameters, its path
// item's included and each $ref resolved.
func specQueryParams(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc struct {
		Paths      map[string]map[string]yaml.Node `yaml:"paths"`
		Components struct {
			Parameters map[string]specParameter `yaml:"parameters"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	const refPrefix = "#/components/parameters/"
	queryNames := func(where string, params []specParameter) []string {
		var out []string
		for _, p := range params {
			if p.Ref != "" {
				shared, ok := doc.Components.Parameters[strings.TrimPrefix(p.Ref, refPrefix)]
				if !strings.HasPrefix(p.Ref, refPrefix) || !ok {
					t.Fatalf("%s: parameter $ref %q resolves to nothing under components.parameters", where, p.Ref)
				}
				p = shared
			}
			if p.In == "query" {
				out = append(out, p.Name)
			}
		}
		return out
	}
	out := make(map[string][]string)
	for path, item := range doc.Paths {
		var onPath []specParameter
		if n, ok := item["parameters"]; ok {
			if err := n.Decode(&onPath); err != nil {
				t.Fatalf("decode parameters of %s: %v", path, err)
			}
		}
		for key, node := range item {
			if !openAPIMethods[key] {
				continue
			}
			var op struct {
				Parameters []specParameter `yaml:"parameters"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatalf("decode %s %s: %v", key, path, err)
			}
			where := strings.ToUpper(key) + " " + path
			names := slices.Concat(queryNames(where, onPath), queryNames(where, op.Parameters))
			slices.Sort(names)
			out[routeShape(strings.ToUpper(key), path)] = names
		}
	}
	if len(out) == 0 {
		t.Fatalf("read 0 operations from %s: the parse is broken, not the spec", specPath)
	}
	return out
}

// TestOpenAPISpec_QueryParametersMatchTheTable ties the query parameters
// api/openapi.yaml documents to the ones the route table declares
// (withQuery, withPaging), which are the only ones refuseUnknownQuery lets
// through. For every operation the two sets are equal, and every route that
// declares a name has an operation. A name the table declares and the spec
// lacks is one no generated client can send; one the spec documents and the
// table lacks is refused when a client sends it.
func TestOpenAPISpec_QueryParametersMatchTheTable(t *testing.T) {
	spec := specQueryParams(t)
	paths := specOperations(t)
	byShape := make(map[string]routeKey, len(routeTable))
	for k := range routeTable {
		byShape[routeShape(k.Method, k.Pattern)] = k
	}
	for shape, documented := range spec {
		k, ok := byShape[shape]
		if !ok {
			continue // TestOpenAPISpec_MatchesRouter reports an operation with no route
		}
		declared := slices.Sorted(slices.Values(routeTable[k].query))
		if !slices.Equal(documented, declared) {
			t.Errorf("%s %s: api/openapi.yaml documents query parameters %v, the route table declares %v. Make them the names the handler reads.",
				k.Method, paths[shape], documented, declared)
		}
	}
	for k, d := range routeTable {
		if _, ok := spec[routeShape(k.Method, k.Pattern)]; len(d.query) > 0 && !ok {
			t.Errorf("%s %s declares query parameters %v but has no operation in api/openapi.yaml", k.Method, k.Pattern, d.query)
		}
	}
}
