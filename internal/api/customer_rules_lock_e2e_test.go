package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/testutil"
)

// The customer-rules lock over HTTP (ADR-130 §8 step 1a). Subscription
// creates, item changes, draft activations, customer updates and
// billing-profile writes each take one per-customer lock before they check a
// customer-scoped rule, and hold it until they commit. So of two such writes
// at once, the second checks against what the first committed.
//
// Two seams make the races deterministic:
//   - The number-row hold: a test transaction locks the tenant's number row
//     (tenant_settings). A create or an activation that bills a day-1 invoice
//     then waits at its invoice number, inside its transaction, past its
//     checks, holding the customer-rules lock.
//   - The row hold: a test transaction updates the customer row. A customer
//     update then waits at its final UPDATE, holding the customer-rules lock.
//
// Each test waits until the expected requests wait for a lock, or until the
// request under test has answered, which is the bug: with nothing to order
// it, it does not wait.

// pendingRequest is one request the rig serves in the background.
type pendingRequest struct {
	done chan struct{}
	code int
	body []byte
}

// start serves a request in the background at the instant at.
func (r *lagRig) start(method, path string, at time.Time, body any) *pendingRequest {
	p := &pendingRequest{done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.code, p.body = r.do(method, path, at, body)
	}()
	return p
}

// wait returns the answer once the request has one.
func (p *pendingRequest) wait(t *testing.T, name string) (int, []byte) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s never answered", name)
	}
	return p.code, p.body
}

// apiErr is the code and message of an error answer.
func apiErr(raw []byte) (code, message string) {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &env) // a body that is not an error leaves both empty, which the caller reports
	return env.Error.Code, env.Error.Message
}

// holdTenantNumberRow locks the tenant's number row FOR UPDATE on the admin
// pool and returns the open transaction; the test rolls it back to release
// the writers queued behind it, in the order they arrived. It creates the row
// first when no invoice has been numbered yet. The row is the same for both
// modes.
func holdTenantNumberRow(t *testing.T, ctx context.Context, admin *sql.DB, tenantID string) *sql.Tx {
	t.Helper()
	if _, err := admin.ExecContext(ctx, `INSERT INTO tenant_settings (tenant_id) VALUES ($1) ON CONFLICT (tenant_id) DO NOTHING`, tenantID); err != nil {
		t.Fatalf("create the number row: %v", err)
	}
	gate, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin gate: %v", err)
	}
	t.Cleanup(func() { _ = gate.Rollback() }) // released by the test already, or on a failure
	if _, err := gate.ExecContext(ctx, `SELECT 1 FROM tenant_settings WHERE tenant_id = $1 FOR UPDATE`, tenantID); err != nil {
		t.Fatalf("gate: lock the number row: %v", err)
	}
	return gate
}

// holdCustomerRow updates the customer row on the admin pool and returns the
// open transaction. A customer update waits for it at its own UPDATE. A
// foreign-key check from a subscription insert does not.
func holdCustomerRow(t *testing.T, ctx context.Context, admin *sql.DB, customerID string) *sql.Tx {
	t.Helper()
	hold, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin row hold: %v", err)
	}
	t.Cleanup(func() { _ = hold.Rollback() }) // released by the test already, or on a failure
	if _, err := hold.ExecContext(ctx, `UPDATE customers SET updated_at = updated_at WHERE id = $1`, customerID); err != nil {
		t.Fatalf("hold the customer row: %v", err)
	}
	return hold
}

// release rolls a hold back.
func release(t *testing.T, hold *sql.Tx) {
	t.Helper()
	if err := hold.Rollback(); err != nil {
		t.Fatalf("release the hold: %v", err)
	}
}

// createBody is a create on planID for the rig's customer.
func (r *lagRig) createBody(code, planID string) map[string]any {
	return map[string]any{
		"code": code, "display_name": code, "customer_id": r.custID,
		"items": []map[string]any{{"plan_id": planID, "quantity": 1}},
	}
}

