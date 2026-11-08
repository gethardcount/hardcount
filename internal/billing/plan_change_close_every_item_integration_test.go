package billing_test

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
	"github.com/gethardcount/hardcount/internal/usage"
	"github.com/gethardcount/hardcount/internal/webhook"
)

// A plan change scheduled on one item of a subscription applies when the
// billing run closes the period that ends at its effective instant. That
// close bills every live item of the subscription: each swapped item on its
// new plan, every other item as it stands (#1620). Each test closes the
// parity period (May 2026) on real Postgres and reads what the close wrote:
// the invoice lines, the audit rows and the queued webhook events.

// everyItemFixture is a tenant with two meters, calls and seats. Usage of
// both is recorded on day 10 of the period: 5 calls and 3 seats.
type everyItemFixture struct {
	*parityFixture
	store *subscription.PostgresStore // with its outbox wired: the close and the cancel use it
	calls domain.Meter
	seats domain.Meter
}

func newEveryItemFixture(t *testing.T, db *postgres.DB, label string) *everyItemFixture {
	t.Helper()
	pf := newParityFixture(t, db, label)
	subs := subscription.NewPostgresStore(db)
	subs.SetOutboxEnqueuer(webhook.NewOutboxStore(db))
	f := &everyItemFixture{parityFixture: pf, store: subs}
	meter := func(name string, qty int64) domain.Meter {
		m, err := pf.pricing.CreateMeter(pf.ctx, pf.tenantID, domain.Meter{
			EventName: "p128_" + name, Name: name, Unit: name, Aggregation: "sum",
		})
		if err != nil {
			t.Fatalf("create meter %s: %v", name, err)
		}
		if _, err := usage.NewPostgresStore(db).Ingest(pf.ctx, pf.tenantID, domain.UsageEvent{
			CustomerID: pf.custID, MeterID: m.ID, Quantity: decimal.NewFromInt(qty), Timestamp: parityPS.AddDate(0, 0, 9),
		}); err != nil {
			t.Fatalf("ingest %s: %v", name, err)
		}
		return m
	}
	f.calls = meter("calls", 5)
	f.seats = meter("seats", 3)
	return f
}

// plan is a monthly plan with a base fee. When meter is set, the plan prices
// it at perUnit cents per unit.
func (f *everyItemFixture) plan(t *testing.T, code string, base int64, timing domain.BillTiming, meter *domain.Meter, perUnit string) domain.Plan {
	t.Helper()
	var prices []priceOn
	if meter != nil {
		prices = append(prices, catchAll(meter.ID, perUnit))
	}
	p, _ := createPlanWithPrices(t, f.db, f.ctx, f.tenantID, domain.Plan{
		Code: code, Name: code, Currency: "USD",
		BillingInterval: domain.BillingMonthly, BaseBillTiming: timing,
		BaseAmountCents: base, Status: domain.PlanActive,
	}, prices...)
	return p
}

// sub creates an active subscription for the period with one item on each
// plan, and returns the item id on each plan.
func (f *everyItemFixture) sub(t *testing.T, code string, plans ...domain.Plan) (string, map[string]string) {
	t.Helper()
	items := make([]domain.SubscriptionItem, 0, len(plans))
	for _, p := range plans {
		items = append(items, domain.SubscriptionItem{PlanID: p.ID, Quantity: 1})
	}
	sub := f.activeSub(t, code, parityPS, items...)
	itemOn := make(map[string]string, len(sub.Items))
	for _, it := range sub.Items {
		itemOn[it.PlanID] = it.ID
	}
	return sub.ID, itemOn
}

// schedule schedules the item's change to plan at the period end.
func (f *everyItemFixture) schedule(t *testing.T, itemID string, plan domain.Plan) {
	t.Helper()
	if _, err := f.store.SetItemPendingPlan(f.ctx, f.tenantID, itemID, plan.ID, parityPE); err != nil {
		t.Fatalf("schedule the change to %s: %v", plan.Code, err)
	}
}

