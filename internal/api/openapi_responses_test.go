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

	"github.com/sagarsuperuser/velox/internal/analytics"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/billing"
	"github.com/sagarsuperuser/velox/internal/credit"
	"github.com/sagarsuperuser/velox/internal/creditnote"
	"github.com/sagarsuperuser/velox/internal/customer"
	"github.com/sagarsuperuser/velox/internal/dashmembers"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/dunning"
	"github.com/sagarsuperuser/velox/internal/hostedinvoice"
	"github.com/sagarsuperuser/velox/internal/integrations/litellm"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/payment"
	"github.com/sagarsuperuser/velox/internal/paymentmethods"
	"github.com/sagarsuperuser/velox/internal/pricing"
	"github.com/sagarsuperuser/velox/internal/recipe"
	"github.com/sagarsuperuser/velox/internal/subscription"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/usage"
	"github.com/sagarsuperuser/velox/internal/user"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// responseTypes maps a components.schemas name to the Go type the server
// encodes for it. The check walks each type's fields into the schemas its
// properties reference, so a schema reached that way needs no entry of its
// own. A schema only a map literal or a list envelope reaches needs one.
var responseTypes = map[string][]any{
	"APIKey":                       {domain.APIKey{}},
	"APIKeyCreated":                {auth.CreateKeyResult{}},
	"APIKeyRotation":               {auth.RotateKeyResult{}},
	"AnalyticsMrrMovementPoint":    {analytics.MRRMovementPoint{}},
	"AnalyticsMrrMovementTotals":   {analytics.MRRMovementTotals{}},
	"AnalyticsOverview":            {analytics.OverviewResponse{}},
	"AnalyticsRevenuePoint":        {analytics.RevenueDataPoint{}},
	"AnalyticsTopMeter":            {analytics.TopMeter{}},
	"AnalyticsUsagePoint":          {analytics.UsagePoint{}},
	"AuditLogEntry":                {domain.AuditEntry{}},
	"BootstrapResult":              {tenant.BootstrapResponse{}},
	"CreditBalance":                {domain.CreditBalance{}},
	"CreditGrants":                 {credit.GrantsResponse{}},
	"CreditLedgerEntry":            {domain.CreditLedgerEntry{}},
	"CreditLiveBalance":            {domain.CreditLiveBalance{}},
	"CreditNote":                   {domain.CreditNote{}},
	"CreditNoteStats":              {creditnote.Stats{}},
	"Customer":                     {domain.Customer{}},
	"CustomerBillingProfile":       {domain.CustomerBillingProfile{}},
	"CustomerPriceOverride":        {domain.CustomerPriceOverride{}},
	"CustomerUsage":                {usage.CustomerUsageResult{}},
	"DunningPolicy":                {domain.DunningPolicy{}},
	"DunningPolicyListItem":        {dunning.PolicyListItem{}},
	"DunningRun":                   {domain.InvoiceDunningRun{}},
	"DunningRunEvent":              {dunning.RunEvent{}},
	"DunningStats":                 {dunning.Stats{}},
	"HeldUsageGroup":               {usage.HeldUsageGroup{}},
	"HeldUsageReleaseResult":       {usage.ReleaseResult{}},
	"HostedInvoice":                {hostedinvoice.HostedInvoicePayload{}},
	"Invoice":                      {domain.Invoice{}},
	"InvoiceDispute":               {domain.InvoiceDispute{}},
	"InvoiceLineItem":              {domain.InvoiceLineItem{}},
	"InvoicePreview":               {billing.PreviewResult{}},
	"InvoiceTimelineEvent":         {invoice.TimelineEvent{}},
	"LitellmSpendResult":           {litellm.SpendResponse{}},
	"MarginReport":                 {usage.MarginReport{}},
	"MemberInvitation":             {dashmembers.InvitationView{}},
	"Meter":                        {pricing.MeterResponse{}},
	"PaymentMethod":                {domain.PaymentMethodView{}},
	"Plan":                         {pricing.PlanResponse{}},
	"PlanDetail":                   {pricing.PlanDetailResponse{}},
	"PlanPrice":                    {domain.PlanPrice{}},
	"PriceWrite":                   {pricing.PriceWriteResponse{}},
	"ProviderCostRate":             {domain.ProviderCostRate{}},
	"PublicPaymentUpdate":          {payment.TokenValidateResponse{}},
	"RecipeDetail":                 {recipe.RecipeDetail{}},
	"RecipeInstance":               {domain.RecipeInstance{}},
	"RecipeListItem":               {recipe.RecipeListItem{}},
	"RecipePreviewResult":          {recipe.PreviewResult{}},
	"SentEmail":                    {customer.SentEmailRow{}},
	"StripeConnection":             {domain.StripeProviderCredentials{}},
	"Subscription":                 {domain.Subscription{}},
	"SubscriptionItem":             {domain.SubscriptionItem{}},
	"SubscriptionItemChangeResult": {subscription.ItemChangeResult{}},
	"SubscriptionTimelineEvent":    {subscription.TimelineEvent{}},
	"Tenant":                       {domain.Tenant{}},
	"TenantSettings":               {tenant.SettingsResponse{}},
	"TestClock":                    {domain.TestClock{}},
	"UsageAggregate":               {usage.Aggregate{}},
	"UsageEvent":                   {domain.UsageEvent{}},
	"UsageEventBatchResult":        {usage.BatchResponse{}},
	"WebhookDeliveryReplayResult":  {webhook.ReplayDeliveryResult{}},
	"WebhookEndpoint":              {domain.WebhookEndpoint{}},
	"WebhookEndpointCreated":       {webhook.CreateEndpointResult{}},
	"WebhookEndpointDelivery":      {webhook.EndpointDeliveryView{}},
	"WebhookEndpointStats":         {webhook.EndpointStats{}},
	"WebhookEvent":                 {domain.WebhookEvent{}},
	"WebhookEventDeliveryAttempt":  {webhook.DeliveryView{}},
	"WebhookReplayResult":          {webhook.ReplayResult{}},
	"WebhookSecretRotation":        {webhook.RotateSecretResult{}},
}

