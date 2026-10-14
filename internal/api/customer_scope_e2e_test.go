package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenantstripe"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestE2E_CustomerIDIsTenantAndModeScoped pins that the three operator
// endpoints taking a customer id in the request body — credit grant, credit
// adjust, subscription create — resolve that customer inside the caller's
// tenant and mode before writing anything.
//
// The foreign key from the credit ledger and from subscriptions to customers
// is on the id alone, and foreign-key checks do not apply row-level security,
// so the database accepts a row that points at any existing customer. The
// service's own read is the only thing that scopes the id.
//
// Driven through the real router so it also fails if the router stops wiring
// the customer reader into either service. The same-tenant same-mode control
// keeps a check that refuses everything from passing.
func TestE2E_CustomerIDIsTenantAndModeScoped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantA := testutil.CreateTestTenant(t, db, "Customer Scope A")
	tenantB := testutil.CreateTestTenant(t, db, "Customer Scope B")
	aLive := "Bearer " + createAPIKeyWithMode(t, db, tenantA, true)
	aTest := "Bearer " + createAPIKeyWithMode(t, db, tenantA, false)
	bLive := "Bearer " + createAPIKeyWithMode(t, db, tenantB, true)

	createCustomer := func(auth, externalID string) string {
		t.Helper()
		resp := doPost(t, ts, "/v1/customers", auth, map[string]any{
			"external_id": externalID, "display_name": externalID,
		})
		assertStatus(t, resp, 201)
		return readJSON(t, resp)["id"].(string)
	}
	own := createCustomer(aLive, "cus_scope_own")
	otherTenant := createCustomer(bLive, "cus_scope_other_tenant")
	otherMode := createCustomer(aTest, "cus_scope_other_mode")

	resp := doPost(t, ts, "/v1/plans", aLive, map[string]any{
		"code": "scope-plan", "name": "Scope Plan", "currency": "USD",
		"billing_interval": "monthly", "base_amount_cents": 1000,
	})
	assertStatus(t, resp, 201)
	planID := readJSON(t, resp)["id"].(string)

	// rowsFor counts rows referencing the customer across every tenant and
	// mode, so a row the caller could not read back is still counted.
	rowsFor := func(table, customerID string) int {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin bypass tx: %v", err)
		}
		defer postgres.Rollback(tx)
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM `+table+` WHERE customer_id = $1`, customerID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	endpoints := []struct {
		name, path, table string
		body              func(customerID, suffix string) map[string]any
	}{
		{"credit grant", "/v1/credits/grant", "customer_credit_ledger", func(id, _ string) map[string]any {
			return map[string]any{"customer_id": id, "amount_cents": 500, "description": "scope check"}
		}},
		{"credit adjust", "/v1/credits/adjust", "customer_credit_ledger", func(id, _ string) map[string]any {
			return map[string]any{"customer_id": id, "amount_cents": 500, "description": "scope check"}
		}},
		{"subscription create", "/v1/subscriptions", "subscriptions", func(id, suffix string) map[string]any {
			return map[string]any{
				"code": "scope-sub-" + suffix, "display_name": "Scope Sub", "customer_id": id,
				"items": []map[string]any{{"plan_id": planID}},
			}
		}},
	}
	refused := []struct{ name, suffix, customerID string }{
		{"another tenant's customer", "tenant", otherTenant},
		{"same tenant, other mode", "mode", otherMode},
		{"unknown id", "unknown", "vlx_cus_does_not_exist"},
	}

	for _, ep := range endpoints {
		for _, c := range refused {
			t.Run(ep.name+"/"+c.name, func(t *testing.T) {
				before := rowsFor(ep.table, c.customerID)
				resp := doPost(t, ts, ep.path, aLive, ep.body(c.customerID, c.suffix))
				body := readJSON(t, resp)
				if resp.StatusCode != 404 {
					t.Errorf("status = %d, want 404; body: %v", resp.StatusCode, body)
				}
				// One message for all three cases: a customer hidden by
				// isolation reads the same as one that does not exist.
				if errBody, _ := body["error"].(map[string]any); errBody["message"] != "customer not found" {
					t.Errorf("error message = %v, want \"customer not found\"", errBody["message"])
				}
				if wrote := rowsFor(ep.table, c.customerID) - before; wrote != 0 {
					t.Errorf("wrote %d %s row(s) for a customer outside the caller's tenant and mode, want 0", wrote, ep.table)
				}
			})
		}
		t.Run(ep.name+"/own customer", func(t *testing.T) {
			before := rowsFor(ep.table, own)
			resp := doPost(t, ts, ep.path, aLive, ep.body(own, "own"))
			assertStatus(t, resp, 201)
			if wrote := rowsFor(ep.table, own) - before; wrote != 1 {
				t.Errorf("wrote %d %s row(s) for the caller's own customer, want 1", wrote, ep.table)
			}
		})
	}
}

// scopeRefs is one set of objects a request can name: the caller's own, or a
// set the caller must not be able to reference.
type scopeRefs struct {
	cus, ext, rule, meter, meterKey, plan, plan2, plan3 string
	sub, item, activeSub, activeItem                    string
	draftInv, finalInv, creditNote, dunningRun          string
	policy, override, hook, rate, apiKey                string
}

func (r scopeRefs) ids() []string {
	return []string{
		r.cus, r.ext, r.rule, r.meter, r.meterKey, r.plan, r.plan2, r.plan3, r.sub, r.item, r.activeSub, r.activeItem,
		r.draftInv, r.finalInv, r.creditNote, r.dunningRun, r.policy, r.override, r.hook, r.rate, r.apiKey,
	}
}

// TestE2E_ReferencedIDsAreTenantAndModeScoped widens the test above from
// three endpoints to every operator endpoint that writes a row naming another
// object: by a customer, subscription, invoice, plan, meter, rating rule,
// dunning policy or test clock id in the body, or by the id in the path.
//
// Each request is sent three times, naming another tenant's object, the same
// tenant's object in the other mode, and an id that does not exist. It must
// be refused, and afterwards no table may hold more rows than before, no row
// anywhere may newly mention the named id, and no invoice number may be used
// up. Where a control is listed, the same request naming the caller's own
// object must succeed, so a check that refuses everything does not pass.
//
// Most rows were already safe and are here to stay that way. The ones fixed
// with this test are marked "fixed".
func TestE2E_ReferencedIDsAreTenantAndModeScoped(t *testing.T) {
	db := testutil.SetupTestDB(t)
	srv := NewServer(db, clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	tenantA := testutil.CreateTestTenant(t, db, "Reference Scope A")
	tenantB := testutil.CreateTestTenant(t, db, "Reference Scope B")
	keyALive := createAPIKeyWithMode(t, db, tenantA, true)
	keyATest := createAPIKeyWithMode(t, db, tenantA, false)
	keyBLive := createAPIKeyWithMode(t, db, tenantB, true)
	aLive, aTest, bLive := "Bearer "+keyALive, "Bearer "+keyATest, "Bearer "+keyBLive
	bTest := "Bearer " + createAPIKeyWithMode(t, db, tenantB, false)

	call := func(method, path, auth string, body any) (int, map[string]any) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", auth)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	errMessage := func(body map[string]any) string {
		e, _ := body["error"].(map[string]any)
		m, _ := e["message"].(string)
		return m
	}
	bypass := func(fn func(q func(query string, args ...any) *sqlRow)) {
		t.Helper()
		tx, err := db.BeginTx(context.Background(), postgres.TxBypass, "")
		if err != nil {
			t.Fatalf("begin bypass tx: %v", err)
		}
		defer postgres.Rollback(tx)
		fn(func(query string, args ...any) *sqlRow { return tx.QueryRow(query, args...) })
	}

	line := []map[string]any{{"description": "work", "quantity": 1, "unit_amount_cents": 1000}}
	seed := func(auth, key, tag string) scopeRefs {
		t.Helper()
		create := func(method, path string, body any) map[string]any {
			t.Helper()
			code, out := call(method, path, auth, body)
			if code != 200 && code != 201 {
				t.Fatalf("seed %s: %s %s = %d %v", tag, method, path, code, out)
			}
			return out
		}
		id := func(m map[string]any) string { return m["id"].(string) }
		plan := func(code string, cents int, meters []string) string {
			return id(create("POST", "/v1/plans", map[string]any{
				"code": code, "name": code, "currency": "USD", "billing_interval": "monthly",
				"base_amount_cents": cents, "meter_ids": meters,
			}))
		}
		r := scopeRefs{ext: tag + "_cus", meterKey: tag + "_meter"}
		r.rule = id(create("POST", "/v1/rating-rules", map[string]any{
			"rule_key": tag + "_rule", "name": "rule", "mode": "flat", "currency": "USD", "flat_amount_cents": 100,
		}))
		r.meter = id(create("POST", "/v1/meters", map[string]any{
			"key": r.meterKey, "name": "meter", "unit": "unit", "aggregation": "sum", "rating_rule_version_id": r.rule,
		}))
		r.plan = plan(tag+"-plan", 1000, []string{r.meter})
		r.plan2 = plan(tag+"-plan2", 2000, nil)
		r.plan3 = plan(tag+"-plan3", 3000, nil)
		r.cus = id(create("POST", "/v1/customers", map[string]any{"external_id": r.ext, "display_name": tag}))
		sub := create("POST", "/v1/subscriptions", map[string]any{
			"code": tag + "-sub", "display_name": "sub", "customer_id": r.cus, "items": []map[string]any{{"plan_id": r.plan}},
		})
		r.sub, r.item = id(sub), id(sub["items"].([]any)[0].(map[string]any))
		cus2 := id(create("POST", "/v1/customers", map[string]any{"external_id": tag + "_cus2", "display_name": tag}))
		sub = create("POST", "/v1/subscriptions", map[string]any{
			"code": tag + "-active", "display_name": "sub", "customer_id": cus2, "items": []map[string]any{{"plan_id": r.plan}}, "start_now": true,
		})
		r.activeSub, r.activeItem = id(sub), id(sub["items"].([]any)[0].(map[string]any))
		// The policy comes first: finalizing an unpaid invoice starts a
		// dunning run only when the tenant has one.
		r.policy = id(create("POST", "/v1/dunning/policies", map[string]any{
			"name": tag + " policy", "enabled": true, "max_retry_attempts": 2, "retry_schedule": []string{"72h", "120h"},
		}))
		r.draftInv = id(create("POST", "/v1/invoices", map[string]any{"customer_id": r.cus, "line_items": line}))
		r.finalInv = id(create("POST", "/v1/invoices", map[string]any{"customer_id": cus2, "line_items": line}))
		create("POST", "/v1/invoices/"+r.finalInv+"/finalize", nil)
		r.creditNote = id(create("POST", "/v1/credit-notes", map[string]any{
			"invoice_id": r.finalInv, "reason": "seed", "lines": []map[string]any{{"description": "x", "quantity": 1, "unit_amount_cents": 100}},
		}))
		bypass(func(q func(string, ...any) *sqlRow) {
			if err := q(`SELECT id FROM invoice_dunning_runs WHERE invoice_id = $1`, r.finalInv).Scan(&r.dunningRun); err != nil {
				t.Fatalf("seed %s: dunning run: %v", tag, err)
			}
		})
		r.override = id(create("POST", "/v1/price-overrides", map[string]any{
			"customer_id": cus2, "rating_rule_version_id": r.rule, "mode": "flat", "flat_amount_cents": 50,
		}))
		r.hook = id(create("POST", "/v1/webhook-endpoints/endpoints", map[string]any{"url": "https://example.com/" + tag})["endpoint"].(map[string]any))
		r.rate = id(create("PUT", "/v1/provider-costs", map[string]any{
			"provider": "openai", "model": "model-" + tag, "token_type": "input", "cost_per_token": "0.000001",
		}))
		// createAPIKeyWithMode derives the key id from the key itself.
		mode := strings.SplitN(strings.TrimPrefix(key, "vlx_secret_"), "_", 2)
		r.apiKey = "vlx_key_e2e_" + mode[1][:8] + "_" + mode[0]
		return r
	}
	own := seed(aLive, keyALive, "own")
	foreign := []struct {
		name string
		refs scopeRefs
	}{
		{"another tenant's", seed(bLive, keyBLive, "tenant")},
		{"same tenant, other mode", seed(aTest, keyATest, "mode")},
		{"unknown id", scopeRefs{
			cus: "vlx_cus_missing", ext: "missing_cus", rule: "vlx_rrv_missing", meter: "vlx_mtr_missing", meterKey: "missing_meter",
			plan: "vlx_pln_missing", plan2: "vlx_pln_missing2", plan3: "vlx_pln_missing3",
			sub: "vlx_sub_missing", item: "vlx_si_missing", activeSub: "vlx_sub_missing2", activeItem: "vlx_si_missing2",
			draftInv: "vlx_inv_missing", finalInv: "vlx_inv_missing2", creditNote: "vlx_cn_missing", dunningRun: "vlx_drun_missing",
			policy: "vlx_dpol_missing", override: "vlx_cpo_missing", hook: "vlx_whe_missing", rate: "vlx_pcr_missing", apiKey: "vlx_key_missing",
		}},
	}

	// snapshot records, for every table, how many rows it holds and how many
	// of them mention one of ids anywhere in the row, across all tenants and
	// modes, plus tenant A's next live invoice number. Equal snapshots before
	// and after a request mean it inserted nothing, deleted nothing, pointed
	// no existing row at one of ids, and used up no invoice number.
	var tables []string
	bypass(func(q func(string, ...any) *sqlRow) {
		var list string
		if err := q(`SELECT string_agg(c.relname, ',' ORDER BY c.relname) FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition`).Scan(&list); err != nil {
			t.Fatalf("list tables: %v", err)
		}
		tables = strings.Split(list, ",")
	})
	snapshot := func(ids []string) string {
		t.Helper()
		var b strings.Builder
		bypass(func(q func(string, ...any) *sqlRow) {
			pattern := "(" + strings.Join(ids, "|") + ")"
			for _, table := range tables {
				var rows, mentions int
				if err := q(fmt.Sprintf(`SELECT count(*), count(*) FILTER (WHERE t::text ~ $1) FROM %q t`, table), pattern).Scan(&rows, &mentions); err != nil {
					t.Fatalf("snapshot %s: %v", table, err)
				}
				fmt.Fprintf(&b, "%s rows=%d mentions=%d\n", table, rows, mentions)
			}
			var seq int
			_ = q(`SELECT invoice_next_seq_live FROM tenant_settings WHERE tenant_id = $1`, tenantA).Scan(&seq)
			fmt.Fprintf(&b, "next live invoice number=%d\n", seq)
		})
		return b.String()
	}
	changed := func(before, after string) string {
		b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
		var out []string
		for i := range b {
			if b[i] != a[i] {
				out = append(out, b[i]+" -> "+a[i])
			}
		}
		return strings.Join(out, "; ")
	}

	type refCase struct {
		name, method string
		path         func(o, f scopeRefs) string
		body         func(o, f scopeRefs) any
		refused      int    // status when f is not the caller's
		message      string // text the refusal's error message must contain
		control      int    // status when f is the caller's own; 0 = no control
	}
	at := func(format string, pick func(f scopeRefs) string) func(o, f scopeRefs) string {
		return func(_, f scopeRefs) string { return fmt.Sprintf(format, pick(f)) }
	}
	fixed := func(p string) func(o, f scopeRefs) string { return func(_, _ scopeRefs) string { return p } }
	none := func(_, _ scopeRefs) any { return nil }
	cus := func(f scopeRefs) string { return f.cus }
	sub := func(f scopeRefs) string { return f.sub }
	inv := func(f scopeRefs) string { return f.draftInv }
	note := func(f scopeRefs) string { return f.creditNote }
	hook := func(f scopeRefs) string { return f.hook }
	policy := func(f scopeRefs) string { return f.policy }
	suffix := func(id string) string { return id[len(id)-6:] }

	cases := []refCase{
		// --- customer id ---
		{"fixed: invoice create [customer_id]", "POST", fixed("/v1/invoices"), func(_, f scopeRefs) any {
			return map[string]any{"customer_id": f.cus, "line_items": line}
		}, 404, "customer not found", 201},
		{"fixed: price override create [customer_id]", "POST", fixed("/v1/price-overrides"), func(o, f scopeRefs) any {
			return map[string]any{"customer_id": f.cus, "rating_rule_version_id": o.rule, "mode": "flat", "flat_amount_cents": 50}
		}, 404, "customer not found", 201},
		{"fixed: billing profile upsert [path customer]", "PUT", at("/v1/customers/%s/billing-profile", cus), func(_, _ scopeRefs) any {
			return map[string]any{"legal_name": "Legal", "country": "US"}
		}, 404, "customer not found", 200},
		{"customer update [path customer]", "PATCH", at("/v1/customers/%s", cus), func(_, _ scopeRefs) any {
			return map[string]any{"display_name": "renamed"}
		}, 404, "", 200},
		{"cost dashboard token rotate [path customer]", "POST", at("/v1/customers/%s/rotate-cost-dashboard-token", cus), none, 404, "", 200},
		{"payment method setup session [path customer]", "POST", at("/v1/customers/%s/payment-methods/setup-session", cus), none, 404, "", 0},
		{"payment method setup email [path customer]", "POST", at("/v1/customers/%s/payment-methods/send-setup-email", cus), none, 404, "", 0},
		{"payment method set default [path customer]", "POST", at("/v1/customers/%s/payment-methods/vlx_pm_any/default", cus), none, 404, "", 0},
		{"payment method detach [path customer]", "DELETE", at("/v1/customers/%s/payment-methods/vlx_pm_any", cus), none, 404, "", 0},
		{"usage ingest [external_customer_id]", "POST", fixed("/v1/usage-events"), func(o, f scopeRefs) any {
			return map[string]any{"external_customer_id": f.ext, "event_name": o.meterKey, "quantity": 1, "idempotency_key": "scope-ingest-ext-" + f.ext}
		}, 422, "", 201},
		{"usage ingest [customer id as external id]", "POST", fixed("/v1/usage-events"), func(o, f scopeRefs) any {
			return map[string]any{"external_customer_id": f.cus, "event_name": o.meterKey, "quantity": 1, "idempotency_key": "scope-ingest-cus-" + f.cus}
		}, 422, "", 0},
		{"usage batch [external_customer_id]", "POST", fixed("/v1/usage-events/batch"), func(o, f scopeRefs) any {
			return []map[string]any{{"external_customer_id": f.ext, "event_name": o.meterKey, "quantity": 1, "idempotency_key": "scope-batch-ext-" + f.ext}}
		}, 422, "", 201},
		{"usage backfill [external_customer_id]", "POST", fixed("/v1/usage-events/backfill"), func(o, f scopeRefs) any {
			return map[string]any{"external_customer_id": f.ext, "event_name": o.meterKey, "quantity": 1, "timestamp": "2026-02-01T00:00:00Z", "idempotency_key": "scope-backfill-ext-" + f.ext}
		}, 422, "", 201},
		// LiteLLM reports an unmapped customer per row inside a 200.
		{"litellm spend [user]", "POST", fixed("/v1/integrations/litellm/spend"), func(_, f scopeRefs) any {
			return map[string]any{
				"id": "req-" + f.ext, "call_type": "acompletion", "model": "gpt-4o", "custom_llm_provider": "openai", "user": f.ext,
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			}
		}, 200, "", 0},

		// --- dunning policy id ---
		{"fixed: customer update [dunning_policy_id]", "PATCH", func(o, _ scopeRefs) string { return "/v1/customers/" + o.cus }, func(_, f scopeRefs) any {
			return map[string]any{"dunning_policy_id": f.policy}
		}, 404, "dunning policy not found", 200},
		{"dunning policy set default [path policy]", "POST", at("/v1/dunning/policies/%s/set-default", policy), none, 404, "", 200},
		{"dunning policy delete [path policy]", "DELETE", at("/v1/dunning/policies/%s", policy), none, 404, "", 0},
		{"dunning run resolve [path run]", "POST", at("/v1/dunning/runs/%s/resolve", func(f scopeRefs) string { return f.dunningRun }), func(_, _ scopeRefs) any {
			return map[string]any{"resolution": "invoice_voided"}
		}, 404, "", 0},

		// --- meter and rating rule ids ---
		{"plan create [meter_ids]", "POST", fixed("/v1/plans"), func(_, f scopeRefs) any {
			return map[string]any{"code": "p-" + suffix(f.meter), "name": "p", "currency": "USD", "billing_interval": "monthly", "base_amount_cents": 1000, "meter_ids": []string{f.meter}}
		}, 422, "", 201},
		{"plan update [meter_ids]", "PATCH", func(o, _ scopeRefs) string { return "/v1/plans/" + o.plan3 }, func(_, f scopeRefs) any {
			return map[string]any{"meter_ids": []string{f.meter}}
		}, 422, "not found", 0},
		{"plan update [path plan]", "PATCH", at("/v1/plans/%s", func(f scopeRefs) string { return f.plan2 }), func(_, _ scopeRefs) any {
			return map[string]any{"name": "renamed"}
		}, 404, "", 200},
		{"fixed: meter create [rating_rule_version_id]", "POST", fixed("/v1/meters"), func(_, f scopeRefs) any {
			return map[string]any{"key": "m_" + suffix(f.rule), "name": "m", "unit": "unit", "aggregation": "sum", "rating_rule_version_id": f.rule}
		}, 422, "rating rule not found", 201},
		{"meter update [rating_rule_version_id]", "PATCH", func(o, _ scopeRefs) string { return "/v1/meters/" + o.meter }, func(_, f scopeRefs) any {
			return map[string]any{"rating_rule_version_id": f.rule}
		}, 422, "", 200},
		{"meter update [path meter]", "PATCH", at("/v1/meters/%s", func(f scopeRefs) string { return f.meter }), func(_, _ scopeRefs) any {
			return map[string]any{"name": "renamed"}
		}, 404, "", 200},
		{"meter pricing rule upsert [path meter]", "POST", at("/v1/meters/%s/pricing-rules", func(f scopeRefs) string { return f.meter }), func(o, _ scopeRefs) any {
			return map[string]any{"rating_rule_version_id": o.rule, "dimension_match": map[string]any{"k": "v"}}
		}, 422, "", 201},
		{"meter pricing rule upsert [rating_rule_version_id]", "POST", func(o, _ scopeRefs) string { return "/v1/meters/" + o.meter + "/pricing-rules" }, func(_, f scopeRefs) any {
			return map[string]any{"rating_rule_version_id": f.rule, "dimension_match": map[string]any{"k": "v"}}
		}, 422, "", 201},
		{"price override create [rating_rule_version_id]", "POST", fixed("/v1/price-overrides"), func(o, f scopeRefs) any {
			return map[string]any{"customer_id": o.cus, "rating_rule_version_id": f.rule, "mode": "flat", "flat_amount_cents": 50}
		}, 422, "", 201},
		{"price override delete [path override]", "DELETE", at("/v1/price-overrides/%s", func(f scopeRefs) string { return f.override }), none, 404, "", 200},
		{"usage ingest [event_name]", "POST", fixed("/v1/usage-events"), func(o, f scopeRefs) any {
			return map[string]any{"external_customer_id": o.ext, "event_name": f.meterKey, "quantity": 1, "idempotency_key": "scope-ingest-meter-" + f.meterKey}
		}, 422, "", 201},
		{"provider cost delete [path rate]", "DELETE", at("/v1/provider-costs/%s", func(f scopeRefs) string { return f.rate }), none, 404, "", 204},

		// --- plan id ---
		{"subscription create [plan_id]", "POST", fixed("/v1/subscriptions"), func(o, f scopeRefs) any {
			return map[string]any{"code": "s-" + suffix(f.plan2), "display_name": "s", "customer_id": o.cus, "items": []map[string]any{{"plan_id": f.plan2}}}
		}, 422, "", 201},
		{"subscription add item [plan_id]", "POST", func(o, _ scopeRefs) string { return "/v1/subscriptions/" + o.sub + "/items" }, func(_, f scopeRefs) any {
			return map[string]any{"plan_id": f.plan2}
		}, 422, "", 201},
		{"subscription item plan change now [new_plan_id]", "PATCH", func(o, _ scopeRefs) string { return "/v1/subscriptions/" + o.activeSub + "/items/" + o.activeItem }, func(_, f scopeRefs) any {
			return map[string]any{"new_plan_id": f.plan3, "immediate": true}
		}, 422, "", 200},
		{"subscription item plan change scheduled [new_plan_id]", "PATCH", func(o, _ scopeRefs) string { return "/v1/subscriptions/" + o.activeSub + "/items/" + o.activeItem }, func(_, f scopeRefs) any {
			return map[string]any{"new_plan_id": f.plan2, "immediate": false}
		}, 422, "", 200},

		// --- subscription and subscription item ids ---
		{"fixed: invoice create [subscription_id]", "POST", fixed("/v1/invoices"), func(o, f scopeRefs) any {
			return map[string]any{"customer_id": o.cus, "subscription_id": f.sub, "line_items": line}
		}, 404, "subscription not found", 201},
		{"billing thresholds [subscription_item_id]", "PUT", func(o, _ scopeRefs) string { return "/v1/subscriptions/" + o.activeSub + "/billing-thresholds" }, func(_, f scopeRefs) any {
			return map[string]any{"item_thresholds": []map[string]any{{"subscription_item_id": f.activeItem, "usage_gte": "100"}}}
		}, 422, "", 200},
		{"subscription add item [path subscription]", "POST", at("/v1/subscriptions/%s/items", sub), func(o, _ scopeRefs) any {
			return map[string]any{"plan_id": o.plan3}
		}, 404, "", 0},
		{"subscription item update [path item]", "PATCH", func(o, f scopeRefs) string { return "/v1/subscriptions/" + o.sub + "/items/" + f.item }, func(_, _ scopeRefs) any {
			return map[string]any{"quantity": 3}
		}, 404, "", 0},
		{"subscription item remove [path item]", "DELETE", func(o, f scopeRefs) string { return "/v1/subscriptions/" + o.sub + "/items/" + f.item }, none, 404, "", 0},
		{"subscription item pending change cancel [path item]", "DELETE", func(o, f scopeRefs) string {
			return "/v1/subscriptions/" + o.sub + "/items/" + f.item + "/pending-change"
		}, none, 404, "", 0},
		{"subscription activate [path subscription]", "POST", at("/v1/subscriptions/%s/activate", sub), none, 404, "", 0},
		{"subscription schedule cancel [path subscription]", "POST", at("/v1/subscriptions/%s/schedule-cancel", sub), func(_, _ scopeRefs) any {
			return map[string]any{"at_period_end": true}
		}, 404, "", 0},
		{"subscription scheduled cancel clear [path subscription]", "DELETE", at("/v1/subscriptions/%s/scheduled-cancel", sub), none, 404, "", 0},
		{"subscription pause collection [path subscription]", "PUT", at("/v1/subscriptions/%s/pause-collection", sub), func(_, _ scopeRefs) any {
			return map[string]any{"behavior": "keep_as_draft"}
		}, 404, "", 0},
		{"subscription resume collection [path subscription]", "DELETE", at("/v1/subscriptions/%s/pause-collection", sub), none, 404, "", 0},
		{"subscription billing thresholds [path subscription]", "PUT", at("/v1/subscriptions/%s/billing-thresholds", sub), func(_, _ scopeRefs) any {
			return map[string]any{"amount_gte": 1000}
		}, 404, "", 0},
		{"subscription billing thresholds clear [path subscription]", "DELETE", at("/v1/subscriptions/%s/billing-thresholds", sub), none, 404, "", 0},
		{"subscription end trial [path subscription]", "POST", at("/v1/subscriptions/%s/end-trial", sub), none, 404, "", 0},
		{"subscription extend trial [path subscription]", "POST", at("/v1/subscriptions/%s/extend-trial", sub), func(_, _ scopeRefs) any {
			return map[string]any{"trial_end": "2026-04-01T00:00:00Z"}
		}, 404, "", 0},
		{"subscription retry billing [path subscription]", "POST", at("/v1/subscriptions/%s/retry-billing", sub), none, 404, "", 0},
		{"subscription cancel [path subscription]", "POST", at("/v1/subscriptions/%s/cancel", sub), none, 404, "", 200},

		// --- invoice and credit note ids ---
		{"credit note create [invoice_id]", "POST", fixed("/v1/credit-notes"), func(_, f scopeRefs) any {
			return map[string]any{"invoice_id": f.finalInv, "reason": "r", "lines": []map[string]any{{"description": "x", "quantity": 1, "unit_amount_cents": 100}}}
		}, 404, "", 201},
		{"invoice add line item [path invoice]", "POST", at("/v1/invoices/%s/line-items", inv), func(_, _ scopeRefs) any { return line[0] }, 404, "", 201},
		{"invoice send [path invoice]", "POST", at("/v1/invoices/%s/send", inv), func(_, _ scopeRefs) any {
			return map[string]any{"email": "billing@example.com"}
		}, 404, "", 0},
		{"invoice resend setup link [path invoice]", "POST", at("/v1/invoices/%s/resend-setup-link", inv), none, 404, "", 0},
		{"invoice collect [path invoice]", "POST", at("/v1/invoices/%s/collect", inv), none, 404, "", 0},
		{"invoice refund [path invoice]", "POST", at("/v1/invoices/%s/refund", inv), func(_, _ scopeRefs) any {
			return map[string]any{"amount_cents": 100, "reason": "requested_by_customer"}
		}, 404, "", 0},
		{"invoice retry tax [path invoice]", "POST", at("/v1/invoices/%s/retry-tax", inv), none, 404, "", 0},
		{"invoice rotate public token [path invoice]", "POST", at("/v1/invoices/%s/rotate-public-token", inv), none, 404, "", 0},
		{"invoice record payment [path invoice]", "POST", at("/v1/invoices/%s/record-payment", inv), func(_, _ scopeRefs) any {
			return map[string]any{"note": "wire"}
		}, 404, "", 0},
		{"invoice mark uncollectible [path invoice]", "POST", at("/v1/invoices/%s/mark-uncollectible", inv), none, 404, "", 0},
		{"invoice finalize [path invoice]", "POST", at("/v1/invoices/%s/finalize", inv), none, 404, "", 200},
		{"invoice void [path invoice]", "POST", at("/v1/invoices/%s/void", inv), none, 404, "", 200},
		{"credit note send [path credit note]", "POST", at("/v1/credit-notes/%s/send", note), func(_, _ scopeRefs) any {
			return map[string]any{"email": "billing@example.com"}
		}, 404, "", 0},
		{"credit note retry refund [path credit note]", "POST", at("/v1/credit-notes/%s/retry-refund", note), none, 404, "", 0},
		{"credit note void [path credit note]", "POST", at("/v1/credit-notes/%s/void", note), none, 404, "", 0},
		{"credit note issue [path credit note]", "POST", at("/v1/credit-notes/%s/issue", note), none, 404, "", 200},

		// --- webhook endpoint and API key ids ---
		{"webhook endpoint update [path endpoint]", "PATCH", at("/v1/webhook-endpoints/endpoints/%s", hook), func(_, _ scopeRefs) any {
			return map[string]any{"description": "renamed"}
		}, 404, "", 200},
		{"webhook endpoint rotate secret [path endpoint]", "POST", at("/v1/webhook-endpoints/endpoints/%s/rotate-secret", hook), none, 404, "", 200},
		{"webhook endpoint delete [path endpoint]", "DELETE", at("/v1/webhook-endpoints/endpoints/%s", hook), none, 404, "", 200},
		{"api key revoke [path key]", "DELETE", at("/v1/api-keys/%s", func(f scopeRefs) string { return f.apiKey }), none, 404, "", 0},
	}

	refuse := func(t *testing.T, method, path, auth string, body any, ids []string, status int, message string) {
		t.Helper()
		before := snapshot(ids)
		code, out := call(method, path, auth, body)
		if code != status {
			t.Errorf("status = %d, want %d; body: %v", code, status, out)
		}
		if !strings.Contains(errMessage(out), message) {
			t.Errorf("error message = %q, want it to contain %q", errMessage(out), message)
		}
		if diff := changed(before, snapshot(ids)); diff != "" {
			t.Errorf("a refused request changed the database: %s", diff)
		}
	}
	for _, c := range cases {
		for _, f := range foreign {
			t.Run(c.name+"/"+f.name, func(t *testing.T) {
				refuse(t, c.method, c.path(own, f.refs), aLive, c.body(own, f.refs), f.refs.ids(), c.refused, c.message)
			})
		}
		if c.control != 0 {
			t.Run(c.name+"/own", func(t *testing.T) {
				if code, out := call(c.method, c.path(own, own), aLive, c.body(own, own)); code != c.control {
					t.Errorf("status = %d, want %d; body: %v", code, c.control, out)
				}
			})
		}
	}

	// Credit grant: fixed. The body used to be decoded into the service's
	// input, which also carries invoice and source_* ids the engine sets on
	// its own grants, so a caller could stamp any of them on a ledger row.
	// The endpoint now reads only its documented fields: the grant is made
	// and none of those ids reach the database.
	for _, f := range foreign {
		t.Run("fixed: credit grant ignores engine-only ids/"+f.name, func(t *testing.T) {
			ids := []string{f.refs.sub, f.refs.item, f.refs.draftInv, f.refs.creditNote}
			mentions := func() (n int) {
				for _, l := range strings.Split(snapshot(ids), "\n") {
					var table string
					var rows, m int
					if _, err := fmt.Sscanf(l, "%s rows=%d mentions=%d", &table, &rows, &m); err == nil {
						n += m
					}
				}
				return n
			}
			before := mentions()
			code, out := call("POST", "/v1/credits/grant", aLive, map[string]any{
				"customer_id": own.cus, "amount_cents": 100, "description": "scope check",
				"invoice_id": f.refs.draftInv, "source_credit_note_id": f.refs.creditNote,
				"source_subscription_id": f.refs.sub, "source_subscription_item_id": f.refs.item,
				"source_plan_changed_at": "2026-03-01T00:00:00Z", "source_change_type": "plan",
			})
			if code != 201 {
				t.Fatalf("status = %d, want 201; body: %v", code, out)
			}
			if wrote := mentions() - before; wrote != 0 {
				t.Errorf("%d row(s) now mention an id the request body named, want 0", wrote)
			}
		})
	}

	// Test clocks exist only in test mode, so the caller is tenant A's test
	// key and there is no other-mode clock to name. A live key naming a
	// test-mode clock is the closest case.
	newClock := func(auth, name string) string {
		code, out := call("POST", "/v1/test-clocks", auth, map[string]any{"name": name, "frozen_time": "2026-03-01T00:00:00Z"})
		if code != 201 {
			t.Fatalf("seed test clock: %d %v", code, out)
		}
		return out["id"].(string)
	}
	ownClock, otherClock := newClock(aTest, "own"), newClock(bTest, "other")
	for _, c := range []struct {
		name, auth, clockID string
		customer, path      int
	}{
		{"another tenant's", aTest, otherClock, 422, 404},
		{"unknown id", aTest, "vlx_clk_missing", 422, 404},
		{"live key, test-mode clock", aLive, ownClock, 422, 403},
	} {
		ids := []string{c.clockID}
		t.Run("customer create [test_clock_id]/"+c.name, func(t *testing.T) {
			refuse(t, "POST", "/v1/customers", c.auth, map[string]any{"external_id": "clock_" + suffix(c.clockID), "display_name": "c", "test_clock_id": c.clockID}, ids, c.customer, "")
		})
		t.Run("test clock advance [path clock]/"+c.name, func(t *testing.T) {
			refuse(t, "POST", "/v1/test-clocks/"+c.clockID+"/advance", c.auth, map[string]any{"frozen_time": "2026-03-02T00:00:00Z"}, ids, c.path, "")
		})
		t.Run("test clock delete [path clock]/"+c.name, func(t *testing.T) {
			refuse(t, "DELETE", "/v1/test-clocks/"+c.clockID, c.auth, nil, ids, c.path, "")
		})
	}
	t.Run("customer create [test_clock_id]/own", func(t *testing.T) {
		if code, out := call("POST", "/v1/customers", aTest, map[string]any{"external_id": "clock_own", "display_name": "c", "test_clock_id": ownClock}); code != 201 {
			t.Errorf("status = %d, want 201; body: %v", code, out)
		}
	})

	// Checkout setup: fixed. It already answered 404 and wrote no row, but
	// only after creating a Stripe customer for the id it was given. It now
	// stops before calling Stripe. Runs last: connecting Stripe changes what
	// other endpoints do. Stripe is replaced by a stub that counts calls.
	t.Run("fixed: checkout setup [customer_id]", func(t *testing.T) {
		if _, err := tenantstripe.NewStore(db).Upsert(context.Background(), tenantstripe.UpsertInput{
			TenantID: tenantA, Livemode: true, SecretKey: "sk_live_stub", PublishableKey: "pk_live_stub",
		}); err != nil {
			t.Fatalf("connect stripe: %v", err)
		}
		real := http.DefaultTransport
		stripeCalls := 0
		http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Host != "api.stripe.com" {
				return real.RoundTrip(req)
			}
			stripeCalls++
			body := `{"id":"cus_stub","object":"customer"}`
			if strings.HasSuffix(req.URL.Path, "/checkout/sessions") {
				body = `{"id":"cs_stub","object":"checkout.session","url":"https://checkout.example/cs_stub"}`
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
		defer func() { http.DefaultTransport = real }()

		for _, f := range foreign {
			t.Run(f.name, func(t *testing.T) {
				stripeCalls = 0
				refuse(t, "POST", "/v1/checkout/setup", aLive, map[string]any{"customer_id": f.refs.cus, "customer_name": "x"}, f.refs.ids(), 404, "customer not found")
				if stripeCalls != 0 {
					t.Errorf("made %d Stripe call(s) for a customer outside the caller's tenant and mode, want 0", stripeCalls)
				}
			})
		}
		t.Run("own", func(t *testing.T) {
			stripeCalls = 0
			if code, out := call("POST", "/v1/checkout/setup", aLive, map[string]any{"customer_id": own.cus, "customer_name": "x"}); code != 201 {
				t.Fatalf("status = %d, want 201; body: %v", code, out)
			}
			if stripeCalls != 2 {
				t.Errorf("made %d Stripe call(s) for the caller's own customer, want 2 (customer, checkout session)", stripeCalls)
			}
		})
	})
}

type sqlRow = sql.Row

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