// liveSubs counts the customer's draft, trialing and active subscriptions.
func (r *lagRig) liveSubs(customerID string) int {
	r.t.Helper()
	return flowCount(r.t, r.db, `SELECT count(*) FROM subscriptions
		WHERE customer_id = $1 AND status IN ('draft', 'trialing', 'active')`, customerID)
}

// customerField reads one field of the customer as GET returns it.
func (r *lagRig) customerField(customerID, field string) any {
	r.t.Helper()
	return r.must(http.MethodGet, "/v1/customers/"+customerID, lagP0, nil, http.StatusOK)[field]
}

// TestE2E_CustomerLock_TwoCreatesOneMeter: two creates for one customer, on
// two plans that bill one meter, at the same moment. Both pass the service's
// meter check, which reads only committed subscriptions. The first holds the
// customer-rules lock while it waits at its invoice number. The second waits
// for that lock, then its meter check on the transaction reads the first
// subscription and refuses. Without the lock both commit, and every period
// bills the meter's usage on both invoices.
//
// Mutation-verify: drop the lock from createInTx → both answer 201.
func TestE2E_CustomerLock_TwoCreatesOneMeter(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	ctx := t.Context()
	admin := testutil.AdminPool(t)

	gate := holdTenantNumberRow(t, ctx, admin, r.tenantID)
	first := r.start(http.MethodPost, "/v1/subscriptions", lagP0, r.createBody("first", r.planA))
	testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 1, first.done)
	second := r.start(http.MethodPost, "/v1/subscriptions", lagP0, r.createBody("second", r.planB))
	testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 2, second.done)
	release(t, gate)

	code1, raw1 := first.wait(t, "the first create")
	code2, raw2 := second.wait(t, "the second create")
	if n := r.liveSubs(r.custID); n != 1 {
		t.Fatalf("%d live subscriptions bill meter %s, want 1 (first: %d %s; second: %d %s)", n, r.meterID, code1, raw1, code2, raw2)
	}
	if code1 != http.StatusCreated {
		t.Fatalf("the first create = %d %s, want 201", code1, raw1)
	}
	if errCode, msg := apiErr(raw2); code2 != http.StatusConflict || errCode != "invalid_state" || !strings.Contains(msg, "already bills meter") {
		t.Fatalf("the second create = %d %s, want 409 invalid_state naming the meter", code2, raw2)
	}
}

// TestE2E_CustomerLock_TwoCreatesTwoCurrencies: two creates for one
// customer, one in USD and one in EUR, at the same moment. A customer bills
// in one currency (ADR-100), and the create's currency check reads only
// committed subscriptions. It runs on the create's transaction, under the
// customer-rules lock, so the second create checks against the first. The
// plans bill no meter, so the meter check passes and the currency check
// decides.
//
// Mutation-verify: move Service.Create's currency check back before
// CreateWithBill → both answer 201.
func TestE2E_CustomerLock_TwoCreatesTwoCurrencies(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	ctx := t.Context()
	admin := testutil.AdminPool(t)
	plan := func(code, currency string) string {
		return r.must(http.MethodPost, "/v1/plans", lagP0, map[string]any{
			"code": code, "name": code, "currency": currency, "billing_interval": "monthly",
			"base_amount_cents": 2000, "base_bill_timing": "in_advance", "prices": []map[string]any{},
		}, http.StatusCreated)["id"].(string)
	}
	usd, eur := plan("seat_usd", "USD"), plan("seat_eur", "EUR")

	gate := holdTenantNumberRow(t, ctx, admin, r.tenantID)
	first := r.start(http.MethodPost, "/v1/subscriptions", lagP0, r.createBody("in-usd", usd))
	testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 1, first.done)
	second := r.start(http.MethodPost, "/v1/subscriptions", lagP0, r.createBody("in-eur", eur))
	testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 2, second.done)
	release(t, gate)

	code1, raw1 := first.wait(t, "the USD create")
	code2, raw2 := second.wait(t, "the EUR create")
	if code1 != http.StatusCreated {
		t.Fatalf("the USD create = %d %s, want 201", code1, raw1)
	}
	if errCode, msg := apiErr(raw2); code2 != http.StatusConflict || errCode != "invalid_state" || !strings.Contains(msg, "billing in USD") {
		t.Fatalf("the EUR create = %d %s, want 409 invalid_state: the customer already has a subscription billing in USD", code2, raw2)
	}
	if n := r.liveSubs(r.custID); n != 1 {
		t.Fatalf("%d live subscriptions, want 1", n)
	}
}

