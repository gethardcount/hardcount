package subscription

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
)

// TestUpdateItem_Downgrade_UnpaidSource_AdjustsNotRefundableCredit locks the
// ADR-050 unpaid-source rule for a NET CREDIT: a mid-cycle DOWNGRADE on an
// UNPAID in_advance prebill settles the unused slice as a tax-reversing
// ADJUSTMENT credit note against the open invoice (reducing amount_due), NOT a
// refundable balance credit — no cash was funded, so there is nothing to refund.
func TestUpdateItem_Downgrade_UnpaidSource_AdjustsNotRefundableCredit(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	// Downgrade Pro ($60) → Basic ($20). 15/30 remain → net credit = $40 × 15/30 = $20 = 2000.
	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	// UNPAID source prebill: net 6000 + 10% tax = 6600 gross, fully outstanding.
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentPending,
			SubtotalCents: 6000, TaxFacts: domain.TaxFacts{TaxAmountCents: 600}, TotalAmountCents: 6600,
			AmountDueCents: 6600,
		},
	}
	credits := &creditsMock{}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, credits)
	h.SetCreditNoteIssuer(cn)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (downgrade proceeds). body=%s", rr.Code, rr.Body.String())
	}
	// net 2000 grossed up by the source's 6600/6000 ratio = 2200; amount_due
	// 6600 > 2200 so the cap doesn't bite.
	const wantGross = int64(2200)
	if len(cn.calls) != 1 {
		t.Fatalf("CreateAndIssueAdjustment calls: got %d, want 1 (adjustment against the unpaid invoice)", len(cn.calls))
	}
	if cn.calls[0].invoiceID != "src_inv" || cn.calls[0].gross != wantGross {
		t.Errorf("CN adjustment: got invoice=%q gross=%d, want src_inv/%d", cn.calls[0].invoiceID, cn.calls[0].gross, wantGross)
	}
	if cn.calls[0].reason != "subscription_downgrade" {
		t.Errorf("CN reason: got %q, want subscription_downgrade", cn.calls[0].reason)
	}
	// The refundable balance grant must NOT fire — the customer never funded this.
	if len(credits.grantCalls) != 0 {
		t.Errorf("GrantProration calls: got %d, want 0 (unpaid source → adjustment, not refundable credit)", len(credits.grantCalls))
	}

	var resp ItemChangeResult
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Proration == nil || resp.Proration.Type != "adjustment" {
		t.Fatalf("proration: got %+v, want type=adjustment", resp.Proration)
	}
	if resp.Proration.AmountCents != wantGross {
		t.Errorf("proration.amount_cents: got %d, want %d", resp.Proration.AmountCents, wantGross)
	}
}

// TestUpdateItem_Downgrade_UnpaidSource_CapsAtAmountDue proves the adjustment is
// clamped to what's still owed — CreateAndIssueAdjustment rejects a gross above
// amount_due, so settleUnpaidSourceProration pre-caps it (matching the engine's
// unpaid-cancel relief).
func TestUpdateItem_Downgrade_UnpaidSource_CapsAtAmountDue(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	// Only 500 still outstanding (e.g. a partial payment landed) — the 2200 gross
	// credit must clamp to 500.
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentPending,
			SubtotalCents: 6000, TaxFacts: domain.TaxFacts{TaxAmountCents: 600}, TotalAmountCents: 6600,
			AmountDueCents: 500, AmountPaidCents: 6100,
		},
	}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, &creditsMock{})
	h.SetCreditNoteIssuer(cn)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(cn.calls) != 1 || cn.calls[0].gross != 500 {
		t.Fatalf("CN adjustment gross: got %+v, want a single call capped at amount_due=500", cn.calls)
	}
}

// An unpaid source that customer credits were applied to is relieved up to what
// is due PLUS those credits: the note returns the credits it displaces when it
// is issued (ADR-061). Capped at the amount due alone, the 2200 credit shrank
// to 500 and the customer's credits stayed spent on the removed plan.
func TestUpdateItem_Downgrade_UnpaidSource_CoversAppliedCredits(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_pro", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", PaymentStatus: domain.PaymentPending,
			SubtotalCents: 6000, TaxFacts: domain.TaxFacts{TaxAmountCents: 600}, TotalAmountCents: 6600,
			AmountDueCents: 500, CreditsAppliedCents: 6100,
		},
	}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, &creditsMock{})
	h.SetCreditNoteIssuer(cn)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_basic", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%s", rr.Code, rr.Body.String())
	}
	if len(cn.calls) != 1 || cn.calls[0].gross != 2200 {
		t.Fatalf("CN adjustment gross: got %+v, want one call for the full 2200 (500 due + applied credits cover it)", cn.calls)
	}
}