// mapLiteral is why a response schema has no Go type to check.
const mapLiteral = "The handler writes a map literal, so there is no Go type to compare. When you change the handler, keep required to the keys it always writes."

// responsesNotChecked lists response component schemas with properties that
// no Go type is compared against, each with the reason.
var responsesNotChecked = map[string]string{
	"AuditLogFilters":        mapLiteral,
	"CustomerOverview":       mapLiteral,
	"DunningRunDetail":       mapLiteral,
	"InvoiceDisputeList":     mapLiteral,
	"InvoicePaymentTimeline": mapLiteral,
	"InvoiceWithLineItems":   mapLiteral,
	"ReadinessReport":        mapLiteral,
}

// inlineResponseTypes maps a 2xx response whose object schema is written
// inline in the operation, keyed "METHOD /spec/path status", to the Go type
// its handler encodes.
var inlineResponseTypes = map[string]any{
	"POST /v1/customers/{customer_id}/payment-methods/send-setup-email 202": paymentmethods.SendSetupEmailResponse{},
	"POST /v1/customers/{customer_id}/payment-methods/setup-session 201":    paymentmethods.SetupSessionResponse{},
	"GET /v1/public/cost-dashboard/{token} 200":                             usage.CostDashboardProjection{},
	"POST /v1/public/invoices/{token}/checkout 200":                         hostedinvoice.CheckoutResponse{},
	"POST /v1/public/payment-updates/{token}/checkout 201":                  payment.CheckoutResponse{},
	"GET /v1/webhook_events/{id}/deliveries 200":                            webhook.DeliveriesResponse{},
	"POST /v1/auth/login 200":                                               user.LoginResponse{},
	"GET /v1/settings/email 200":                                            emailSettingsStatus{},
}

