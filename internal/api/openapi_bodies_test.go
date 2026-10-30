package api

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/dashmembers"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/dunning"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/paymentmethods"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/recipe"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
	"github.com/gethardcount/hardcount/internal/testclock"
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/user"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// requestBody is the Go value a handler decodes its JSON body into.
type requestBody struct {
	body any
	// undocumented lists fields the type decodes that the spec leaves out on
	// purpose, keyed by dotted JSON path ("lines[].quantity"), each with the
	// reason.
	undocumented map[string]string
}

// serverSet is why a field of a type the handler also returns is not in the
// request schema: the handler decodes the whole type, and the server sets the
// field, so a body that sends it is refused with 422 naming it
// (respond.ReadOnly).
const serverSet = "Read-only. The handler decodes the type it returns; the server sets this field, and a body that sends it is refused with 422."

// propertiesRefused is why a usage event decodes `properties`: it was the old
// name of `dimensions`, and an event that still sends it is refused with 422.
const propertiesRefused = "Not a field: the old name of dimensions. The handler decodes it only to refuse the event with 422."

// readOnly maps each name to serverSet.
func readOnly(names ...string) map[string]string {
	out := make(map[string]string, len(names))
	for _, n := range names {
		out[n] = serverSet
	}
	return out
}

// requestBodies maps every spec operation that has a JSON request body,
// keyed "METHOD /spec/path", to the type its handler decodes. A route whose
// body the server does not define goes in bodiesNotChecked instead.
var requestBodies = map[string]requestBody{
	"POST /v1/customers":                                                {body: customer.CreateInput{}},
	"PATCH /v1/customers/{id}":                                          {body: customer.UpdateInput{}},
	"PUT /v1/customers/{id}/billing-profile":                            {body: domain.CustomerBillingProfile{}, undocumented: readOnly("customer_id", "tenant_id", "profile_status", "created_at", "updated_at")},
	"POST /v1/customers/{customer_id}/payment-methods/send-setup-email": {body: paymentmethods.SendSetupEmailRequest{}},
	"POST /v1/customers/{customer_id}/payment-methods/setup-session":    {body: paymentmethods.SetupSessionRequest{}},
	"POST /v1/meters":                                                   {body: pricing.CreateMeterInput{}},
	"PATCH /v1/meters/{id}":                                             {body: pricing.UpdateMeterInput{}},
	"POST /v1/credit-notes":                                             {body: creditnote.CreateRequest{}},
	"POST /v1/credit-notes/{id}/send":                                   {body: creditnote.SendEmailRequest{}},
	"POST /v1/credits/grant":                                            {body: credit.GrantRequest{}},
	"POST /v1/credits/adjust":                                           {body: credit.AdjustInput{}},
	"POST /v1/webhook-endpoints/endpoints":                              {body: webhook.CreateEndpointInput{}},
	"PATCH /v1/webhook-endpoints/endpoints/{id}":                        {body: webhook.UpdateEndpointInput{}},
	"POST /v1/plans":                                                    {body: pricing.CreatePlanInput{}},
	"PATCH /v1/plans/{id}":                                              {body: pricing.UpdatePlanInput{}},
	"POST /v1/plans/{id}/prices":                                        {body: pricing.PriceInput{}},
	"PATCH /v1/plans/{id}/prices/{price_id}":                            {body: domain.PriceShape{}},
	"POST /v1/price-overrides":                                          {body: pricing.CreateOverrideInput{}},
	"POST /v1/subscriptions":                                            {body: subscription.CreateInput{}},
	"POST /v1/subscriptions/{id}/schedule-cancel":                       {body: subscription.ScheduleCancelInput{}},
	"PUT /v1/subscriptions/{id}/pause-collection":                       {body: subscription.PauseCollectionInput{}},
	"POST /v1/subscriptions/{id}/extend-trial":                          {body: subscription.ExtendTrialInput{}},
	"PUT /v1/subscriptions/{id}/billing-thresholds":                     {body: subscription.BillingThresholdsInput{}},
	"POST /v1/subscriptions/{id}/items":                                 {body: subscription.AddItemInput{}},
	"PATCH /v1/subscriptions/{id}/items/{itemID}":                       {body: subscription.UpdateItemInput{}},
	"POST /v1/usage-events":                                             {body: usage.APIEvent{}, undocumented: map[string]string{"properties": propertiesRefused}},
	"POST /v1/usage-events/batch":                                       {body: []usage.APIEvent{}, undocumented: map[string]string{"[].properties": propertiesRefused}},
	"POST /v1/usage-events/backfill":                                    {body: usage.APIEvent{}, undocumented: map[string]string{"properties": propertiesRefused}},
	"POST /v1/usage-events/held/release":                                {body: usage.ReleaseHeldRequest{}},
	"POST /v1/usage-events/held/discard":                                {body: usage.DiscardHeldRequest{}},
	"PUT /v1/provider-costs":                                            {body: usage.ProviderCostRequest{}},
	"POST /v1/invoices":                                                 {body: invoice.CreateInput{}},
	"POST /v1/invoices/create_preview":                                  {body: billing.CreatePreviewWireRequest{}},
	"POST /v1/invoices/{id}/send":                                       {body: invoice.SendEmailInput{}},
	"POST /v1/invoices/{id}/line-items":                                 {body: invoice.AddLineItemInput{}},
	"POST /v1/invoices/{id}/record-payment":                             {body: invoice.RecordPaymentInput{}},
	"POST /v1/invoices/{id}/refund":                                     {body: invoice.RefundRequest{}},
	"POST /v1/auth/login":                                               {body: user.LoginReq{}},
	"POST /v1/auth/mode":                                                {body: user.SetModeReq{}},
	"POST /v1/auth/password-reset/request":                              {body: user.RequestResetReq{}},
	"POST /v1/auth/password-reset/confirm":                              {body: user.ConfirmResetReq{}},
	"POST /v1/auth/accept-invite":                                       {body: dashmembers.AcceptInviteReq{}},
	"POST /v1/members/invite":                                           {body: dashmembers.InviteReq{}},
	"POST /v1/test-clocks":                                              {body: testclock.CreateInput{}},
	"POST /v1/test-clocks/{id}/advance":                                 {body: testclock.AdvanceInput{}},
	"POST /v1/recipes/{key}/preview":                                    {body: recipe.PreviewRequest{}},
	"POST /v1/recipes/{key}/instantiate":                                {body: recipe.InstantiateRequest{}},
	"POST /v1/dunning/runs/{id}/resolve":                                {body: dunning.ResolveInput{}},
	"POST /v1/dunning/policies":                                         {body: domain.DunningPolicy{}, undocumented: dunningPolicyReadOnly},
	"PATCH /v1/dunning/policies/{id}":                                   {body: domain.DunningPolicy{}, undocumented: dunningPolicyReadOnly},
	"POST /v1/api-keys":                                                 {body: auth.CreateKeyInput{}},
	"POST /v1/api-keys/{id}/rotate":                                     {body: auth.RotateKeyInput{}},
	"PUT /v1/settings":                                                  {body: domain.TenantSettings{}, undocumented: readOnly("tenant_id", "created_at", "updated_at")},
	"POST /v1/settings/stripe":                                          {body: tenantstripe.ConnectInput{}},
	"PATCH /v1/settings/stripe/{mode}/webhook":                          {body: tenantstripe.SetWebhookInput{}},
	"POST /v1/bootstrap":                                                {body: tenant.BootstrapRequest{}},
}

