package api

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// Credit currency through the real router and database (ADR-129): every
// ledger row stores its currency, a balance is reported per currency, a
// credit pays only an invoice in its own currency, and a manual invoice uses
// the customer's currency (ADR-100).

type ccyEnv struct {
	ts       *httptest.Server
	db       *postgres.DB
	tenantID string
	key      string
}

func newCcyEnv(t *testing.T, name string) ccyEnv {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ts := httptest.NewServer(NewServer(db, clock.Real(), nil))
	t.Cleanup(ts.Close)
	tenantID := testutil.CreateTestTenant(t, db, name)
	return ccyEnv{ts: ts, db: db, tenantID: tenantID, key: createAPIKeyWithMode(t, db, tenantID, false)}
}

func (e ccyEnv) call(t *testing.T, method, path string, body any) flowResp {
	t.Helper()
	return flowDo(t, e.ts, method, path, flowReq{bearer: e.key, body: body})
}

func (e ccyEnv) customer(t *testing.T, ext string) string {
	t.Helper()
	return flowStr(e.call(t, "POST", "/v1/customers", map[string]any{
		"external_id": ext, "display_name": ext, "email": ext + "@ccy.test",
	}).want(t, 201, "create customer "+ext).body, "id")
}

func (e ccyEnv) profileCurrency(t *testing.T, customerID, currency string) {
	t.Helper()
	e.call(t, "PUT", "/v1/customers/"+customerID+"/billing-profile", map[string]any{
		"legal_name": "Ccy Co", "currency": currency,
	}).want(t, 200, "set billing profile currency "+currency)
}

// subscribe starts a live subscription of the customer on a new plan in the
// currency.
func (e ccyEnv) subscribe(t *testing.T, customerID, code, currency string) string {
	t.Helper()
	planID := flowStr(e.call(t, "POST", "/v1/plans", map[string]any{
		"code": code, "name": code, "currency": currency, "billing_interval": "monthly",
		"base_amount_cents": 1000, "base_bill_timing": "in_arrears",
	}).want(t, 201, "create plan "+code).body, "id")
	return flowStr(e.call(t, "POST", "/v1/subscriptions", map[string]any{
		"code": "sub-" + code, "display_name": code, "customer_id": customerID,
		"items": []map[string]any{{"plan_id": planID, "quantity": 1}}, "billing_time": "anniversary", "start_now": true,
	}).want(t, 201, "create subscription "+code).body, "id")
}

func (e ccyEnv) grant(t *testing.T, customerID, currency string, cents int64) map[string]any {
	t.Helper()
	return e.call(t, "POST", "/v1/credits/grant", map[string]any{
		"customer_id": customerID, "currency": currency, "amount_cents": cents, "description": "goodwill " + currency,
	}).want(t, 201, "grant "+currency).body
}

func (e ccyEnv) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	tx, err := e.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// balances is GET /v1/credits/balance/{id} as currency → balance_cents.
func (e ccyEnv) balances(t *testing.T, customerID string) map[string]int64 {
	t.Helper()
	r := e.call(t, "GET", "/v1/credits/balance/"+customerID, nil).want(t, 200, "get balance")
	rows := flowList(r.body, "data")
	if rows == nil || r.body["data"] == nil {
		t.Fatalf("GET /v1/credits/balance: no data array; body: %s", r.raw)
	}
	out := map[string]int64{}
	var order []string
	for _, row := range rows {
		c := flowStr(row, "currency")
		order = append(order, c)
		out[c] = flowInt(row, "balance_cents")
	}
	if !sort.StringsAreSorted(order) {
		t.Errorf("balance rows in order %v, want sorted by currency", order)
	}
	return out
}

func (e ccyEnv) invoice(t *testing.T, customerID, currency string, cents int64) flowResp {
	t.Helper()
	body := map[string]any{
		"customer_id": customerID,
		"line_items":  []map[string]any{{"description": "Setup", "line_type": "add_on", "quantity": 1, "unit_amount_cents": cents}},
	}
	if currency != "" {
		body["currency"] = currency
	}
	return e.call(t, "POST", "/v1/invoices", body)
}