// close runs the billing run at the period end on an engine wired as the
// server wires it for this path: the subscription store with its outbox, and
// the audit log. afterRead, when set, runs once, right after the run reads
// the subscription it is about to close.
func (f *everyItemFixture) close(t *testing.T, afterRead func()) {
	t.Helper()
	reader := &readHookSubAdapter{subStoreAdapter: &subStoreAdapter{f.store}, afterRead: afterRead}
	e := billing.NewEngine(
		reader, &usageStoreAdapter{usage.NewPostgresStore(f.db)},
		&pricingStoreAdapter{pricing.NewPostgresStore(f.db)}, &invoiceStoreAdapter{invoice.NewPostgresStore(f.db)},
		nil, tenant.NewSettingsStore(f.db), testPaymentSetupsNoPM{}, testChargerSentinel{}, clock.NewFake(parityPE.Add(time.Nanosecond)),
	)
	e.SetIntervalReader(f.store)
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	e.SetNoPaymentMethodNotifier(&testNoPMNotifier{})
	e.SetDunningStarter(testDunningStarter{})
	e.SetStripeReadiness(testStripeConnected{})
	e.SetInvoiceEmailer(&testInvoiceEmailer{})
	e.SetAuditLogger(audit.NewLogger(f.db))
	if _, errs := e.RunCycleForTenant(f.ctx, f.tenantID, 50); len(errs) > 0 {
		t.Fatalf("billing run: %v", errs)
	}
}

// readHookSubAdapter runs afterRead once, right after the engine's first read
// of a subscription. The billing run closes a period from that read, so a
// write afterRead commits lands between the close's read and its plan swap:
// the concurrent writer of the money-path playbook's §5 pattern 3.
type readHookSubAdapter struct {
	*subStoreAdapter
	afterRead func()
}

func (a *readHookSubAdapter) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	sub, err := a.subStoreAdapter.Get(ctx, tenantID, id)
	if hook := a.afterRead; hook != nil {
		a.afterRead = nil
		hook()
	}
	return sub, err
}

// closeLines returns the subscription's one invoice: its subtotal and its
// lines as "type|what|cents", sorted. A base fee line is named by its
// description and a usage line by its meter.
func (f *everyItemFixture) closeLines(t *testing.T, subID string) (int64, []string) {
	t.Helper()
	invs, _, err := f.invoices.List(f.ctx, invoice.ListFilter{TenantID: f.tenantID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	var mine []domain.Invoice
	for _, inv := range invs {
		if inv.SubscriptionID == subID {
			mine = append(mine, inv)
		}
	}
	if len(mine) != 1 || mine[0].BillingReason != domain.BillingReasonSubscriptionCycle {
		t.Fatalf("invoices %+v, want the one cycle invoice of the close", mine)
	}
	lines, err := f.invoices.ListLineItems(f.ctx, f.tenantID, mine[0].ID)
	if err != nil {
		t.Fatalf("list invoice lines: %v", err)
	}
	meterName := map[string]string{f.calls.ID: f.calls.Name, f.seats.ID: f.seats.Name}
	out := make([]string, 0, len(lines))
	for _, li := range lines {
		what := li.Description
		if li.LineType == domain.LineTypeUsage {
			what = meterName[li.MeterID]
		}
		out = append(out, string(li.LineType)+"|"+what+"|"+strconv.FormatInt(li.AmountCents, 10))
	}
	sort.Strings(out)
	return mine[0].SubtotalCents, out
}

// appliedAudit returns "item|old plan|new plan" for every audit row the
// close wrote for an applied plan change of the subscription, sorted.
func (f *everyItemFixture) appliedAudit(t *testing.T, subID string) []string {
	t.Helper()
	var out []string
	f.eachRow(t, `SELECT metadata FROM audit_log WHERE action = $1 AND resource_id = $2`,
		[]any{domain.AuditActionSubscriptionPendingChangeApplied, subID}, func(m map[string]any) {
			out = append(out, metaString(m["item_id"])+"|"+metaString(m["old_plan_id"])+"|"+metaString(m["new_plan_id"]))
		})
	sort.Strings(out)
	return out
}

// appliedEvents returns "item|old plan" for every queued
// subscription.pending_change.applied event of the subscription, sorted.
func (f *everyItemFixture) appliedEvents(t *testing.T, subID string) []string {
	t.Helper()
	var out []string
	f.eachRow(t, `SELECT payload FROM webhook_outbox WHERE event_type = $1`,
		[]any{domain.EventSubscriptionPendingChangeApplied}, func(m map[string]any) {
			if sub, _ := m[domain.WebhookResourceSubscription].(map[string]any); sub["id"] != subID {
				return
			}
			out = append(out, metaString(m["item_id"])+"|"+metaString(m["old_plan_id"]))
		})
	sort.Strings(out)
	return out
}

// eachRow runs query under the tenant's row-level security and decodes its
// one JSON column for each row.
func (f *everyItemFixture) eachRow(t *testing.T, query string, args []any, each func(map[string]any)) {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(f.ctx, query, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("decode: %v", err)
		}
		each(m)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rows: %v", err)
	}
}

// metaString is v when it is a string, else "".
func metaString(v any) string {
	s, _ := v.(string)
	return s
}

func checkLines(t *testing.T, gotSubtotal int64, got []string, wantSubtotal int64, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") || gotSubtotal != wantSubtotal {
		t.Errorf("the close billed %d:\n%s\nwant %d:\n%s", gotSubtotal, strings.Join(got, "\n"), wantSubtotal, strings.Join(want, "\n"))
	}
}