// TestE2E_CustomerLock_ProfileCurrencyRacesCreate: a billing-profile save
// with currency EUR while a USD create is in flight. The profile currency
// replaces the plan currency on every invoice, so a saved EUR profile would
// relabel the USD subscription's cents as euros. The profile write checks
// and saves under the customer-rules lock, so it waits for the create and
// then refuses.
//
// Mutation-verify: run UpsertBillingProfile's currency check before
// WithCustomerRulesLock, or drop the lock from WithCustomerRulesLock → the
// profile save answers 200.
func TestE2E_CustomerLock_ProfileCurrencyRacesCreate(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	ctx := t.Context()
	admin := testutil.AdminPool(t)

	gate := holdTenantNumberRow(t, ctx, admin, r.tenantID)
	create := r.start(http.MethodPost, "/v1/subscriptions", lagP0, r.createBody("in-usd", r.planA))
	testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 1, create.done)
	profile := r.start(http.MethodPut, "/v1/customers/"+r.custID+"/billing-profile", lagP0, map[string]any{"currency": "EUR"})
	testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 2, profile.done)
	release(t, gate)

	codeC, rawC := create.wait(t, "the create")
	codeP, rawP := profile.wait(t, "the profile save")
	if codeC != http.StatusCreated {
		t.Fatalf("the create = %d %s, want 201", codeC, rawC)
	}
	if errCode, _ := apiErr(rawP); codeP != http.StatusConflict || errCode != "invalid_state" {
		t.Fatalf("the EUR profile save = %d %s, want 409 invalid_state: the customer has a USD subscription", codeP, rawP)
	}
	if n := flowCount(t, r.db, `SELECT count(*) FROM customer_billing_profiles
		WHERE customer_id = $1 AND COALESCE(currency, '') <> ''`, r.custID); n != 0 {
		t.Fatal("the profile currency was saved beside a USD subscription")
	}
}