// T13: grant and adjust take a required currency, validated like every other
// currency input (allowlist, uppercase, no zero-decimal codes); lowercase is
// stored uppercase.
func TestCreditCurrency_GrantAndAdjustRequireACurrency(t *testing.T) {
	e := newCcyEnv(t, "Credit Currency Required")
	cust := e.customer(t, "ccy-required")

	for _, c := range []struct{ name, path, currency string }{
		{"grant without currency", "/v1/credits/grant", ""},
		{"grant with an unknown code", "/v1/credits/grant", "XYZ"},
		{"grant in a zero-decimal currency", "/v1/credits/grant", "JPY"},
		{"adjust without currency", "/v1/credits/adjust", ""},
		{"adjust with an unknown code", "/v1/credits/adjust", "XYZ"},
		{"adjust in a zero-decimal currency", "/v1/credits/adjust", "JPY"},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := map[string]any{"customer_id": cust, "amount_cents": 500, "description": "x"}
			if c.currency != "" {
				body["currency"] = c.currency
			}
			wantInvalid(t, e.call(t, "POST", c.path, body), "currency", c.name)
		})
	}
	if got := e.balances(t, cust); len(got) != 0 {
		t.Errorf("refused writes left balances %v, want none", got)
	}

	g := e.grant(t, cust, "eur", 1500)
	if got := flowStr(g, "currency"); got != "EUR" {
		t.Errorf("grant in eur: currency %q, want EUR", got)
	}
	a := e.call(t, "POST", "/v1/credits/adjust", map[string]any{
		"customer_id": cust, "currency": "eur", "amount_cents": 250, "description": "top up",
	}).want(t, 201, "adjust in eur").body
	if got := flowStr(a, "currency"); got != "EUR" {
		t.Errorf("adjust in eur: currency %q, want EUR", got)
	}
	ledger := flowList(e.call(t, "GET", "/v1/credits/ledger/"+cust, nil).want(t, 200, "ledger").body, "data")
	if len(ledger) != 2 {
		t.Fatalf("ledger rows = %d, want 2", len(ledger))
	}
	for _, row := range ledger {
		if got := flowStr(row, "currency"); got != "EUR" {
			t.Errorf("ledger row %s currency %q, want EUR", flowStr(row, "id"), got)
		}
	}
}

// A balance is one figure per currency, never a sum across them.
func TestCreditCurrency_BalancesArePerCurrency(t *testing.T) {
	e := newCcyEnv(t, "Credit Balances Per Currency")
	cust := e.customer(t, "ccy-balances")
	e.grant(t, cust, "USD", 5000)
	e.grant(t, cust, "EUR", 1000)
	e.call(t, "POST", "/v1/credits/grant", map[string]any{
		"customer_id": cust, "currency": "EUR", "amount_cents": 300, "description": "promo", "grant_kind": "promotional",
	}).want(t, 201, "promotional EUR grant")

	got := e.balances(t, cust)
	if len(got) != 2 || got["USD"] != 5000 || got["EUR"] != 1300 {
		t.Errorf("balances = %v, want USD 5000 and EUR 1300", got)
	}

	rows := 0
	for _, row := range flowList(e.call(t, "GET", "/v1/credits/balances", nil).want(t, 200, "list balances").body, "data") {
		if flowStr(row, "customer_id") != cust {
			continue
		}
		rows++
		want := map[string]int64{"USD": 5000, "EUR": 1300}[flowStr(row, "currency")]
		if flowInt(row, "balance_cents") != want || want == 0 {
			t.Errorf("list balances row %v, want one row per currency", row)
		}
	}
	if rows != 2 {
		t.Errorf("list balances: %d rows for the customer, want 2 (one per currency)", rows)
	}

	grants := e.call(t, "GET", "/v1/credits/grants/"+cust, nil).want(t, 200, "grants").body
	split := map[string]map[string]any{}
	for _, r := range flowList(grants, "remaining_by_currency") {
		split[flowStr(r, "currency")] = r
	}
	if len(split) != 2 {
		t.Fatalf("remaining_by_currency = %v, want USD and EUR", grants["remaining_by_currency"])
	}
	if flowInt(split["USD"], "other_remaining_cents") != 5000 || flowInt(split["EUR"], "other_remaining_cents") != 1000 ||
		flowInt(split["EUR"], "promotional_remaining_cents") != 300 || flowInt(split["USD"], "promotional_remaining_cents") != 0 {
		t.Errorf("remaining_by_currency = %v, want USD other 5000; EUR other 1000, promotional 300", split)
	}
	for _, g := range flowList(grants, "grants") {
		if c := flowStr(g, "currency"); c != "USD" && c != "EUR" {
			t.Errorf("grant %s currency %q, want its own currency", flowStr(g, "id"), c)
		}
	}
}

