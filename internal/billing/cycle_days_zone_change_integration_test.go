package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/billing"
	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/customer"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/invoice"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/pricing"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/subscription/subscriptiontest"
	"github.com/gethardcount/hardcount/internal/tenant"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// zoneChangeRig is a tenant in Asia/Kolkata billing a 10000 monthly fee in
// advance on the 1st, with the month [Jan 1, Feb 1) IST billed and paid; the
// tenant then moves to UTC. Read in UTC that month is [Dec 31 18:30, Jan 31
// 18:30): its start is off anchor 1.
type zoneChangeRig struct {
	t        *testing.T
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	invoices *invoice.PostgresStore
	subs     *subscription.PostgresStore
	clk      *clock.Fake
	engine   *billing.Engine
	sub      domain.Subscription
}

var (
	zcIST, _ = time.LoadLocation("Asia/Kolkata")
	zcStart  = time.Date(2027, 1, 1, 0, 0, 0, 0, zcIST)
	zcEnd    = time.Date(2027, 2, 1, 0, 0, 0, 0, zcIST) // Jan 31 18:30 UTC
)

func newZoneChangeRig(t *testing.T) *zoneChangeRig {
	t.Helper()
	db := testutil.SetupTestDB(t)
	r := &zoneChangeRig{t: t, ctx: postgres.WithLivemode(context.Background(), false), db: db,
		invoices: invoice.NewPostgresStore(db), subs: subscription.NewPostgresStore(db), clk: clock.NewFake(zcStart.Add(time.Hour))}
	settings := tenant.NewSettingsStore(db)
	r.tenantID = testutil.CreateTestTenant(t, db, "Zone Change Refund")
	r.setZone("Asia/Kolkata")
	plan, err := pricing.NewPostgresStore(db).CreatePlan(r.ctx, r.tenantID, domain.Plan{
		Code: "adv", Name: "Advance", Currency: "USD", BillingInterval: domain.BillingMonthly, Status: domain.PlanActive,
		BaseAmountCents: 10000, BaseBillTiming: domain.BillInAdvance,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}
	creditNoteSvc := creditnote.NewService(creditnote.NewPostgresStore(db), r.invoices, nil, &cnGrantAdapter{credit.NewService(credit.NewPostgresStore(db))})
	creditNoteSvc.SetNumberGenerator(settings)
	r.engine = newRealEngine(db, r.clk)
	r.engine.SetInvoiceVoider(invoice.NewService(r.invoices, clock.Real(), settings))
	r.engine.SetCreditNoteAdjuster(creditNoteSvc)
	r.engine.SetCreditHeadroomReader(creditNoteSvc)

	cust, err := customer.NewPostgresStore(db).Create(r.ctx, r.tenantID, domain.Customer{ExternalID: "cus_zc", DisplayName: "ZC"})
	if err != nil {
		t.Fatalf("create customer: %v", err)
	}
	r.sub, err = r.subs.Create(r.ctx, r.tenantID, domain.Subscription{
		Code: "sub-zc", DisplayName: "ZC", CustomerID: cust.ID,
		Items:  []domain.SubscriptionItem{{PlanID: plan.ID, Quantity: 1}},
		Status: domain.SubscriptionActive, BillingTime: domain.BillingTimeAnniversary,
		StartedAt: &zcStart, CurrentBillingPeriodStart: &zcStart, CurrentBillingPeriodEnd: &zcEnd,
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}
	subscriptiontest.SetBillingCycle(t, r.ctx, db, r.tenantID, r.sub.ID, zcStart, zcEnd, zcEnd, 1)
	r.sub, err = r.subs.Get(r.ctx, r.tenantID, r.sub.ID)
	if err != nil {
		t.Fatalf("read sub: %v", err)
	}
	inv, err := r.engine.BillOnCreate(r.ctx, r.sub)
	if err != nil {
		t.Fatalf("BillOnCreate: %v", err)
	}
	if inv.TotalAmountCents != 10000 {
		t.Fatalf("the month's invoice = %d, want the whole fee, 10000", inv.TotalAmountCents)
	}
	r.pay(inv.ID, zcStart)
	r.setZone("UTC")
	return r
}

func (r *zoneChangeRig) setZone(tz string) {
	r.t.Helper()
	if _, err := tenant.NewSettingsStore(r.db).Update(r.ctx, r.tenantID, func(ts *domain.TenantSettings) error {
		ts.Timezone = tz
		return nil
	}); err != nil {
		r.t.Fatalf("set the zone to %s: %v", tz, err)
	}
}

func (r *zoneChangeRig) pay(invID string, at time.Time) {
	r.t.Helper()
	if _, err := r.invoices.MarkPaid(r.ctx, r.tenantID, invID, "pi_"+invID, at); err != nil {
		r.t.Fatalf("pay %s: %v", invID, err)
	}
}

// cancelCredit writes the relief of a cancel at `at` on the subscription as
// it stands, and returns what it credits.
func (r *zoneChangeRig) cancelCredit(at time.Time) int64 {
	r.t.Helper()
	sub, err := r.subs.Get(r.ctx, r.tenantID, r.sub.ID)
	if err != nil {
		r.t.Fatalf("read sub: %v", err)
	}
	sub.Status, sub.CanceledAt = domain.SubscriptionCanceled, &at
	tx, err := r.db.BeginTx(r.ctx, postgres.TxTenant, r.tenantID)
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	relief, err := r.engine.WriteCancelReliefTx(r.ctx, tx, sub)
	if err != nil {
		r.t.Fatalf("WriteCancelReliefTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatalf("commit the relief: %v", err)
	}
	return relief.CreditedCents
}

// offAnchorQuery is the operator's query in #1709 and the CHANGELOG: the
// credit notes and proration invoices of the periods whose start is off the
// subscription's billing day, read in the tenant's current zone.
const offAnchorQuery = `WITH periods AS (
  SELECT ci.id AS invoice_id, ci.subscription_id, ci.billing_period_start AS ps, ci.billing_period_end AS pe
  FROM invoices ci
  JOIN subscriptions s ON s.id = ci.subscription_id
  JOIN tenant_settings ts ON ts.tenant_id = ci.tenant_id
  CROSS JOIN LATERAL (
    SELECT ci.billing_period_start AT TIME ZONE COALESCE(NULLIF(ts.timezone, ''), 'UTC') AS local_start
  ) z
  WHERE ci.billing_reason IN ('subscription_create', 'subscription_cycle')
    AND ci.status <> 'voided'
    AND s.billing_anchor_day > 0
    AND extract(day FROM z.local_start) <> least(s.billing_anchor_day,
          extract(day FROM date_trunc('month', z.local_start) + interval '1 month - 1 day'))
),
funding AS (
  SELECT p.subscription_id, p.invoice_id FROM periods p
  UNION
  SELECT p.subscription_id, i.id FROM periods p
  JOIN invoices i ON i.subscription_id = p.subscription_id
   AND i.billing_reason = 'subscription_update'
   AND i.source_plan_changed_at >= p.ps AND i.source_plan_changed_at < p.pe
)
SELECT 'credit_note' AS kind, cn.id, cn.credit_note_number AS number, cn.total_cents AS cents, f.subscription_id
FROM funding f
JOIN credit_notes cn ON cn.invoice_id = f.invoice_id
WHERE cn.status <> 'voided'
UNION ALL
SELECT 'proration_invoice', i.id, i.invoice_number, i.total_amount_cents, i.subscription_id
FROM funding f
JOIN invoices i ON i.id = f.invoice_id
WHERE i.billing_reason = 'subscription_update'
ORDER BY 5, 1, 2`

// offAnchorRows returns the kinds of the rows offAnchorQuery lists for the
// rig's subscription.
func (r *zoneChangeRig) offAnchorRows() []string {
	r.t.Helper()
	tx, err := r.db.BeginTx(r.ctx, postgres.TxBypass, "")
	if err != nil {
		r.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	rows, err := tx.QueryContext(r.ctx, `SELECT kind FROM (`+offAnchorQuery+`) q WHERE q.subscription_id = $1`, r.sub.ID)
	if err != nil {
		r.t.Fatalf("the operator's query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			r.t.Fatalf("scan: %v", err)
		}
		kinds = append(kinds, k)
	}
	return kinds
}

// After the move, the month read in UTC starts on Dec 31, off anchor 1. The
// refund divided by the stored anchor's advance from that start, Dec 31 18:30
// to Jan 1 18:30: one day. So a cancel with 16 days left refunded 16 days of
// a 1-day cycle, the whole invoice (10000). The month is 31 days: 16/31.
func TestRefund_AfterAZoneChange_CreditsTheDaysLeftOfTheMonth(t *testing.T) {
	r := newZoneChangeRig(t)
	if got := r.cancelCredit(time.Date(2027, 1, 15, 18, 30, 0, 0, time.UTC)); got != 5161 {
		t.Errorf("credited %d, want 5161 (10000 × 16/31)", got)
	}
	if got := r.offAnchorRows(); len(got) != 1 || got[0] != "credit_note" {
		t.Errorf("the operator's query lists %v, want the cancel's credit note", got)
	}
}

// The close at the end of the month rolls, in UTC, from IST's Feb 1 boundary
// (Jan 31 18:30 UTC) to Feb 1 18:30 on anchor 1: a 1-day seam. Its renewal
// bills one day of a month from Jan 31, 1/28 (357; before, 1/31 by AddDate,
// 323), and a cancel at its start refunds exactly that one day, 357 (before,
// the whole renewal through a 1-day cycle).
func TestRefund_TheSeamAfterAZoneChange_RenewsAndRefundsOneDayOf28(t *testing.T) {
	r := newZoneChangeRig(t)
	r.clk.Set(zcEnd.Add(time.Hour))
	if _, fails := r.engine.RunCycleForTenant(r.ctx, r.tenantID, 50); len(fails) > 0 {
		t.Fatalf("close: %v", fails)
	}
	invs, _, err := r.invoices.List(r.ctx, invoice.ListFilter{TenantID: r.tenantID, CustomerID: r.sub.CustomerID})
	if err != nil {
		t.Fatalf("list invoices: %v", err)
	}
	var renewal domain.Invoice
	for _, inv := range invs {
		if inv.BillingPeriodStart.Equal(zcEnd) {
			renewal = inv
		}
	}
	if renewal.ID == "" {
		t.Fatalf("no invoice for the seam from %v", zcEnd.UTC())
	}
	lines, err := r.invoices.ListLineItems(r.ctx, r.tenantID, renewal.ID)
	if err != nil {
		t.Fatalf("list lines: %v", err)
	}
	var base int64
	for _, li := range lines {
		if li.LineType == domain.LineTypeBaseFee {
			base += li.AmountCents
		}
	}
	if base != 357 {
		t.Fatalf("the seam's renewal base = %d, want 357 (10000 × 1/28)", base)
	}
	r.pay(renewal.ID, zcEnd)
	if got := r.cancelCredit(zcEnd); got != 357 {
		t.Errorf("credited %d, want 357 (the seam's one day, 10000 × 1/28)", got)
	}
	if got := r.offAnchorRows(); len(got) != 1 || got[0] != "credit_note" {
		t.Errorf("the operator's query lists %v, want the cancel's credit note on the seam's renewal", got)
	}
}