// TestUpdateItem_Upgrade_UnpaidSource_Blocked locks the ADR-050 unpaid-source
// rule for a NET CHARGE: an UPGRADE on an UNPAID in_advance prebill is rejected
// (409) rather than stacking a second receivable on the unpaid period. No
// proration charge invoice is created.
func TestUpdateItem_Upgrade_UnpaidSource_Blocked(t *testing.T) {
	ctx := clock.WithEffectiveNow(context.Background(), proNow)
	tenantID := "t1"

	store := newMemStore()
	subID, itemID := seedSubWithItemAt(t, store, tenantID, "cus_1", "plan_basic", proPeriodStart, proPeriodEnd)
	svc := NewService(store, nil)

	plans := &plansMock{plans: map[string]domain.Plan{
		"plan_basic": {ID: "plan_basic", Name: "Basic", BaseAmountCents: 2000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
		"plan_pro":   {ID: "plan_pro", Name: "Pro", BaseAmountCents: 6000, Currency: "USD", BaseBillTiming: domain.BillInAdvance},
	}}
	invoices := &invoicesMock{
		sourceInvoice: domain.Invoice{
			ID: "src_inv", InvoiceNumber: "INV-1", PaymentStatus: domain.PaymentPending,
			SubtotalCents: 2000, TotalAmountCents: 2000, AmountDueCents: 2000, Currency: "USD",
		},
	}
	cn := &fakeCNIssuer{}

	h := NewHandler(svc)
	h.SetProrationDeps(plans, invoices, &creditsMock{})
	h.SetCreditNoteIssuer(cn)

	body, _ := json.Marshal(UpdateItemInput{NewPlanID: "plan_pro", Immediate: true})
	req := updateItemURL(context.WithValue(ctx, auth.TestTenantIDKey(), tenantID), subID, itemID, body)
	rr := httptest.NewRecorder()
	h.updateItem(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status: got %d, want 409 (upgrade blocked). body=%s", rr.Code, rr.Body.String())
	}
	// No charge invoice stacked on the unpaid period, and no credit note either.
	if len(invoices.createdInvoices) != 0 {
		t.Errorf("createdInvoices: got %d, want 0 (no second receivable on an unpaid period)", len(invoices.createdInvoices))
	}
	if len(cn.calls) != 0 {
		t.Errorf("CN calls: got %d, want 0", len(cn.calls))
	}

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error envelope: %v", err)
	}
	if env.Error.Code != "unpaid_invoice_blocks_change" {
		t.Errorf("error code: got %q, want unpaid_invoice_blocks_change", env.Error.Code)
	}
}

// A clawback split across funding invoices caps an UNPAID one at what is due
// plus the customer credits applied to it. Here the newest funding invoice (the
// upgrade) is unpaid with 500 due and 3900 of credits applied: a 2000 net
// downgrade credit fits on it whole. Capped at the amount due alone it took 500
// and pushed the rest onto the older invoice.
func TestResolveClawbackPieces_UnpaidFundingInvoice_CapIncludesAppliedCredits(t *testing.T) {
	ctx := context.Background()
	baseInv := domain.Invoice{
		ID: "base_inv", PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 2000, TaxFacts: domain.TaxFacts{TaxAmountCents: 200}, TotalAmountCents: 2200,
		CreatedAt: proPeriodStart,
	}
	upInv := domain.Invoice{
		ID: "up_inv", PaymentStatus: domain.PaymentPending,
		SubtotalCents: 4000, TaxFacts: domain.TaxFacts{TaxAmountCents: 400}, TotalAmountCents: 4400,
		AmountDueCents: 500, CreditsAppliedCents: 3900,
		CreatedAt: proPeriodStart.AddDate(0, 0, 5), // newer → a plan downgrade reverses it first
	}
	h := NewHandler(NewService(newMemStore(), nil))
	h.SetProrationDeps(&plansMock{}, &invoicesMock{sourceInvoice: baseInv, fundingInvoices: []domain.Invoice{baseInv, upInv}}, &creditsMock{})
	h.SetCreditNoteIssuer(&fakeCNIssuer{})

	ps, pe := proPeriodStart, proPeriodEnd
	sub := domain.Subscription{ID: "sub_1", CurrentBillingPeriodStart: &ps, CurrentBillingPeriodEnd: &pe}
	pieces, err := h.resolveClawbackPieces(ctx, "t1", sub, domain.ItemChangeTypePlan, 2000)
	if err != nil {
		t.Fatalf("resolveClawbackPieces: %v", err)
	}
	// 2000 net grossed up by the upgrade invoice's 10% tax = 2200, all on it.
	if len(pieces) != 1 || pieces[0].InvoiceID != "up_inv" || pieces[0].GrossCents != 2200 {
		t.Fatalf("pieces = %+v, want one piece of 2200 on up_inv", pieces)
	}
}