// inlineNotChecked lists the other inline 2xx object schemas, each with the
// reason. A list page ({data, total}, written by respond.List) needs no entry.
var inlineNotChecked = map[string]string{
	"GET /health 200": mapLiteral,
	"POST /v1/customers/{id}/rotate-cost-dashboard-token 200": mapLiteral,
	"DELETE /v1/webhook-endpoints/endpoints/{id} 200":         mapLiteral,
	"POST /v1/webhooks/stripe/{endpoint_id} 200":              mapLiteral,
	"POST /v1/webhooks/postmark 200":                          mapLiteral,
	"POST /v1/subscriptions/{id}/retry-billing 200":           mapLiteral,
	"GET /v1/subscriptions/{id}/timeline 200":                 mapLiteral,
	"GET /v1/usage-events/held 200":                           mapLiteral,
	"POST /v1/usage-events/held/discard 200":                  mapLiteral,
	"POST /v1/invoices/{id}/send 200":                         mapLiteral,
	"POST /v1/invoices/{id}/resend-setup-link 200":            mapLiteral,
	"POST /v1/credit-notes/{id}/send 200":                     mapLiteral,
	"POST /v1/auth/mode 200":                                  mapLiteral,
	"POST /v1/auth/password-reset/request 200":                mapLiteral,
	"GET /v1/auth/password-reset/check 200":                   mapLiteral,
	"GET /v1/auth/invite/{token} 200":                         mapLiteral,
	"POST /v1/auth/accept-invite 200":                         mapLiteral,
	"GET /v1/members 200":                                     mapLiteral,
	"GET /v1/whoami 200":                                      mapLiteral,
	"POST /v1/billing/run 200":                                mapLiteral,
	"GET /v1/credits/balance/{customer_id} 200":               mapLiteral,
	"DELETE /v1/test-clocks/{id} 200":                         mapLiteral,
	"GET /v1/analytics/revenue-chart 200":                     mapLiteral,
	"GET /v1/analytics/mrr-movement 200":                      mapLiteral,
	"GET /v1/analytics/usage 200":                             mapLiteral,
	"DELETE /v1/dunning/policies/{id} 200":                    mapLiteral,
	"POST /v1/dunning/policies/{id}/set-default 200":          mapLiteral,
	"POST /v1/settings/email/test 200":                        mapLiteral,
	"GET /v1/settings/stripe 200":                             mapLiteral,
}

// specResponses reads api/openapi.yaml. It returns components.schemas, the
// names of the object schemas a 2xx response reaches, directly or through
// other schemas, and each 2xx JSON object schema written inline in its
// operation, keyed "METHOD /spec/path status".
func specResponses(t *testing.T) (map[string]any, map[string]bool, map[string]map[string]any) {
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
	var queue []string
	seen := map[string]bool{}
	inline := map[string]map[string]any{}
	var collect func(v any)
	collect = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				name := strings.TrimPrefix(ref, "#/components/schemas/")
				if !seen[name] {
					seen[name] = true
					queue = append(queue, name)
				}
			}
			for _, e := range x {
				collect(e)
			}
		case []any:
			for _, e := range x {
				collect(e)
			}
		}
	}
	for path, item := range doc.Paths {
		for method, op := range item {
			if !openAPIMethods[method] {
				continue
			}
			opMap, _ := op.(map[string]any)
			responses, _ := opMap["responses"].(map[string]any)
			for code, resp := range responses {
				if !strings.HasPrefix(code, "2") {
					continue
				}
				collect(resp)
				rm, _ := resp.(map[string]any)
				content, _ := rm["content"].(map[string]any)
				media, _ := content["application/json"].(map[string]any)
				schema, _ := media["schema"].(map[string]any)
				if _, ok := schema["properties"]; ok {
					inline[strings.ToUpper(method)+" "+path+" "+code] = schema
				}
			}
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		collect(doc.Components.Schemas[name])
	}
	objects := map[string]bool{}
	for name := range seen {
		s, _ := doc.Components.Schemas[name].(map[string]any)
		if _, ok := s["properties"]; ok {
			objects[name] = true
		}
		if _, ok := s["allOf"]; ok {
			objects[name] = true
		}
	}
	if len(objects) == 0 {
		t.Fatalf("read 0 response schemas from %s: the parse is broken, not the spec", specPath)
	}
	if len(inline) == 0 {
		t.Fatalf("read 0 inline response schemas from %s: the parse is broken, not the spec", specPath)
	}
	return doc.Components.Schemas, objects, inline
}

