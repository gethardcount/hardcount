package api

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The API reference shows the spec's examples as the requests to send and
// the answers to expect, and the website builds its object pages from them.
// An example the server would refuse, or one with a field the schema does
// not have, teaches an integrator the wrong call. These tests hold every
// example to its schema, and every operation to having one.

// specDoc reads api/openapi.yaml as plain maps.
func specDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	return doc
}

// exampleChecker validates example values against OpenAPI 3.0 schemas.
type exampleChecker struct {
	doc  map[string]any
	errs []string
}

func (c *exampleChecker) fail(at, format string, args ...any) {
	c.errs = append(c.errs, at+": "+fmt.Sprintf(format, args...))
}

// deref follows a local $ref ("#/components/...") to its target.
func (c *exampleChecker) deref(s map[string]any) map[string]any {
	for i := 0; i < 20; i++ {
		ref, ok := s["$ref"].(string)
		if !ok {
			return s
		}
		var node any = c.doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, _ := node.(map[string]any)
			node = m[part]
		}
		next, ok := node.(map[string]any)
		if !ok {
			c.fail(ref, "the $ref resolves to nothing")
			return map[string]any{}
		}
		s = next
	}
	return s
}

// object flattens a schema's properties, required list and
// additionalProperties across allOf.
func (c *exampleChecker) object(s map[string]any) (props map[string]any, required []string, additional any) {
	s = c.deref(s)
	props = map[string]any{}
	if p, ok := s["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	for _, r := range asList(s["required"]) {
		required = append(required, fmt.Sprint(r))
	}
	additional = s["additionalProperties"]
	for _, part := range asList(s["allOf"]) {
		pm, _ := part.(map[string]any)
		p, r, a := c.object(pm)
		for k, v := range p {
			props[k] = v
		}
		required = append(required, r...)
		if a != nil {
			additional = a
		}
	}
	return props, required, additional
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

var (
	rfc3339  = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)$`)
	dateOnly = regexp.MustCompile(`^\d{4}-\d\d-\d\d$`)
)

// check reports where value breaks schema.
func (c *exampleChecker) check(at string, schema map[string]any, value any) {
	s := c.deref(schema)
	if value == nil {
		nullable, _ := s["nullable"].(bool)
		if !nullable {
			c.fail(at, "null, but the schema is not nullable")
		}
		return
	}
	if options := asList(s["oneOf"]); len(options) > 0 || len(asList(s["anyOf"])) > 0 {
		if len(options) == 0 {
			options = asList(s["anyOf"])
		}
		for _, o := range options {
			sub := &exampleChecker{doc: c.doc}
			om, _ := o.(map[string]any)
			sub.check(at, om, value)
			if len(sub.errs) == 0 {
				return
			}
		}
		c.fail(at, "%v matches none of the schema's oneOf/anyOf", value)
		return
	}
	if enum := asList(s["enum"]); len(enum) > 0 {
		found := false
		for _, e := range enum {
			if fmt.Sprint(e) == fmt.Sprint(value) {
				found = true
			}
		}
		if !found {
			c.fail(at, "%v is not one of %v", value, enum)
		}
	}
	typ, _ := s["type"].(string)
	if typ == "" {
		if _, ok := s["properties"]; ok {
			typ = "object"
		} else if _, ok := s["allOf"]; ok {
			typ = "object"
		}
	}
	switch typ {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			c.fail(at, "want an object, got %T", value)
			return
		}
		props, required, additional := c.object(s)
		for _, k := range required {
			if _, ok := obj[k]; !ok {
				c.fail(at, "required key %q missing", k)
			}
		}
		keys := make([]string, 0, len(obj))
		for k := range obj {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if p, ok := props[k].(map[string]any); ok {
				c.check(at+"."+k, p, obj[k])
				continue
			}
			switch a := additional.(type) {
			case map[string]any:
				c.check(at+"."+k, a, obj[k])
			case bool:
				if !a {
					c.fail(at, "key %q is not a property of the schema", k)
				}
			default:
				if len(props) > 0 {
					c.fail(at, "key %q is not a property of the schema", k)
				}
			}
		}
	case "array":
		list, ok := value.([]any)
		if !ok {
			c.fail(at, "want an array, got %T", value)
			return
		}
		if n, ok := s["maxItems"].(int); ok && len(list) > n {
			c.fail(at, "%d items, more than maxItems %d", len(list), n)
		}
		if n, ok := s["minItems"].(int); ok && len(list) < n {
			c.fail(at, "%d items, fewer than minItems %d", len(list), n)
		}
		items, _ := s["items"].(map[string]any)
		for i, v := range list {
			c.check(fmt.Sprintf("%s[%d]", at, i), items, v)
		}
	case "string":
		str, ok := value.(string)
		if !ok {
			c.fail(at, "want a string, got %T %v", value, value)
			return
		}
		switch s["format"] {
		case "date-time":
			if !rfc3339.MatchString(str) {
				c.fail(at, "%q is not an RFC 3339 date-time", str)
			}
		case "date":
			if !dateOnly.MatchString(str) {
				c.fail(at, "%q is not a YYYY-MM-DD date", str)
			}
		}
		if n, ok := s["maxLength"].(int); ok && len([]rune(str)) > n {
			c.fail(at, "longer than maxLength %d", n)
		}
		if n, ok := s["minLength"].(int); ok && len([]rune(str)) < n {
			c.fail(at, "shorter than minLength %d", n)
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			c.fail(at, "%q does not match the pattern %s", str, p)
		}
	case "integer":
		n, ok := value.(int)
		if !ok {
			c.fail(at, "want an integer, got %T %v", value, value)
			return
		}
		c.bounds(at, s, float64(n))
	case "number":
		switch n := value.(type) {
		case int:
			c.bounds(at, s, float64(n))
		case float64:
			c.bounds(at, s, n)
		default:
			c.fail(at, "want a number, got %T %v", value, value)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			c.fail(at, "want a boolean, got %T %v", value, value)
		}
	}
}

func (c *exampleChecker) bounds(at string, s map[string]any, v float64) {
	if m, ok := toFloat(s["minimum"]); ok && v < m {
		c.fail(at, "%v is below the minimum %v", v, m)
	}
	if m, ok := toFloat(s["maximum"]); ok && v > m {
		c.fail(at, "%v is above the maximum %v", v, m)
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// schemaExamples checks the schema-level and property-level examples inside
// one schema tree. It does not follow $refs: each component is walked once,
// from components.schemas.
func (c *exampleChecker) schemaExamples(at string, s map[string]any) {
	if s == nil {
		return
	}
	if _, ok := s["$ref"]; ok {
		return
	}
	if ex, ok := s["example"]; ok {
		c.check(at+" (example)", s, ex)
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for k, p := range props {
			pm, _ := p.(map[string]any)
			c.schemaExamples(at+"."+k, pm)
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		c.schemaExamples(at+"[]", items)
	}
	if a, ok := s["additionalProperties"].(map[string]any); ok {
		c.schemaExamples(at+"{}", a)
	}
	for _, kw := range []string{"allOf", "oneOf", "anyOf"} {
		for i, part := range asList(s[kw]) {
			pm, _ := part.(map[string]any)
			c.schemaExamples(fmt.Sprintf("%s/%s[%d]", at, kw, i), pm)
		}
	}
}

// content checks a content map's media-type examples (JSON media types
// only) and the examples inside each inline schema.
func (c *exampleChecker) content(at string, content map[string]any) {
	for mt, m := range content {
		media, _ := m.(map[string]any)
		schema, _ := media["schema"].(map[string]any)
		if ex, ok := media["example"]; ok && strings.Contains(mt, "json") {
			c.check(at+" "+mt+" example", schema, ex)
		}
		c.schemaExamples(at+" "+mt, schema)
	}
}

func mapAt(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m, _ = m[k].(map[string]any)
	}
	return m
}

// exampleOperations returns every operation, keyed "METHOD /path", sorted.
func exampleOperations(doc map[string]any) ([]string, map[string]map[string]any) {
	ops := map[string]map[string]any{}
	var keys []string
	for path, item := range mapAt(doc, "paths") {
		im, _ := item.(map[string]any)
		for method, op := range im {
			if !openAPIMethods[method] {
				continue
			}
			k := strings.ToUpper(method) + " " + path
			ops[k], _ = op.(map[string]any)
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, ops
}

// TestOpenAPISpec_ExamplesMatchSchemas: every example in the spec fits its
// schema. That is schema and property examples, parameter and header
// examples, request and response examples, and the shared responses. An
// object example holds every required key and no key the schema lacks; each
// value has the schema's type, an enum value from the list, null only where
// the schema is nullable, and a date-time in RFC 3339.
func TestOpenAPISpec_ExamplesMatchSchemas(t *testing.T) {
	doc := specDoc(t)
	c := &exampleChecker{doc: doc}
	for name, s := range mapAt(doc, "components", "schemas") {
		sm, _ := s.(map[string]any)
		c.schemaExamples("schemas."+name, sm)
	}
	for name, r := range mapAt(doc, "components", "responses") {
		rm, _ := r.(map[string]any)
		c.content("responses."+name, mapAt(rm, "content"))
	}
	for _, group := range []string{"parameters", "headers"} {
		for name, p := range mapAt(doc, "components", group) {
			pm, _ := p.(map[string]any)
			schema := mapAt(pm, "schema")
			if ex, ok := pm["example"]; ok {
				c.check(group+"."+name, schema, ex)
			}
			c.schemaExamples(group+"."+name, schema)
		}
	}
	keys, ops := exampleOperations(doc)
	for _, k := range keys {
		op := ops[k]
		for _, p := range asList(op["parameters"]) {
			pm, _ := p.(map[string]any)
			pm = c.deref(pm)
			at := fmt.Sprintf("%s parameter %v", k, pm["name"])
			if ex, ok := pm["example"]; ok {
				c.check(at, mapAt(pm, "schema"), ex)
			}
			c.schemaExamples(at, mapAt(pm, "schema"))
		}
		c.content(k+" request", mapAt(op, "requestBody", "content"))
		for code, r := range mapAt(op, "responses") {
			rm, _ := r.(map[string]any)
			if _, ok := rm["$ref"]; ok {
				continue
			}
			c.content(k+" "+code, mapAt(rm, "content"))
		}
	}
	sort.Strings(c.errs)
	for _, e := range c.errs {
		t.Error(e)
	}
}

// TestOpenAPISpec_EveryOperationHasExamples: each JSON request body and each
// 2xx JSON response has an example, so the reference shows a real call and
// a real answer instead of one generated from the types (which sent
// "email": "example" and failed validation).
func TestOpenAPISpec_EveryOperationHasExamples(t *testing.T) {
	doc := specDoc(t)
	keys, ops := exampleOperations(doc)
	for _, k := range keys {
		op := ops[k]
		for mt, m := range mapAt(op, "requestBody", "content") {
			if _, ok := m.(map[string]any)["example"]; !ok && strings.Contains(mt, "json") {
				t.Errorf("%s: the %s request body has no example", k, mt)
			}
		}
		for code, r := range mapAt(op, "responses") {
			if !strings.HasPrefix(code, "2") {
				continue
			}
			rm, _ := r.(map[string]any)
			for mt, m := range mapAt(rm, "content") {
				if _, ok := m.(map[string]any)["example"]; !ok && strings.Contains(mt, "json") {
					t.Errorf("%s %s: the %s response has no example", k, code, mt)
				}
			}
		}
	}
}

// TestOpenAPISpec_ExampleTimesAreQuoted: a date-time in an example is a
// quoted string. JavaScript YAML readers turn an unquoted one into a Date,
// so the generated TypeScript comments and other tools would show it in
// another shape.
func TestOpenAPISpec_ExampleTimesAreQuoted(t *testing.T) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	var bad []string
	var walk func(n *yaml.Node, inExample bool)
	walk = func(n *yaml.Node, inExample bool) {
		if n.Kind == yaml.ScalarNode && inExample && n.Style == 0 && n.ShortTag() == "!!timestamp" {
			bad = append(bad, fmt.Sprintf("line %d: %s", n.Line, n.Value))
		}
		for i, child := range n.Content {
			ex := inExample
			if n.Kind == yaml.MappingNode && i%2 == 1 && n.Content[i-1].Value == "example" {
				ex = true
			}
			walk(child, ex)
		}
	}
	walk(&root, false)
	for _, b := range bad {
		t.Errorf("unquoted date-time in an example, %s: quote it", b)
	}
}

// TestExampleChecker_FindsMistakes: the checker catches each mistake it is
// for, so a green TestOpenAPISpec_ExamplesMatchSchemas means something.
func TestExampleChecker_FindsMistakes(t *testing.T) {
	doc := map[string]any{"components": map[string]any{"schemas": map[string]any{
		"Thing": map[string]any{
			"type":     "object",
			"required": []any{"id", "status"},
			"properties": map[string]any{
				"id":         map[string]any{"type": "string"},
				"status":     map[string]any{"type": "string", "enum": []any{"open", "paid"}},
				"amount":     map[string]any{"type": "integer"},
				"at":         map[string]any{"type": "string", "format": "date-time"},
				"note":       map[string]any{"type": "string", "nullable": true},
				"tags":       map[string]any{"type": "array", "maxItems": 1, "items": map[string]any{"type": "string"}},
				"parent":     map[string]any{"$ref": "#/components/schemas/Parent"},
				"free_form":  map[string]any{"type": "object"},
				"label_vals": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
			},
		},
		"Parent": map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": map[string]any{"type": "string"}},
		},
	}}}
	thing := map[string]any{"$ref": "#/components/schemas/Thing"}
	good := map[string]any{
		"id": "hc_x", "status": "open", "amount": 5, "at": "2026-10-01T00:00:00Z", "note": nil,
		"tags": []any{"a"}, "parent": map[string]any{"id": "p"}, "free_form": map[string]any{"any": 1},
		"label_vals": map[string]any{"model": "gpt"},
	}
	c := &exampleChecker{doc: doc}
	c.check("good", thing, good)
	if len(c.errs) != 0 {
		t.Fatalf("a valid example was refused: %v", c.errs)
	}
	for name, bad := range map[string]map[string]any{
		"missing required": {"id": "hc_x"},
		"unknown key":      {"id": "hc_x", "status": "open", "colour": "red"},
		"wrong type":       {"id": 5, "status": "open"},
		"not in enum":      {"id": "hc_x", "status": "closed"},
		"bad date-time":    {"id": "hc_x", "status": "open", "at": "2026-10-01"},
		"null not allowed": {"id": "hc_x", "status": "open", "amount": nil},
		"too many items":   {"id": "hc_x", "status": "open", "tags": []any{"a", "b"}},
		"nested unknown":   {"id": "hc_x", "status": "open", "parent": map[string]any{"name": "p"}},
		"map value type":   {"id": "hc_x", "status": "open", "label_vals": map[string]any{"model": 4}},
		"float as integer": {"id": "hc_x", "status": "open", "amount": 1.5},
	} {
		c := &exampleChecker{doc: doc}
		c.check(name, thing, bad)
		if len(c.errs) == 0 {
			t.Errorf("%s: the checker accepted it", name)
		}
	}
}
