package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// probeKey is the key every probe adds to an otherwise empty body. No
// request type declares it.
const probeKey = "b9_unknown_field"

// strictRig is a bootstrapped account on a real router and database, with
// the three credentials the routes take: the owner's dashboard session, a
// test-mode secret key and a platform key.
type strictRig struct {
	t        *testing.T
	ts       *httptest.Server
	db       *postgres.DB
	tenantID string
	cookie   *http.Cookie
	secret   string
	platform string
}

func newStrictRig(t *testing.T) *strictRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Setenv("VELOX_BOOTSTRAP_TOKEN", "strict-bodies-bootstrap")
	t.Setenv("DASHBOARD_BASE_URL", "http://dash.strict-bodies.test")
	t.Setenv("SMTP_HOST", "smtp.strict-bodies.invalid")
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)

	const email, password = "owner@strict-bodies.test", "strict-bodies-pw-12"
	boot := flowBootstrap(t, ts, "strict-bodies-bootstrap", email, password)
	tenantID := flowStr(boot, "tenant", "id")
	return &strictRig{
		t: t, ts: ts, db: db, tenantID: tenantID,
		cookie:   login(t, ts, email, password),
		secret:   flowStr(boot, "secret_key_test"),
		platform: createPlatformKey(t, db, tenantID),
	}
}

// call sends the request with the credential the route takes: none for the
// public sign-in routes and bootstrap, the platform key for /v1/tenants, the
// dashboard session for everything else (the dashboard is the main client,
// and /v1/members and /v1/auth/mode take nothing else).
func (r *strictRig) call(method, path string, body any) flowResp {
	r.t.Helper()
	req := flowReq{body: body}
	switch {
	case strings.HasPrefix(path, "/v1/bootstrap"),
		strings.HasPrefix(path, "/v1/auth/") && path != "/v1/auth/mode":
	case strings.HasPrefix(path, "/v1/tenants"):
		req.bearer = r.platform
	default:
		req.cookie = r.cookie
	}
	return flowDo(r.t, r.ts, method, path, req)
}