// TestOpenAPISpec_ResponseRequired ties each response schema's properties and
// required list to the Go type the server encodes, both ways:
//
//   - a key the type always sends (present when every field is zero) must be a
//     property and be in required, so generated clients do not mark it
//     optional;
//   - a required key must be one the type always sends;
//   - a key the type can send must be a property, and a property must be a key
//     the type can send.
//
// It encodes each type twice, once zero and once with every field set, so a
// custom MarshalJSON is checked as it really encodes. It follows each
// property into the schema it describes, inline or by $ref, at every depth.
// Every object schema a 2xx response reaches, by $ref or inline, must be
// checked through some type, or be listed as not checked with the reason.
func TestOpenAPISpec_ResponseRequired(t *testing.T) {
	components, objects, inline := specResponses(t)
	c := &respComparer{components: components, checked: map[string]bool{}, done: map[string]bool{}}

	var stale []string
	for name, types := range responseTypes {
		if _, ok := components[name]; !ok {
			stale = append(stale, name+": responseTypes names it, but the spec has no such schema")
			continue
		}
		if !objects[name] {
			stale = append(stale, name+": responseTypes names it, but no 2xx response reaches it")
		}
		for _, v := range types {
			c.compare(name, reflect.TypeOf(v), map[string]any{"$ref": "#/components/schemas/" + name})
		}
	}
	for name, reason := range responsesNotChecked {
		if strings.TrimSpace(reason) == "" {
			stale = append(stale, name+": responsesNotChecked gives no reason")
		}
		if !objects[name] {
			stale = append(stale, name+": responsesNotChecked names it, but no 2xx response reaches it")
		}
		if c.checked[name] {
			stale = append(stale, name+": in responsesNotChecked, but a Go type is compared against it")
		}
	}
	for op, v := range inlineResponseTypes {
		schema, ok := inline[op]
		if !ok {
			stale = append(stale, op+": inlineResponseTypes names it, but the spec has no such inline response schema")
			continue
		}
		c.compare(op, reflect.TypeOf(v), schema)
	}
	for op, reason := range inlineNotChecked {
		if strings.TrimSpace(reason) == "" {
			stale = append(stale, op+": inlineNotChecked gives no reason")
		}
		if _, ok := inline[op]; !ok {
			stale = append(stale, op+": inlineNotChecked names it, but the spec has no such inline response schema")
		}
		if _, ok := inlineResponseTypes[op]; ok {
			stale = append(stale, op+": in both inlineResponseTypes and inlineNotChecked")
		}
	}
	var unchecked []string
	for op, schema := range inline {
		_, typed := inlineResponseTypes[op]
		_, skipped := inlineNotChecked[op]
		if typed || skipped {
			continue
		}
		props, required, _, _ := c.resolve(schema)
		if len(props) == 2 && props["data"] != nil && props["total"] != nil {
			if !required["data"] || !required["total"] {
				c.diffs = append(c.diffs, op+": a list page always sends data and total, so both are required")
			}
			continue
		}
		unchecked = append(unchecked, op)
	}
	for name := range objects {
		if !c.checked[name] {
			if _, skip := responsesNotChecked[name]; !skip {
				unchecked = append(unchecked, name)
			}
		}
	}

	if len(unchecked) > 0 {
		sort.Strings(unchecked)
		t.Errorf(`%d response schema(s) are not compared with any Go type:

  %s

Add each to responseTypes (a component) or inlineResponseTypes (an operation)
with the type its handler encodes. Only a schema with no Go type behind it
goes in responsesNotChecked or inlineNotChecked, with the reason.`, len(unchecked), strings.Join(unchecked, "\n  "))
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("the response type tables have entries to fix:\n  %s", strings.Join(stale, "\n  "))
	}
	if len(c.diffs) > 0 {
		sort.Strings(c.diffs)
		t.Errorf(`%d response field(s) differ between api/openapi.yaml and the Go type the server encodes:

  %s

"always sent, not required": add the key to the schema's required list.
"required, but omitted": the server leaves the key out when it is empty, so
take it out of required. "sent, not in spec": describe the key. "never sent":
remove the property, or fix responseTypes. Then run make gen.`, len(c.diffs), strings.Join(c.diffs, "\n  "))
	}
}

// respComparer walks a Go type and a response schema side by side.
type respComparer struct {
	components map[string]any
	// checked names each component schema some Go type was compared with.
	checked map[string]bool
	// done holds "schema|type" pairs already compared.
	done  map[string]bool
	diffs []string
}

// resolve follows $ref and merges allOf. It returns the schema's properties,
// its required keys, and the component name when the schema is one.
func (c *respComparer) resolve(schema map[string]any) (props map[string]any, required map[string]bool, name string, ok bool) {
	for i := 0; i < 20; i++ {
		ref, _ := schema["$ref"].(string)
		if ref == "" {
			break
		}
		name = strings.TrimPrefix(ref, "#/components/schemas/")
		next, _ := c.components[name].(map[string]any)
		if next == nil {
			c.diffs = append(c.diffs, "unresolvable $ref "+ref)
			return nil, nil, name, false
		}
		schema = next
	}
	props = map[string]any{}
	required = map[string]bool{}
	_, hasProps := schema["properties"]
	if own, ok := schema["properties"].(map[string]any); ok {
		for k, v := range own {
			props[k] = v
		}
	}
	if req, ok := schema["required"].([]any); ok {
		for _, k := range req {
			if s, ok := k.(string); ok {
				required[s] = true
			}
		}
	}
	all, _ := schema["allOf"].([]any)
	for _, part := range all {
		p, _ := part.(map[string]any)
		pp, pr, partName, pok := c.resolve(p)
		if !pok {
			continue
		}
		if partName != "" {
			c.checked[partName] = true
		}
		hasProps = true
		for k, v := range pp {
			props[k] = v
		}
		for k := range pr {
			required[k] = true
		}
	}
	return props, required, name, hasProps
}