// T7: a deduction draws only on the balance in its currency.
func TestCreditCurrency_DeductOnlyFromItsCurrency(t *testing.T) {
	e := newCcyEnv(t, "Credit Deduct Currency")
	cust := e.customer(t, "ccy-deduct")
	e.grant(t, cust, "USD", 5000)

	wantInvalid(t, e.call(t, "POST", "/v1/credits/adjust", map[string]any{
		"customer_id": cust, "currency": "EUR", "amount_cents": -100, "description": "wrong pot",
	}), "amount_cents", "EUR deduction with only USD credit")
	if got := e.balances(t, cust); got["USD"] != 5000 || len(got) != 1 {
		t.Errorf("after the refused EUR deduction: balances %v, want USD 5000 only", got)
	}

	e.call(t, "POST", "/v1/credits/adjust", map[string]any{
		"customer_id": cust, "currency": "EUR", "amount_cents": 700, "description": "EUR top up",
	}).want(t, 201, "positive EUR adjust")
	e.call(t, "POST", "/v1/credits/adjust", map[string]any{
		"customer_id": cust, "currency": "USD", "amount_cents": -2000, "description": "USD correction",
	}).want(t, 201, "USD deduction")
	if got := e.balances(t, cust); got["USD"] != 3000 || got["EUR"] != 700 {
		t.Errorf("balances %v, want USD 3000, EUR 700", got)
	}
}

// R1 + G2: a manual invoice uses the customer's currency. An explicit one
// must agree with the billing profile and every live subscription; an empty
// one takes the profile, then the live subscription's plan, then the
// workspace default.
func TestInvoiceCreate_CurrencyIsTheCustomers(t *testing.T) {
	e := newCcyEnv(t, "Invoice Currency Customer")
	e.exec(t, `INSERT INTO tenant_settings (tenant_id, default_currency) VALUES ($1, 'GBP')
	           ON CONFLICT (tenant_id) DO UPDATE SET default_currency = 'GBP'`, e.tenantID)

	profiled := e.customer(t, "ccy-inv-profile")
	e.profileCurrency(t, profiled, "USD")
	subscribed := e.customer(t, "ccy-inv-sub")
	e.subscribe(t, subscribed, "ccy-inv-cad", "CAD")
	bare := e.customer(t, "ccy-inv-bare")

	wantConflict := func(r flowResp, fact, what string) {
		t.Helper()
		if r.status != 409 || r.errCode() != "customer_currency_conflict" {
			t.Errorf("%s: status %d code %q, want 409 customer_currency_conflict; body: %s", what, r.status, r.errCode(), r.raw)
			return
		}
		if !strings.Contains(r.errMessage(), fact) {
			t.Errorf("%s: message %q does not name %q", what, r.errMessage(), fact)
		}
	}
	wantCreated := func(r flowResp, currency, what string) {
		t.Helper()
		r.want(t, 201, what)
		if got := flowStr(r.body, "currency"); got != currency {
			t.Errorf("%s: currency %q, want %q", what, got, currency)
		}
	}

	wantConflict(e.invoice(t, profiled, "EUR", 3000), "USD", "EUR invoice, USD profile")
	wantConflict(e.invoice(t, profiled, "eur", 3000), "USD", "lowercase eur invoice, USD profile")
	wantConflict(e.invoice(t, subscribed, "EUR", 3000), "CAD", "EUR invoice, CAD subscription")
	wantCreated(e.invoice(t, profiled, "usd", 3000), "USD", "USD invoice, USD profile")
	wantCreated(e.invoice(t, subscribed, "CAD", 3000), "CAD", "CAD invoice, CAD subscription")
	wantCreated(e.invoice(t, bare, "EUR", 3000), "EUR", "EUR invoice, customer with no currency facts")

	wantCreated(e.invoice(t, profiled, "", 3000), "USD", "no currency, USD profile")
	wantCreated(e.invoice(t, subscribed, "", 3000), "CAD", "no currency, CAD subscription")
	wantCreated(e.invoice(t, bare, "", 3000), "GBP", "no currency, no facts: the workspace default")
}