// createPlatformKey inserts a platform key for the tenant, the way
// createAPIKeyWithMode inserts a secret key. Only a platform key may call
// /v1/tenants.
func createPlatformKey(t *testing.T, db *postgres.DB, tenantID string) string {
	t.Helper()
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	secretHex := hex.EncodeToString(secret)
	rawKey := "vlx_platform_live_" + secretHex
	hash := sha256.Sum256([]byte(rawKey))
	tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(context.Background(), `SELECT set_config('app.livemode', 'on', true)`); err != nil {
		t.Fatalf("set app.livemode: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		`INSERT INTO api_keys (id, key_prefix, key_hash, key_type, name, tenant_id)
		 VALUES ($1, $2, $3, 'platform', 'Strict bodies platform key', $4)`,
		"vlx_key_strict_"+secretHex[:8], "vlx_platform_live_"+secretHex[:12], hex.EncodeToString(hash[:]), tenantID); err != nil {
		t.Fatalf("insert platform key: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return rawKey
}

// probePath fills a chi pattern's parameters. A handler reads its body
// before it looks anything up, so made-up ids reach the decode. {mode} must
// be a real mode for the Stripe settings routes to reach their handler.
func probePath(pattern string) string {
	return pathParam.ReplaceAllStringFunc(pattern, func(p string) string {
		if p == "{mode}" {
			return "test"
		}
		return "vlx_b9_probe"
	})
}

// TestStrictBodies_EveryJSONRouteRefusesAnUnknownKey walks the live router.
// Every route whose spec operation has a JSON request body gets a body with
// one key no request type declares, and must answer 422 with param naming
// that key. A handler that drops unknown keys (encoding/json's default)
// answers something else, and fails here.
//
// The only routes not probed are the ones whose body Velox does not define
// (bodiesNotChecked: the Stripe and Postmark webhooks and the LiteLLM
// callback; their sender adds fields when it likes) and routes the spec
// gives no body. A handler that reads a body the spec does not declare
// fails TestNoRequestBodyDecodeOutsideRespond instead.
func TestStrictBodies_EveryJSONRouteRefusesAnUnknownKey(t *testing.T) {
	r := newStrictRig(t)

	schemas, _ := specBodySchemas(t)
	bodyOps := make(map[string]string, len(schemas)) // routeShape -> spec op
	for op := range schemas {
		method, path, _ := strings.Cut(op, " ")
		bodyOps[routeShape(method, path)] = op
	}

	var keys []routeKey
	for k := range walkMutatingRoutes(t) {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Pattern != keys[j].Pattern {
			return keys[i].Pattern < keys[j].Pattern
		}
		return keys[i].Method < keys[j].Method
	})

	probed, lenient := 0, 0
	for _, k := range keys {
		op, hasBody := bodyOps[routeShape(k.Method, k.Pattern)]
		if !hasBody {
			continue
		}
		if _, ok := bodiesNotChecked[op]; ok {
			lenient++
			continue
		}
		rb, ok := requestBodies[op]
		if !ok {
			t.Errorf("%s: the spec gives it a JSON body, but requestBodies does not name its Go type", op)
			continue
		}
		var body any = map[string]any{probeKey: 1}
		if reflect.TypeOf(rb.body).Kind() == reflect.Slice {
			body = []any{body}
		}
		probed++
		res := r.call(k.Method, probePath(k.Pattern), body)
		if res.status != http.StatusUnprocessableEntity || flowStr(res.body, "error", "param") != probeKey {
			t.Errorf("%s %s with an unknown key: %d %s, want 422 with param %q", k.Method, k.Pattern, res.status, res.raw, probeKey)
		}
	}
	if probed < 50 {
		t.Fatalf("probed %d routes: the walk or the spec read is broken", probed)
	}
	if lenient != len(bodiesNotChecked) {
		t.Errorf("skipped %d provider-body routes, bodiesNotChecked lists %d", lenient, len(bodiesNotChecked))
	}
	t.Logf("probed %d JSON-body routes; %d provider-body routes stay lenient", probed, lenient)
}

// TestStrictBodies_PatchRefusesFieldsItCannotChange: every PATCH route names
// the field a caller tried to change but cannot, with 422, and changes
// nothing. Before, the customer, dunning policy and webhook endpoint PATCH
// routes answered 200 and dropped the field; the Stripe webhook-secret PATCH
// answered whatever the rest of the request deserved.
func TestStrictBodies_PatchRefusesFieldsItCannotChange(t *testing.T) {
	r := newStrictRig(t)
	create := func(method, path string, body map[string]any) map[string]any {
		t.Helper()
		res := r.call(method, path, body)
		if res.status/100 != 2 {
			t.Fatalf("%s %s: %d %s", method, path, res.status, res.raw)
		}
		return res.body
	}

	cus := flowStr(create("POST", "/v1/customers", map[string]any{
		"external_id": "strict-cus", "display_name": "Strict Customer", "email": "ap@strict-bodies.test",
	}), "id")
	meter := flowStr(create("POST", "/v1/meters", map[string]any{
		"event_name": "strict_calls", "name": "Strict calls", "unit": "call", "aggregation": "sum",
	}), "id")
	plan := create("POST", "/v1/plans", map[string]any{
		"code": "strict-plan", "name": "Strict Plan", "currency": "USD", "billing_interval": "monthly",
		"base_amount_cents": 1000, "base_bill_timing": "in_arrears",
		"prices": []map[string]any{{"meter_id": meter, "mode": "per_unit", "unit_amount_cents": "10"}},
	})
	planID := flowStr(plan, "id")
	priceID := flowStr(flowList(plan, "prices")[0], "id")
	sub := create("POST", "/v1/subscriptions", map[string]any{
		"code": "strict-sub", "display_name": "Strict Sub", "customer_id": cus,
		"items": []map[string]any{{"plan_id": planID, "quantity": 1}}, "billing_time": "anniversary",
	})
	subID := flowStr(sub, "id")
	itemID := flowStr(flowList(sub, "items")[0], "id")
	policy := flowStr(create("POST", "/v1/dunning/policies", map[string]any{
		"name": "Strict policy", "enabled": true, "retry_days": []int{3, 4},
		"final_subscription_action": "pause", "final_invoice_action": "none",
	}), "id")
	hook := flowStr(create("POST", "/v1/webhook-endpoints/endpoints", map[string]any{
		"url": "https://example.com/strict-bodies",
	})["endpoint"].(map[string]any), "id")

	cases := []struct {
		path  string
		field string
		value any
		get   string // read back to show nothing changed; "" when there is no single GET
	}{
		{"/v1/customers/" + cus, "external_id", "strict-cus-renamed", "/v1/customers/" + cus},
		{"/v1/customers/" + cus, "display_nam", "typo", "/v1/customers/" + cus},
		{"/v1/dunning/policies/" + policy, "is_default", true, "/v1/dunning/policies/" + policy},
		{"/v1/dunning/policies/" + policy, "id", "vlx_dpol_other", "/v1/dunning/policies/" + policy},
		{"/v1/meters/" + meter, "event_name", "strict_renamed", "/v1/meters/" + meter},
		{"/v1/plans/" + planID, "currency", "EUR", "/v1/plans/" + planID},
		{"/v1/plans/" + planID + "/prices/" + priceID, "meter_id", "vlx_mtr_other", "/v1/plans/" + planID},
		{"/v1/settings/stripe/test/webhook", "livemode", true, ""},
		{"/v1/subscriptions/" + subID + "/items/" + itemID, "plan_id", "vlx_pln_other", "/v1/subscriptions/" + subID},
		{"/v1/webhook-endpoints/endpoints/" + hook, "secret", "whsec_strict", ""},
	}
	for _, tc := range cases {
		var before flowResp
		if tc.get != "" {
			before = r.call("GET", tc.get, nil).want(t, 200, "read "+tc.get)
		}
		res := r.call("PATCH", tc.path, map[string]any{tc.field: tc.value})
		if res.status != http.StatusUnprocessableEntity || flowStr(res.body, "error", "param") != tc.field {
			t.Errorf("PATCH %s {%s}: %d %s, want 422 with param %q", tc.path, tc.field, res.status, res.raw, tc.field)
		}
		if tc.get != "" {
			after := r.call("GET", tc.get, nil).want(t, 200, "read "+tc.get)
			if !reflect.DeepEqual(before.body, after.body) {
				t.Errorf("PATCH %s {%s} changed the resource:\nbefore %s\nafter  %s", tc.path, tc.field, before.raw, after.raw)
			}
		}
	}
}

// TestSetupSession_StripeNotConnected: with no Stripe account connected for
// the mode, asking for a card-setup link is the caller's state to fix, not a
// server fault. Both operator routes that mint one answer 409
// stripe_not_connected with a sentence that says what to do, for a customer
// Velox has never sent to Stripe (the path that answered 500).
func TestSetupSession_StripeNotConnected(t *testing.T) {
	r := newStrictRig(t)
	cus := flowStr(r.call("POST", "/v1/customers", map[string]any{
		"external_id": "no-stripe-cus", "display_name": "No Stripe", "email": "ap@no-stripe.test",
	}).want(t, 201, "create customer").body, "id")

	for _, path := range []string{
		"/v1/customers/" + cus + "/payment-methods/setup-session",
		"/v1/customers/" + cus + "/payment-methods/send-setup-email",
	} {
		res := r.call("POST", path, map[string]any{})
		if res.status != http.StatusConflict || res.errCode() != "stripe_not_connected" {
			t.Errorf("POST %s without Stripe: %d %s, want 409 stripe_not_connected", path, res.status, res.raw)
			continue
		}
		if msg := flowStr(res.body, "error", "message"); msg != "Stripe is not connected for this mode. Connect it under Settings, then try again." {
			t.Errorf("POST %s without Stripe: message %q, want it to say Stripe is not connected and where to connect it", path, msg)
		}
	}
}