// TestE2E_CustomerLock_ArchiveRacesCreate: an archive and a create for one
// customer at the same moment. Archive is refused while a subscription bills
// (ADR-067), and a create is refused for an archived customer. Whichever
// takes the customer-rules lock first, the other sees it: the customer never
// ends archived with a billing subscription.
func TestE2E_CustomerLock_ArchiveRacesCreate(t *testing.T) {
	// The create holds the lock at its invoice number; the archive waits,
	// then counts the new subscription.
	//
	// Mutation-verify: drop the lock from createInTx, or from
	// WithCustomerRulesLock, or run Service.Update's archive check before
	// WithCustomerRulesLock → the archive answers 200.
	t.Run("create_first", func(t *testing.T) {
		r := newLagRigBare(t, "in_advance", 0)
		ctx := t.Context()
		admin := testutil.AdminPool(t)

		gate := holdTenantNumberRow(t, ctx, admin, r.tenantID)
		create := r.start(http.MethodPost, "/v1/subscriptions", lagP0, r.createBody("pro-seat", r.planA))
		testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 1, create.done)
		archive := r.start(http.MethodPatch, "/v1/customers/"+r.custID, lagP0, map[string]any{"status": "archived"})
		testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 2, archive.done)
		release(t, gate)

		codeC, rawC := create.wait(t, "the create")
		codeA, rawA := archive.wait(t, "the archive")
		if codeC != http.StatusCreated {
			t.Fatalf("the create = %d %s, want 201", codeC, rawC)
		}
		if errCode, msg := apiErr(rawA); codeA != http.StatusConflict || errCode != "invalid_state" || !strings.Contains(msg, "(pro-seat)") {
			t.Fatalf("the archive = %d %s, want 409 invalid_state naming subscription pro-seat", codeA, rawA)
		}
		if got := r.customerField(r.custID, "status"); got != "active" {
			t.Fatalf("customer status = %v, want active: it was archived with a subscription that bills", got)
		}
	})

	// The archive holds the lock while it waits at its UPDATE. The create
	// read the customer as active before it waits; it reads the status again
	// under the lock and refuses.
	//
	// Mutation-verify: keep the lock in createInTx but drop its archived
	// refusal → the create answers 201.
	t.Run("archive_first", func(t *testing.T) {
		r := newLagRigBare(t, "in_advance", 0)
		ctx := t.Context()
		admin := testutil.AdminPool(t)

		hold := holdCustomerRow(t, ctx, admin, r.custID)
		archive := r.start(http.MethodPatch, "/v1/customers/"+r.custID, lagP0, map[string]any{"status": "archived"})
		testutil.WaitForLockWaitersOrDone(t, ctx, admin, "UPDATE customers", 1, archive.done)
		create := r.start(http.MethodPost, "/v1/subscriptions", lagP0, r.createBody("late", r.planA))
		testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 2, create.done)
		release(t, hold)

		codeA, rawA := archive.wait(t, "the archive")
		codeC, rawC := create.wait(t, "the create")
		if codeA != http.StatusOK {
			t.Fatalf("the archive = %d %s, want 200", codeA, rawA)
		}
		if errCode, msg := apiErr(rawC); codeC != http.StatusConflict || errCode != "invalid_state" || !strings.Contains(msg, "customer is archived") {
			t.Fatalf("the create = %d %s, want 409 invalid_state: customer is archived", codeC, rawC)
		}
		if n := r.liveSubs(r.custID); n != 0 {
			t.Fatalf("%d live subscriptions on the archived customer, want 0", n)
		}
	})
}

// TestE2E_CustomerUpdate_ConcurrentEditsKeepBothFields: two PATCHes to one
// customer at the same moment, each sending one field. Each update reads the
// customer and writes every field back, so with no lock the second wrote the
// first's field back as it had read it: a rename un-archived the customer or
// reset its dunning policy. Under the customer-rules lock the second reads
// the row the first saved.
//
// Mutation-verify: drop the lock from WithCustomerRulesLock, or build the
// update from a store.Get read before the lock → the first field is lost.
func TestE2E_CustomerUpdate_ConcurrentEditsKeepBothFields(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	ctx := t.Context()
	admin := testutil.AdminPool(t)
	policyID := r.must(http.MethodPost, "/v1/dunning/policies", lagP0, map[string]any{
		"name": "Gentle", "enabled": true, "retry_days": []int{3, 3},
	}, http.StatusCreated)["id"].(string)

	archive := map[string]any{"status": "archived"}
	rename := map[string]any{"display_name": "Renamed"}
	policy := map[string]any{"dunning_policy_id": policyID}
	cases := []struct {
		name   string
		a, b   map[string]any
		expect map[string]any
	}{
		{"archive then name", archive, rename, map[string]any{"status": "archived", "display_name": "Renamed"}},
		{"name then archive", rename, archive, map[string]any{"status": "archived", "display_name": "Renamed"}},
		{"policy then name", policy, rename, map[string]any{"dunning_policy_id": policyID, "display_name": "Renamed"}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext := "concurrent_" + string(rune('a'+i))
			custID := r.must(http.MethodPost, "/v1/customers", lagP0, map[string]any{
				"external_id": ext, "display_name": "Original", "email": ext + "@example.test",
			}, http.StatusCreated)["id"].(string)
			path := "/v1/customers/" + custID

			hold := holdCustomerRow(t, ctx, admin, custID)
			first := r.start(http.MethodPatch, path, lagP0, tc.a)
			testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 1, first.done)
			second := r.start(http.MethodPatch, path, lagP0, tc.b)
			testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 2, second.done)
			release(t, hold)

			for name, p := range map[string]*pendingRequest{"the first PATCH": first, "the second PATCH": second} {
				if code, raw := p.wait(t, name); code != http.StatusOK {
					t.Fatalf("%s = %d %s, want 200", name, code, raw)
				}
			}
			for field, v := range tc.expect {
				if got := r.customerField(custID, field); got != v {
					t.Errorf("%s = %v, want %v: one PATCH wrote back a field it did not send", field, got, v)
				}
			}
		})
	}
}