// The operator's collect route: a credit pays only an invoice in its own
// currency.
func TestCreditCurrency_CollectAppliesOnlySameCurrency(t *testing.T) {
	e := newCcyEnv(t, "Credit Collect Currency")
	cust := e.customer(t, "ccy-collect")
	e.grant(t, cust, "USD", 5000)

	collect := func(currency string) map[string]any {
		t.Helper()
		id := flowStr(e.invoice(t, cust, currency, 3000).want(t, 201, "create "+currency+" invoice").body, "id")
		e.call(t, "POST", "/v1/invoices/"+id+"/finalize", nil).want(t, 200, "finalize "+currency)
		e.call(t, "POST", "/v1/invoices/"+id+"/collect", nil)
		return e.call(t, "GET", "/v1/invoices/"+id, nil).want(t, 200, "get "+currency+" invoice").body
	}

	eur := collect("EUR")
	if flowInt(eur, "credits_applied_cents") != 0 || flowInt(eur, "amount_due_cents") != 3000 {
		t.Errorf("EUR invoice: credits_applied %d, amount_due %d; want 0 and 3000 (USD credit never pays EUR)",
			flowInt(eur, "credits_applied_cents"), flowInt(eur, "amount_due_cents"))
	}
	if got := e.balances(t, cust); got["USD"] != 5000 {
		t.Errorf("after the EUR collect: balances %v, want USD 5000", got)
	}

	usd := collect("USD")
	if flowInt(usd, "credits_applied_cents") != 3000 || flowInt(usd, "amount_due_cents") != 0 {
		t.Errorf("USD invoice: credits_applied %d, amount_due %d; want 3000 and 0",
			flowInt(usd, "credits_applied_cents"), flowInt(usd, "amount_due_cents"))
	}
	if got := e.balances(t, cust); got["USD"] != 2000 {
		t.Errorf("after the USD collect: balances %v, want USD 2000", got)
	}
}

// ledgerCurrency is the currency stored on the ledger rows a query selects;
// it fails unless exactly one distinct value comes back.
func (e ccyEnv) ledgerCurrency(t *testing.T, what, where string, args ...any) string {
	t.Helper()
	tx, err := e.db.BeginTx(context.Background(), postgres.TxBypass, "")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.Query(`SELECT DISTINCT COALESCE(currency, '') FROM customer_credit_ledger WHERE `+where, args...)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		got = append(got, c)
	}
	if len(got) != 1 {
		t.Fatalf("%s: currencies %v, want exactly one row currency", what, got)
	}
	return got[0]
}