func checkList(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n%s\nwant:\n%s", what, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestCycleClose_PlanChangeOnOneItem_BillsEveryItem_InAdvance: an
// in-advance subscription on plan A (calls) and add-on C (seats), with A's
// item scheduled to change to B at the period end. The close bills June's
// base fee of both items, B's and C's, and May's usage of both meters at
// the prices of May's plans.
func TestCycleClose_PlanChangeOnOneItem_BillsEveryItem_InAdvance(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEveryItemFixture(t, db, "p128-advance")
	a := f.plan(t, "p128-a", 3000, domain.BillInAdvance, &f.calls, "10")
	b := f.plan(t, "p128-b", 6000, domain.BillInAdvance, &f.calls, "20")
	c := f.plan(t, "p128-c", 1000, domain.BillInAdvance, &f.seats, "7")
	subID, itemOn := f.sub(t, "sub-p128-advance", a, c)
	f.schedule(t, itemOn[a.ID], b)

	f.close(t, nil)

	subtotal, lines := f.closeLines(t, subID)
	checkLines(t, subtotal, lines, 6000+1000+5*10+3*7,
		"base_fee|p128-b - base fee (qty 1)|6000",
		"base_fee|p128-c - base fee (qty 1)|1000",
		"usage|calls|50",
		"usage|seats|21",
	)
	checkList(t, "audit rows of the applied change", f.appliedAudit(t, subID), itemOn[a.ID]+"|"+a.ID+"|"+b.ID)
	checkList(t, "pending_change.applied events", f.appliedEvents(t, subID), itemOn[a.ID]+"|"+a.ID)
}

// TestCycleClose_PlanChangeOnOneItem_BillsEveryItem_InArrears: the same
// subscription billed in arrears. The close bills May's base fee of both
// items, A's (May stays on A) and C's, and May's usage of both meters.
func TestCycleClose_PlanChangeOnOneItem_BillsEveryItem_InArrears(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEveryItemFixture(t, db, "p128-arrears")
	a := f.plan(t, "p128-a", 3000, domain.BillInArrears, &f.calls, "10")
	b := f.plan(t, "p128-b", 6000, domain.BillInArrears, &f.calls, "20")
	c := f.plan(t, "p128-c", 1000, domain.BillInArrears, &f.seats, "7")
	subID, itemOn := f.sub(t, "sub-p128-arrears", a, c)
	f.schedule(t, itemOn[a.ID], b)

	f.close(t, nil)

	subtotal, lines := f.closeLines(t, subID)
	checkLines(t, subtotal, lines, 3000+1000+5*10+3*7,
		"base_fee|p128-a - base fee (qty 1)|3000",
		"base_fee|p128-c - base fee (qty 1)|1000",
		"usage|calls|50",
		"usage|seats|21",
	)
	checkList(t, "audit rows of the applied change", f.appliedAudit(t, subID), itemOn[a.ID]+"|"+a.ID+"|"+b.ID)
	checkList(t, "pending_change.applied events", f.appliedEvents(t, subID), itemOn[a.ID]+"|"+a.ID)
}

// TestCycleClose_PlanChangesOnTwoOfThreeItems_BillsEveryItem: an in-advance
// subscription with three items. A changes to B and C to D at the period
// end; E has no change. The close bills June's base fee of all three, on B,
// D and E, and May's usage at the prices of May's plans.
func TestCycleClose_PlanChangesOnTwoOfThreeItems_BillsEveryItem(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEveryItemFixture(t, db, "p128-three")
	a := f.plan(t, "p128-a", 3000, domain.BillInAdvance, &f.calls, "10")
	b := f.plan(t, "p128-b", 6000, domain.BillInAdvance, &f.calls, "20")
	c := f.plan(t, "p128-c", 1000, domain.BillInAdvance, &f.seats, "7")
	d := f.plan(t, "p128-d", 1500, domain.BillInAdvance, &f.seats, "9")
	e := f.plan(t, "p128-e", 500, domain.BillInAdvance, nil, "")
	subID, itemOn := f.sub(t, "sub-p128-three", a, c, e)
	f.schedule(t, itemOn[a.ID], b)
	f.schedule(t, itemOn[c.ID], d)

	f.close(t, nil)

	subtotal, lines := f.closeLines(t, subID)
	checkLines(t, subtotal, lines, 6000+1500+500+5*10+3*7,
		"base_fee|p128-b - base fee (qty 1)|6000",
		"base_fee|p128-d - base fee (qty 1)|1500",
		"base_fee|p128-e - base fee (qty 1)|500",
		"usage|calls|50",
		"usage|seats|21",
	)
	checkList(t, "audit rows of the applied changes", f.appliedAudit(t, subID),
		itemOn[a.ID]+"|"+a.ID+"|"+b.ID, itemOn[c.ID]+"|"+c.ID+"|"+d.ID)
	checkList(t, "pending_change.applied events", f.appliedEvents(t, subID),
		itemOn[a.ID]+"|"+a.ID, itemOn[c.ID]+"|"+c.ID)
}

// TestCycleClose_ChangeCanceledBetweenReadAndSwap_RecordsSwappedItemsOnly:
// A is scheduled to change to B and C to D at the period end. The close
// reads the subscription with both changes due, then C's change is canceled
// before the swap. Only A swaps: the audit log and the webhook events record
// A's change alone, and the close bills June on B and on C, which kept its
// plan.
func TestCycleClose_ChangeCanceledBetweenReadAndSwap_RecordsSwappedItemsOnly(t *testing.T) {
	db := testutil.SetupTestDB(t)
	f := newEveryItemFixture(t, db, "p128-cancel")
	a := f.plan(t, "p128-a", 3000, domain.BillInAdvance, &f.calls, "10")
	b := f.plan(t, "p128-b", 6000, domain.BillInAdvance, &f.calls, "20")
	c := f.plan(t, "p128-c", 1000, domain.BillInAdvance, &f.seats, "7")
	d := f.plan(t, "p128-d", 1500, domain.BillInAdvance, &f.seats, "9")
	subID, itemOn := f.sub(t, "sub-p128-cancel", a, c)
	f.schedule(t, itemOn[a.ID], b)
	f.schedule(t, itemOn[c.ID], d)

	canceled := false
	f.close(t, func() {
		if _, err := f.store.ClearItemPendingPlan(f.ctx, f.tenantID, itemOn[c.ID]); err != nil {
			t.Errorf("cancel C's change after the close's read: %v", err)
		}
		canceled = true
	})
	if !canceled {
		t.Fatal("the billing run never read the subscription: the cancel did not land between the read and the swap")
	}

	subtotal, lines := f.closeLines(t, subID)
	checkLines(t, subtotal, lines, 6000+1000+5*10+3*7,
		"base_fee|p128-b - base fee (qty 1)|6000",
		"base_fee|p128-c - base fee (qty 1)|1000",
		"usage|calls|50",
		"usage|seats|21",
	)
	checkList(t, "audit rows of the applied change", f.appliedAudit(t, subID), itemOn[a.ID]+"|"+a.ID+"|"+b.ID)
	checkList(t, "pending_change.applied events", f.appliedEvents(t, subID), itemOn[a.ID]+"|"+a.ID)
}