// TestE2E_Activate_ArchivedCustomerRefused: archive skips drafts (ADR-067),
// so a customer with only a draft can be archived. Activating that draft
// would then start billing a customer the operator retired. Activation reads
// the customer's status under the customer-rules lock and refuses.
//
// Mutation-verify: drop the archived refusal from ActivateDraftWithBill →
// the activation answers 200 and bills a day-1 invoice.
func TestE2E_Activate_ArchivedCustomerRefused(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	body := r.createBody("draft", r.planA)
	body["status"] = "draft"
	subID := r.must(http.MethodPost, "/v1/subscriptions", lagP0, body, http.StatusCreated)["id"].(string)
	r.must(http.MethodPatch, "/v1/customers/"+r.custID, lagP0, map[string]any{"status": "archived"}, http.StatusOK)

	code, raw := r.do(http.MethodPost, "/v1/subscriptions/"+subID+"/activate", lagP0, nil)
	if errCode, msg := apiErr(raw); code != http.StatusConflict || errCode != "invalid_state" || !strings.Contains(msg, "customer is archived") {
		t.Fatalf("activate a draft of an archived customer = %d %s, want 409 invalid_state: customer is archived", code, raw)
	}
	r.subID = subID
	if s := r.sub(); s.Status != "draft" {
		t.Fatalf("subscription status = %s, want draft", s.Status)
	}
	if _, invoices := r.customerCounts(); invoices != 0 {
		t.Fatalf("%d invoices for the archived customer, want 0", invoices)
	}
}

// TestE2E_Activate_CustomerLockedBeforeTheNumberRow pins where activation
// takes the customer-rules lock: before its invoice number. An activation
// waits at the tenant's number row; a create for the same customer then
// waits for the activation's lock. Both must commit. A deadlock here is not
// answered as a retryable 409 (ADR-136 converts neither create nor activate),
// so the loser would get a stored 500.
//
// Passes on main too: it guards the lock order.
//
// Mutation-verify: take the lock in ActivateDraftWithBill after billFn → the
// activation holds the number row and waits for the create's lock while the
// create waits for the number row; Postgres aborts one with 40P01.
func TestE2E_Activate_CustomerLockedBeforeTheNumberRow(t *testing.T) {
	r := newLagRigBare(t, "in_advance", 0)
	ctx := t.Context()
	admin := testutil.AdminPool(t)
	body := r.createBody("draft", r.planA)
	body["status"] = "draft"
	subID := r.must(http.MethodPost, "/v1/subscriptions", lagP0, body, http.StatusCreated)["id"].(string)

	gate := holdTenantNumberRow(t, ctx, admin, r.tenantID)
	activate := r.start(http.MethodPost, "/v1/subscriptions/"+subID+"/activate", lagP0, nil)
	testutil.WaitForLockWaitersOrDone(t, ctx, admin, "tenant_settings", 1, activate.done)
	// plan_c bills no meter and is in USD, so the create passes its checks
	// beside the draft on plan_a.
	create := r.start(http.MethodPost, "/v1/subscriptions", lagP0, r.createBody("add-on", r.planC))
	testutil.WaitForLockWaitersOrDone(t, ctx, admin, "", 2, create.done)
	release(t, gate)

	if code, raw := activate.wait(t, "the activation"); code != http.StatusOK {
		t.Fatalf("the activation = %d %s, want 200", code, raw)
	}
	if code, raw := create.wait(t, "the create"); code != http.StatusCreated {
		t.Fatalf("the create = %d %s, want 201", code, raw)
	}
}