// T5 (routes): the rows a credit note, a commit invoice and its void write
// take the invoice's currency, through the production adapters.
//
// Mutation-verify: hardcode "USD" where creditnote.Issue builds its grant
// input → both credit-note subtests read USD, red.
func TestCreditCurrency_DerivedRowsTakeTheInvoiceCurrency(t *testing.T) {
	e := newCcyEnv(t, "Credit Derived Currency")

	finalized := func(cust string, lines []map[string]any) string {
		t.Helper()
		id := flowStr(e.call(t, "POST", "/v1/invoices", map[string]any{
			"customer_id": cust, "currency": "EUR", "line_items": lines,
		}).want(t, 201, "create EUR invoice").body, "id")
		e.call(t, "POST", "/v1/invoices/"+id+"/finalize", nil).want(t, 200, "finalize EUR invoice")
		return id
	}
	addOn := []map[string]any{{"description": "Setup", "line_type": "add_on", "quantity": 1, "unit_amount_cents": 3000}}
	issueNote := func(invoiceID string, cents int64, creditCents int64) {
		t.Helper()
		body := map[string]any{
			"invoice_id": invoiceID, "reason": "order_change", "auto_issue": true,
			"lines": []map[string]any{{"description": "credit", "quantity": 1, "unit_amount_cents": cents}},
		}
		if creditCents > 0 {
			body["credit_amount_cents"] = creditCents
		}
		e.call(t, "POST", "/v1/credit-notes", body).want(t, 201, "issue credit note")
	}

	t.Run("credit note on a paid invoice credits the balance in the invoice currency", func(t *testing.T) {
		cust := e.customer(t, "ccy-derived-paid")
		inv := finalized(cust, addOn)
		e.call(t, "POST", "/v1/invoices/"+inv+"/record-payment", map[string]any{"note": "wire"}).want(t, 200, "record payment")
		issueNote(inv, 1000, 1000)
		if c := e.ledgerCurrency(t, "credit-note grant", "invoice_id = $1 AND source_credit_note_id IS NOT NULL", inv); c != "EUR" {
			t.Errorf("credit-note grant currency %q, want EUR", c)
		}
		if got := e.balances(t, cust); got["EUR"] != 1000 || len(got) != 1 {
			t.Errorf("balances %v, want EUR 1000", got)
		}
	})

	t.Run("credit note on an unpaid invoice returns the applied credit in its currency", func(t *testing.T) {
		cust := e.customer(t, "ccy-derived-unpaid")
		e.grant(t, cust, "EUR", 2000)
		inv := finalized(cust, addOn)
		e.call(t, "POST", "/v1/invoices/"+inv+"/collect", nil)
		if got := flowInt(e.call(t, "GET", "/v1/invoices/"+inv, nil).want(t, 200, "get invoice").body, "credits_applied_cents"); got != 2000 {
			t.Fatalf("credits applied %d, want 2000", got)
		}
		issueNote(inv, 2500, 0)
		if c := e.ledgerCurrency(t, "credits returned", "invoice_id = $1 AND source_credit_note_id IS NOT NULL", inv); c != "EUR" {
			t.Errorf("credits-returned grant currency %q, want EUR", c)
		}
		if got := e.balances(t, cust); got["EUR"] != 1500 || len(got) != 1 {
			t.Errorf("balances %v, want EUR 1500 (2500 note: 1000 off the due, 1500 back to the balance)", got)
		}
	})

	t.Run("commit grant and its void retire take the funding invoice currency", func(t *testing.T) {
		cust := e.customer(t, "ccy-derived-commit")
		inv := finalized(cust, []map[string]any{{
			"description": "Prepaid commit", "line_type": "add_on", "quantity": 1,
			"unit_amount_cents": 9000, "commit_granted_cents": 10000,
		}})
		if c := e.ledgerCurrency(t, "commit grant", "source_invoice_id = $1 AND grant_kind = 'commit'", inv); c != "EUR" {
			t.Errorf("commit grant currency %q, want EUR", c)
		}
		e.call(t, "POST", "/v1/invoices/"+inv+"/void", nil).want(t, 200, "void commit invoice")
		if c := e.ledgerCurrency(t, "void retire", "metadata->>'reason' = 'commit_void_retire' AND metadata->>'funding_invoice_id' = $1", inv); c != "EUR" {
			t.Errorf("void-retire currency %q, want EUR", c)
		}
		if got := e.balances(t, cust); got["EUR"] != 0 {
			t.Errorf("balances %v, want EUR 0 after the void", got)
		}
	})
}

// T9 (routes): the live balance and the public cost dashboard report one
// entry per currency.
func TestCreditCurrency_LiveBalanceAndCostDashboardPerCurrency(t *testing.T) {
	e := newCcyEnv(t, "Credit Live Currency")
	cust := e.customer(t, "ccy-live")
	e.grant(t, cust, "USD", 5000)
	e.grant(t, cust, "EUR", 1000)

	live := e.call(t, "GET", "/v1/credits/balance/"+cust+"/live", nil).want(t, 200, "live balance").body
	byCcy := map[string]map[string]any{}
	for _, b := range flowList(live, "balances") {
		byCcy[flowStr(b, "currency")] = b
	}
	if len(byCcy) != 2 || flowInt(byCcy["USD"], "balance_cents") != 5000 || flowInt(byCcy["EUR"], "balance_cents") != 1000 ||
		flowInt(byCcy["EUR"], "estimated_remaining_cents") != 1000 {
		t.Errorf("live balances = %v, want USD 5000 and EUR 1000; body: %v", byCcy, live)
	}
	if _, ok := live["balance_cents"]; ok {
		t.Error("the live balance still has a top-level balance_cents: it added currencies together")
	}

	token := flowStr(e.call(t, "POST", "/v1/customers/"+cust+"/rotate-cost-dashboard-token", nil).want(t, 200, "rotate token").body, "token")
	dash := flowDo(t, e.ts, "GET", "/v1/public/cost-dashboard/"+token, flowReq{}).want(t, 200, "cost dashboard").body
	credits := map[string]map[string]any{}
	for _, c := range flowList(dash, "credits") {
		credits[flowStr(c, "currency")] = c
	}
	if len(credits) != 2 || flowInt(credits["USD"], "balance_cents") != 5000 || flowInt(credits["EUR"], "balance_cents") != 1000 {
		t.Errorf("cost dashboard credits = %v, want an entry per currency (USD 5000, EUR 1000); body: %v", credits, dash["credits"])
	}
}