// encodedKeys returns the top-level keys encoding/json writes for v.
func encodedKeys(v reflect.Value) (map[string]bool, error) {
	raw, err := json.Marshal(v.Interface())
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out, nil
}

// fill sets every settable field of v to a non-zero value, so an encode of it
// writes every key the type can write.
func fill(v reflect.Value, depth int) {
	if depth > 6 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), depth+1)
		v.Set(p)
	case reflect.Slice:
		if v.Type() == rawMessageType {
			v.SetBytes([]byte(`"x"`))
			return
		}
		s := reflect.MakeSlice(v.Type(), 1, 1)
		fill(s.Index(0), depth+1)
		v.Set(s)
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return
		}
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		k.SetString("x")
		e := reflect.New(v.Type().Elem()).Elem()
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Interface:
		v.Set(reflect.ValueOf("x"))
	case reflect.Struct:
		if v.Type() == timeType {
			v.Set(reflect.ValueOf(time.Unix(1, 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			fill(v.Field(i), depth+1)
		}
	}
}

var rawMessageType = reflect.TypeFor[json.RawMessage]()

// elemStruct strips pointers, slices and arrays from t and reports whether a
// struct encoded field by field is left.
func elemStruct(t reflect.Type) (reflect.Type, bool) {
	for {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array:
			t = t.Elem()
			continue
		}
		break
	}
	if t.Kind() != reflect.Struct || t == timeType {
		return t, false
	}
	return t, true
}

// itemSchema strips array items from a property schema, to match elemStruct.
func itemSchema(s map[string]any) map[string]any {
	for i := 0; i < 5; i++ {
		items, ok := s["items"].(map[string]any)
		if !ok {
			return s
		}
		s = items
	}
	return s
}

func (c *respComparer) compare(at string, t reflect.Type, schema map[string]any) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	props, required, name, ok := c.resolve(schema)
	if !ok {
		return // not an object schema: an enum, a scalar, a free-form map.
	}
	label := at
	if name != "" {
		label = name
		key := name + "|" + t.PkgPath() + "." + t.Name()
		if c.done[key] {
			return
		}
		c.done[key] = true
		c.checked[name] = true
	}
	if t.Kind() != reflect.Struct {
		c.diffs = append(c.diffs, fmt.Sprintf("%s: the spec describes an object, %s encodes as %s", label, t, t.Kind()))
		return
	}
	who := t.String()

	always, err := encodedKeys(reflect.New(t).Elem())
	if err != nil {
		c.diffs = append(c.diffs, fmt.Sprintf("%s: encode zero %s: %v", label, who, err))
		return
	}
	full := reflect.New(t).Elem()
	fill(full, 0)
	sometimes, err := encodedKeys(full)
	if err != nil {
		c.diffs = append(c.diffs, fmt.Sprintf("%s: encode filled %s: %v", label, who, err))
		return
	}
	for k := range always {
		sometimes[k] = true
	}

	for k := range sometimes {
		_, inSpec := props[k]
		switch {
		case !inSpec && always[k]:
			c.diffs = append(c.diffs, fmt.Sprintf("%s.%s: always sent by %s, not in spec", label, k, who))
		case !inSpec:
			c.diffs = append(c.diffs, fmt.Sprintf("%s.%s: sent by %s when set, not in spec", label, k, who))
		case always[k] && !required[k]:
			c.diffs = append(c.diffs, fmt.Sprintf("%s.%s: always sent by %s, not required", label, k, who))
		}
	}
	for k := range required {
		if !always[k] {
			c.diffs = append(c.diffs, fmt.Sprintf("%s.%s: required, but omitted by %s when empty", label, k, who))
		}
	}
	for k := range props {
		if !sometimes[k] {
			c.diffs = append(c.diffs, fmt.Sprintf("%s.%s: in spec, never sent by %s", label, k, who))
		}
	}

	fields := jsonFields(t)
	for k, ps := range props {
		ft, ok := fields[k]
		if !ok {
			continue
		}
		et, isStruct := elemStruct(ft)
		if !isStruct {
			continue
		}
		sm, _ := ps.(map[string]any)
		c.compare(label+"."+k, et, itemSchema(sm))
	}
}
