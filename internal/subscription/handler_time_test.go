package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// issueTimeCN is the fake credit-note issuer that also records the instant
// the ctx of each Issue carries: the clawback note is issued on it.
type issueTimeCN struct {
	*fakeCNIssuer
	at []time.Time
}

func (c *issueTimeCN) Issue(ctx context.Context, tenantID, id string) (domain.CreditNote, error) {
	at, _ := clock.EffectiveNow(ctx)
	c.at = append(c.at, at)
	return c.fakeCNIssuer.Issue(ctx, tenantID, id)
}

// handlerEntry is an item route run through the handler on a request
// context. It returns the handler's audit rows and the instants of the
// credit notes it issued (clawback notes, or a swap's relief, issued through
// the biller's FinishRelief); wantNotes is how many it must issue.
type handlerEntry struct {
	name      string
	wantNotes int
	run       func(t *testing.T, ctx context.Context, server clock.Clock) (*capturingAudit, []time.Time)
}

func handlerEntries() []handlerEntry {
	const tenantID = "t1"
	wire := func(svc *Service, plans map[string]domain.Plan, src domain.Invoice) (*Handler, *capturingAudit, *issueTimeCN) {
		h := NewHandler(svc)
		h.SetProrationDeps(&plansMock{plans: plans}, &invoicesMock{sourceInvoice: src})
		cn := &issueTimeCN{fakeCNIssuer: &fakeCNIssuer{}}
		h.SetCreditNoteIssuer(cn)
		audits := &capturingAudit{}
		h.SetAuditLogger(audits)
		return h, audits, cn
	}
	seedAt := clock.WithEffectiveNow(context.Background(), proPeriodStart.Add(time.Hour))
	paid := func(subtotal int64) domain.Invoice {
		return domain.Invoice{ID: "src_inv", PaymentStatus: domain.PaymentSucceeded, SubtotalCents: subtotal, TotalAmountCents: subtotal}
	}
	return []handlerEntry{
		{"updateItem: plan downgrade", 1, func(t *testing.T, ctx context.Context, server clock.Clock) (*capturingAudit, []time.Time) {
			store := newMemStore()
			subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
			h, audits, cn := wire(NewService(store, server), map[string]domain.Plan{
				"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
				"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			}, paid(6000))
			body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
			rr := httptest.NewRecorder()
			h.updateItem(rr, updateItemURL(ctx, subID, itemID, body))
			if rr.Code != http.StatusOK {
				t.Fatalf("downgrade: %d %s", rr.Code, rr.Body.String())
			}
			return audits, cn.at
		}},
		{"updateItem: seat decrease", 1, func(t *testing.T, ctx context.Context, server clock.Clock) (*capturingAudit, []time.Time) {
			store := newMemStore()
			subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_seats", proPeriodStart, proPeriodEnd)
			svc := NewService(store, server)
			if _, err := svc.UpdateItem(seedAt, tenantID, subID, itemID, UpdateItemInput{Quantity: int64Ptr(3)}); err != nil {
				t.Fatalf("seed qty=3: %v", err)
			}
			h, audits, cn := wire(svc, map[string]domain.Plan{
				"plan_seats": {ID: "plan_seats", Name: "Seat", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			}, paid(3000))
			body, _ := json.Marshal(UpdateItemInput{Quantity: int64Ptr(1)})
			rr := httptest.NewRecorder()
			h.updateItem(rr, updateItemURL(ctx, subID, itemID, body))
			if rr.Code != http.StatusOK {
				t.Fatalf("seat decrease: %d %s", rr.Code, rr.Body.String())
			}
			return audits, cn.at
		}},
		{"removeItem", 1, func(t *testing.T, ctx context.Context, server clock.Clock) (*capturingAudit, []time.Time) {
			store := newMemStore()
			subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_a", proPeriodStart, proPeriodEnd)
			svc := NewService(store, server)
			if _, err := svc.AddItem(seedAt, tenantID, subID, AddItemInput{PlanID: "plan_b", Quantity: 1}); err != nil {
				t.Fatalf("seed second item: %v", err)
			}
			h, audits, cn := wire(svc, map[string]domain.Plan{
				"plan_a": {ID: "plan_a", Name: "Base A", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
				"plan_b": {ID: "plan_b", Name: "Base B", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			}, paid(3000))
			rr := httptest.NewRecorder()
			h.removeItem(rr, removeItemURL(ctx, subID, itemID))
			if rr.Code != http.StatusNoContent {
				t.Fatalf("remove: %d %s", rr.Code, rr.Body.String())
			}
			return audits, cn.at
		}},
		{"updateItem: cross-interval plan swap (its relief)", 1, func(t *testing.T, ctx context.Context, server clock.Clock) (*capturingAudit, []time.Time) {
			store := newMemStore()
			subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "p_yearly_adv", proPeriodStart, proPeriodEnd)
			plans := map[string]domain.Plan{
				"p_yearly_adv":  {ID: "p_yearly_adv", Name: "Yearly", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 120000, Currency: "USD"},
				"p_monthly_adv": {ID: "p_monthly_adv", Name: "Monthly", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 12000, Currency: "USD"},
			}
			svc := NewService(store, server)
			svc.SetPlanReader(&fakePlanReader{plans: plans})
			b := &reliefTimeBiller{fakeBiller: &fakeBiller{createTxOK: true, swapDraftIDs: []string{"cn_swap_1"}}}
			svc.SetBiller(b)
			h, audits, _ := wire(svc, plans, paid(120000))
			body, _ := json.Marshal(UpdateItemInput{NewPlanID: "p_monthly_adv", Immediate: true})
			rr := httptest.NewRecorder()
			h.updateItem(rr, updateItemURL(ctx, subID, itemID, body))
			if rr.Code != http.StatusOK {
				t.Fatalf("swap: %d %s", rr.Code, rr.Body.String())
			}
			return audits, b.at
		}},
		// addItem issues no credit note (it bills a proration invoice): its rows
		// assert the server clock's read count and the audit rows' instant.
		{"addItem", 0, func(t *testing.T, ctx context.Context, server clock.Clock) (*capturingAudit, []time.Time) {
			store := newMemStore()
			subID, _ := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_a", proPeriodStart, proPeriodEnd)
			h, audits, cn := wire(NewService(store, server), map[string]domain.Plan{
				"plan_a": {ID: "plan_a", Name: "Base A", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
				"plan_b": {ID: "plan_b", Name: "Base B", BaseAmountCents: 1000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
			}, paid(2000))
			body, _ := json.Marshal(AddItemInput{PlanID: "plan_b", Quantity: 1})
			rr := httptest.NewRecorder()
			h.addItem(rr, addItemURL(ctx, subID, body))
			if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
				t.Fatalf("add: %d %s", rr.Code, rr.Body.String())
			}
			return audits, cn.at
		}},
	}
}

// An item route runs on the instant the API's door bound for it: each
// clawback credit note is issued at that instant, each audit row carries it,
// and the server's clock is never read.
func TestItemRoutes_RunOnTheDecidedTime(t *testing.T) {
	for _, e := range handlerEntries() {
		t.Run(e.name, func(t *testing.T) {
			server := &subSteppingClock{at: proNow.Add(30 * 24 * time.Hour)}
			ctx := clock.WithEffectiveNow(auth.WithTenantID(postgres.WithLivemode(context.Background(), false), "t1"), proNow)
			audits, notes := e.run(t, ctx, server)
			if len(notes) != e.wantNotes || !allTimesAt(notes, proNow) || server.reads != 0 {
				t.Errorf("notes issued at %v, server clock reads %d; want %d at %v, none", notes, server.reads, e.wantNotes, proNow)
			}
			if len(audits.entries) == 0 {
				t.Fatal("no audit rows")
			}
			for _, a := range audits.entries {
				if !a.at.Equal(proNow) {
					t.Errorf("audit row %s at %v; want %v", a.action, a.at, proNow)
				}
			}
		})
	}
}

func allTimesAt(got []time.Time, want time.Time) bool {
	for _, at := range got {
		if !at.Equal(want) {
			return false
		}
	}
	return true
}