// dunningPolicyReadOnly: set-default moves is_default, and an edit clears
// seeded (ADR-123). Neither is taken from the body.
var dunningPolicyReadOnly = readOnly("id", "tenant_id", "is_default", "seeded", "created_at", "updated_at")

// providerBody is why the inbound webhook bodies are not checked.
const providerBody = "The provider defines this body, not Hardcount. The handler reads the few fields it needs from it."

// bodiesNotChecked lists spec operations with a request body that the check
// skips, each with the reason.
var bodiesNotChecked = map[string]string{
	"POST /v1/webhooks/stripe/{endpoint_id}": providerBody,
	"POST /v1/webhooks/postmark":             providerBody,
	"POST /v1/integrations/litellm/spend":    providerBody,
}

// specBodySchemas reads api/openapi.yaml. It returns the JSON request body
// schema of every operation that has one, keyed "METHOD /spec/path", and the
// components.schemas map that $refs resolve against.
func specBodySchemas(t *testing.T) (map[string]map[string]any, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read %s: %v", specPath, err)
	}
	var doc struct {
		Paths      map[string]map[string]any `yaml:"paths"`
		Components struct {
			Schemas map[string]any `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", specPath, err)
	}
	out := make(map[string]map[string]any)
	for path, item := range doc.Paths {
		for method, op := range item {
			if !openAPIMethods[method] {
				continue
			}
			opMap, _ := op.(map[string]any)
			rb, _ := opMap["requestBody"].(map[string]any)
			if rb == nil {
				continue
			}
			content, _ := rb["content"].(map[string]any)
			media, _ := content["application/json"].(map[string]any)
			schema, _ := media["schema"].(map[string]any)
			if schema == nil {
				t.Errorf("%s %s: requestBody has no application/json schema", strings.ToUpper(method), path)
				continue
			}
			out[strings.ToUpper(method)+" "+path] = schema
		}
	}
	if len(out) == 0 {
		t.Fatalf("read 0 request bodies from %s: the parse is broken, not the spec", specPath)
	}
	return out, doc.Components.Schemas
}

// TestOpenAPISpec_RequestBodies ties each spec request body to the Go type its
// handler decodes. A field the type reads but the spec leaves out is invisible
// to generated clients; a field the spec lists but the type does not have is
// dropped, or refused by a strict route. Both directions fail, at every depth.
// So does a shape mismatch: a Go slice against a schema with no items, or a Go
// struct with fields against a schema with no properties. Value types are not
// compared.
//
// The check starts from the spec, so every operation with a JSON body must be
// in requestBodies or bodiesNotChecked. It cannot see a handler that decodes a
// body the spec does not declare: give such an operation a requestBody.
func TestOpenAPISpec_RequestBodies(t *testing.T) {
	schemas, components := specBodySchemas(t)

	var unmapped, stale, drift []string
	for op := range schemas {
		_, mapped := requestBodies[op]
		_, skipped := bodiesNotChecked[op]
		switch {
		case mapped && skipped:
			stale = append(stale, op+": in both requestBodies and bodiesNotChecked")
		case !mapped && !skipped:
			unmapped = append(unmapped, op)
		}
	}
	for op := range requestBodies {
		if _, ok := schemas[op]; !ok {
			stale = append(stale, op+": requestBodies names it, but the spec has no such operation with a JSON body")
		}
	}
	for op, reason := range bodiesNotChecked {
		if strings.TrimSpace(reason) == "" {
			stale = append(stale, op+": bodiesNotChecked gives no reason")
		}
		if _, ok := schemas[op]; !ok {
			stale = append(stale, op+": bodiesNotChecked names it, but the spec has no such operation with a JSON body")
		}
	}

	for op, rb := range requestBodies {
		schema, ok := schemas[op]
		if !ok {
			continue
		}
		c := bodyComparer{components: components, rb: rb, used: map[string]bool{}}
		c.compare("", reflect.TypeOf(rb.body), schema)
		for k := range rb.undocumented {
			if !c.used[k] {
				stale = append(stale, fmt.Sprintf("%s: undocumented lists %q, which is not a field the type reads, or the spec lists it now", op, k))
			}
		}
		for _, d := range c.diffs {
			drift = append(drift, op+": "+d)
		}
	}

	if len(unmapped) > 0 {
		sort.Strings(unmapped)
		t.Errorf(`%d spec operation(s) have a JSON request body that requestBodies does not map to a Go type:

  %s

Add each to requestBodies with the type its handler decodes. Only a body the
server does not define goes in bodiesNotChecked, with the reason.`, len(unmapped), strings.Join(unmapped, "\n  "))
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("requestBodies or bodiesNotChecked has entries to fix:\n  %s", strings.Join(stale, "\n  "))
	}
	if len(drift) > 0 {
		sort.Strings(drift)
		t.Errorf(`%d request body field(s) differ between api/openapi.yaml and the Go type the handler decodes:

  %s

"not in spec": the server reads the field, so describe it in api/openapi.yaml
and run make gen. "not read": the spec offers a field the server drops, so
remove it from the spec or make the handler read it.`, len(drift), strings.Join(drift, "\n  "))
	}
}

// bodyComparer walks a Go type and a spec schema side by side.
type bodyComparer struct {
	components map[string]any
	rb         requestBody
	used       map[string]bool
	diffs      []string
}

var (
	jsonUnmarshaler = reflect.TypeFor[json.Unmarshaler]()
	timeType        = reflect.TypeFor[time.Time]()
)

// resolve follows $ref and merges allOf, so the result carries the schema's
// own type, properties and items.
func (c *bodyComparer) resolve(schema map[string]any) map[string]any {
	for i := 0; i < 20; i++ {
		ref, _ := schema["$ref"].(string)
		if ref == "" {
			break
		}
		name := strings.TrimPrefix(ref, "#/components/schemas/")
		next, _ := c.components[name].(map[string]any)
		if next == nil {
			c.diffs = append(c.diffs, "unresolvable $ref "+ref)
			return map[string]any{}
		}
		schema = next
	}
	all, _ := schema["allOf"].([]any)
	if len(all) == 0 {
		return schema
	}
	merged := map[string]any{}
	props := map[string]any{}
	for k, v := range schema {
		if k != "allOf" {
			merged[k] = v
		}
	}
	if own, ok := schema["properties"].(map[string]any); ok {
		for k, v := range own {
			props[k] = v
		}
	}
	for _, part := range all {
		p, _ := part.(map[string]any)
		r := c.resolve(p)
		if pp, ok := r["properties"].(map[string]any); ok {
			for k, v := range pp {
				props[k] = v
			}
		}
		if _, ok := merged["type"]; !ok && r["type"] != nil {
			merged["type"] = r["type"]
		}
	}
	merged["properties"] = props
	return merged
}

// opaque reports whether encoding/json decodes t as one value rather than
// field by field, so there is nothing to compare inside it.
func opaque(t reflect.Type) bool {
	return t == timeType || t.Implements(jsonUnmarshaler) || reflect.PointerTo(t).Implements(jsonUnmarshaler)
}

// jsonFields lists the JSON keys encoding/json decodes into struct type t,
// with embedded structs flattened the way encoding/json flattens them.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		ft := f.Type
		if f.Anonymous && name == "" {
			et := ft
			if et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct && !opaque(et) {
				for k, v := range jsonFields(et) {
					if _, dup := out[k]; !dup {
						out[k] = v
					}
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = ft
	}
	return out
}

func (c *bodyComparer) compare(at string, t reflect.Type, schema map[string]any) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	schema = c.resolve(schema)
	if opaque(t) {
		return
	}
	if _, ok := schema["oneOf"]; ok {
		return
	}
	if _, ok := schema["anyOf"]; ok {
		return
	}

	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return // []byte decodes from a base64 string.
		}
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			c.diffs = append(c.diffs, specAt(at)+" is not an array")
			return
		}
		c.compare(at+"[]", t.Elem(), items)
	case reflect.Struct:
		props, hasProps := schema["properties"].(map[string]any)
		fields := jsonFields(t)
		if !hasProps {
			if len(fields) > 0 {
				c.diffs = append(c.diffs, specAt(at)+" lists no properties")
			}
			return
		}
		for name, ft := range fields {
			key := join(at, name)
			ps, inSpec := props[name]
			if !inSpec {
				if _, ok := c.rb.undocumented[key]; ok {
					c.used[key] = true
					continue
				}
				c.diffs = append(c.diffs, key+" not in spec")
				continue
			}
			sm, _ := ps.(map[string]any)
			c.compare(key, ft, sm)
		}
		for name := range props {
			key := join(at, name)
			if _, ok := fields[name]; ok {
				continue
			}
			c.diffs = append(c.diffs, key+" not read")
		}
	}
}

// specAt names the spec schema at path at, for a shape diff.
func specAt(at string) string {
	if at == "" {
		return "the spec body"
	}
	return at + ": the spec"
}

func join(at, name string) string {
	if at == "" {
		return name
	}
	return at + "." + name
}

// TestBodyComparer_ShapeMismatch: when the spec and the Go type disagree on
// shape, the comparer reports it rather than skipping the subtree, at every
// depth.
func TestBodyComparer_ShapeMismatch(t *testing.T) {
	type inner struct {
		A int `json:"a"`
	}
	type outer struct {
		Inner inner `json:"inner"`
	}
	obj := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	cases := []struct {
		name   string
		body   any
		schema map[string]any
		want   []string
	}{
		{
			name:   "array decoded, spec is an object",
			body:   []inner{},
			schema: obj(map[string]any{"events": map[string]any{"type": "array"}}),
			want:   []string{"the spec body is not an array"},
		},
		{
			name: "nested array decoded, spec is an object",
			body: struct {
				L []inner `json:"l"`
			}{},
			schema: obj(map[string]any{"l": map[string]any{"type": "object"}}),
			want:   []string{"l: the spec is not an array"},
		},
		{
			name:   "nested object, spec lists no properties",
			body:   outer{},
			schema: obj(map[string]any{"inner": map[string]any{"type": "object"}}),
			want:   []string{"inner: the spec lists no properties"},
		},
		{
			name:   "root object, spec lists no properties",
			body:   inner{},
			schema: map[string]any{"type": "object"},
			want:   []string{"the spec body lists no properties"},
		},
		{
			name: "shapes match",
			body: outer{},
			schema: obj(map[string]any{"inner": obj(map[string]any{
				"a": map[string]any{"type": "integer"},
			})}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := bodyComparer{used: map[string]bool{}}
			c.compare("", reflect.TypeOf(tc.body), tc.schema)
			if !reflect.DeepEqual(c.diffs, tc.want) {
				t.Errorf("diffs = %q, want %q", c.diffs, tc.want)
			}
		})
	}
}
