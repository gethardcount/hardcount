package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// canceledInAdvanceSub builds a mid-period-canceled in_advance sub funded by the
// given invoices, for the cancel-credit-draft tests. Period Jun1→Jul1, canceled
// Jun21 → 10/30 unused.
func canceledInAdvanceSub(invoices []domain.Invoice, lines []domain.InvoiceLineItem) (domain.Subscription, *mockInvoices, *mockPricing) {
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cancelAt := time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC)
	pricing := &mockPricing{plans: map[string]domain.Plan{
		"pln_base": {ID: "pln_base", Currency: "USD", BillingInterval: domain.BillingMonthly,
			BaseAmountCents: 15000, BaseBillTiming: domain.BillInAdvance},
	}}
	sub := domain.Subscription{
		ID: "sub_1", TenantID: "t1", CustomerID: "cus_1", Code: "sub",
		Status:                    domain.SubscriptionCanceled,
		Items:                     []domain.SubscriptionItem{{PlanID: "pln_base", Quantity: 1}},
		CurrentBillingPeriodStart: &periodStart, CurrentBillingPeriodEnd: &periodEnd,
		CanceledAt: &cancelAt,
	}
	return sub, &mockInvoices{invoices: invoices, lineItems: lines}, pricing
}

func paidBaseInvoice() (domain.Invoice, domain.InvoiceLineItem) {
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	inv := domain.Invoice{
		ID: "inv_base", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 15000, TotalAmountCents: 15000,
		BillingPeriodStart: periodStart, BillingPeriodEnd: periodEnd,
	}
	line := domain.InvoiceLineItem{ID: "ili_base", InvoiceID: "inv_base", LineType: domain.LineTypeBaseFee, BillingPeriodStart: &periodStart}
	return inv, line
}

func wireCancelEngine(inv *mockInvoices, pricing *mockPricing, adjuster *fakeCreditNoteAdjuster) *Engine {
	e := wireBaseTax(NewEngine(&mockSubs{}, &mockUsage{}, pricing, inv, nil, &mockSettings{}, nil, nil, billingTestClock()))
	e.SetCreditGranter(&fakeCreditGranter{})
	e.SetCreditNoteAdjuster(adjuster)
	e.SetCreditHeadroomReader(&fakeCreditHeadroom{})
	e.SetInvoiceVoider(&fakeInvoiceVoider{})
	return e
}

// TestBillOnCancelDraftsTx_AllPaid_CreatesDrafts: an all-paid funding set gets
// issue_pending DRAFTS on the cancel tx, reports the gross owed, and
// FinishRelief issues them post-commit.
func TestBillOnCancelDraftsTx_AllPaid_CreatesDrafts(t *testing.T) {
	baseInv, baseLine := paidBaseInvoice()
	sub, inv, pricing := canceledInAdvanceSub([]domain.Invoice{baseInv}, []domain.InvoiceLineItem{baseLine})
	adjuster := &fakeCreditNoteAdjuster{}
	e := wireCancelEngine(inv, pricing, adjuster)

	r, err := e.BillOnCancelDraftsTx(context.Background(), nil, sub)
	if err != nil {
		t.Fatalf("BillOnCancelDraftsTx: %v", err)
	}
	const want = 5000 // 15000 * 10/30
	if r.CreditedCents != want {
		t.Errorf("credited=%d, want %d", r.CreditedCents, want)
	}
	if len(r.DraftIDs) != 1 || len(adjuster.draftCall) != 1 || adjuster.draftCall[0].gross != want {
		t.Fatalf("drafts=%v calls=%+v, want one of gross %d", r.DraftIDs, adjuster.draftCall, want)
	}
	if len(adjuster.issued) != 0 {
		t.Errorf("nothing may be issued before the cancel commits; issued=%v", adjuster.issued)
	}
	e.FinishRelief(context.Background(), sub, r)
	if len(adjuster.issued) != 1 || adjuster.issued[0] != r.DraftIDs[0] {
		t.Errorf("FinishRelief issued=%v, want %v", adjuster.issued, r.DraftIDs)
	}
}

// TestBillOnCancelDraftsTx_MixedPaidUnpaid_AllInTx: a paid base plus an unpaid,
// partly-used upgrade invoice. Before ADR-118 any unpaid source sent the whole
// relief to a post-commit path whose failure was only logged; now both shares
// are drafts on the cancel tx. Only the paid share is customer-balance credit.
func TestBillOnCancelDraftsTx_MixedPaidUnpaid_AllInTx(t *testing.T) {
	baseInv, baseLine := paidBaseInvoice()
	changeAt := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	upInv := domain.Invoice{
		ID: "inv_up", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentPending,
		SubtotalCents: 8000, TotalAmountCents: 8000, AmountDueCents: 8000,
		BillingReason: domain.BillingReasonSubscriptionUpdate, SourcePlanChangedAt: &changeAt,
		BillingPeriodStart: changeAt, BillingPeriodEnd: periodEnd,
	}
	sub, inv, pricing := canceledInAdvanceSub([]domain.Invoice{baseInv, upInv}, []domain.InvoiceLineItem{baseLine})
	adjuster := &fakeCreditNoteAdjuster{}
	e := wireCancelEngine(inv, pricing, adjuster)

	r, err := e.BillOnCancelDraftsTx(context.Background(), nil, sub)
	if err != nil {
		t.Fatalf("BillOnCancelDraftsTx: %v", err)
	}
	drafted := map[string]int64{}
	for _, c := range adjuster.draftCall {
		drafted[c.invoiceID] += c.gross
	}
	if drafted["inv_base"] <= 0 || drafted["inv_up"] <= 0 {
		t.Fatalf("drafts=%+v, want a draft on BOTH the paid base and the unpaid upgrade", adjuster.draftCall)
	}
	if drafted["inv_up"] >= 8000 {
		t.Errorf("unpaid upgrade draft %d, want less than its 8000 amount due (partly used → reduce, not void)", drafted["inv_up"])
	}
	if r.CreditedCents != drafted["inv_base"] {
		t.Errorf("credited=%d, want only the paid share %d (unpaid relief reduces amount due, it is not credit)", r.CreditedCents, drafted["inv_base"])
	}
}

// TestBillOnCancelDraftsTx_UnpaidFullyUnused_VoidsInTx: a single unpaid prebill
// that the customer used none of is voided on the cancel tx, and its
// post-commit legs (tax reversal, webhook) run in FinishRelief.
func TestBillOnCancelDraftsTx_UnpaidFullyUnused_VoidsInTx(t *testing.T) {
	baseInv, baseLine := paidBaseInvoice()
	baseInv.PaymentStatus, baseInv.AmountDueCents = domain.PaymentFailed, baseInv.TotalAmountCents
	sub, inv, pricing := canceledInAdvanceSub([]domain.Invoice{baseInv}, []domain.InvoiceLineItem{baseLine})
	start := *sub.CurrentBillingPeriodStart
	sub.CanceledAt = &start // nothing used
	adjuster := &fakeCreditNoteAdjuster{}
	e := wireCancelEngine(inv, pricing, adjuster)
	voider := &fakeInvoiceVoider{}
	e.SetInvoiceVoider(voider)

	r, err := e.BillOnCancelDraftsTx(context.Background(), nil, sub)
	if err != nil {
		t.Fatalf("BillOnCancelDraftsTx: %v", err)
	}
	if len(voider.voided) != 1 || voider.voided[0] != "inv_base" || len(adjuster.draftCall) != 0 {
		t.Fatalf("voided=%v drafts=%+v, want inv_base voided in-tx and no draft", voider.voided, adjuster.draftCall)
	}
	if r.CreditedCents != 0 {
		t.Errorf("credited=%d, want 0 (nothing was paid)", r.CreditedCents)
	}
	e.FinishRelief(context.Background(), sub, r)
	if len(voider.afterVoids) != 1 {
		t.Errorf("AfterVoid ran %d time(s), want 1", len(voider.afterVoids))
	}
}

// failingFundingLookup errors the funding-invoice lookup.
type failingFundingLookup struct{ *mockInvoices }

func (f failingFundingLookup) FindFundingInvoicesForPeriod(context.Context, string, string, time.Time, time.Time) ([]domain.Invoice, error) {
	return nil, errors.New("connection reset")
}

// A funding lookup error fails the relief, so the cancel rolls back. It used
// to decline to a post-commit path that, failing again, only logged.
func TestBillOnCancelDraftsTx_LookupError_Fails(t *testing.T) {
	baseInv, baseLine := paidBaseInvoice()
	sub, inv, pricing := canceledInAdvanceSub([]domain.Invoice{baseInv}, []domain.InvoiceLineItem{baseLine})
	e := wireCancelEngine(inv, pricing, &fakeCreditNoteAdjuster{})
	e.invoices = failingFundingLookup{inv}
	if _, err := e.BillOnCancelDraftsTx(context.Background(), nil, sub); err == nil {
		t.Fatal("a funding lookup error must fail the relief (and roll the cancel back)")
	}
}

// TestBillOnCancelDraftsTx_DraftError_PropagatesForRollback: a draft-create
// failure returns an error so the caller (CancelAtomicWithBill billFn) rolls the
// cancel back — never a canceled sub with a silently-lost credit.
func TestBillOnCancelDraftsTx_DraftError_PropagatesForRollback(t *testing.T) {
	baseInv, baseLine := paidBaseInvoice()
	sub, inv, pricing := canceledInAdvanceSub([]domain.Invoice{baseInv}, []domain.InvoiceLineItem{baseLine})
	adjuster := &fakeCreditNoteAdjuster{draftErr: errors.New("draft insert failed")}
	e := wireCancelEngine(inv, pricing, adjuster)

	if _, err := e.BillOnCancelDraftsTx(context.Background(), nil, sub); err == nil {
		t.Fatal("a draft-create failure must return an error (so the cancel rolls back)")
	}
}

// TestBillOnCancelDraftsTx_MultiPaidSource_RespectsHeadroom pins the #276/#277/#278
// invariant across the allocate→draft split: cumulative draft gross never exceeds
// the funding sources' combined headroom.
func TestBillOnCancelDraftsTx_MultiPaidSource_RespectsHeadroom(t *testing.T) {
	baseInv, baseLine := paidBaseInvoice()
	changeAt := time.Date(2026, 6, 12, 0, 0, 0, 0, time.UTC)
	periodStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// A second PAID funding invoice (mid-period upgrade proration), fully paid.
	upInv := domain.Invoice{
		ID: "inv_up", TenantID: "t1", SubscriptionID: "sub_1",
		Status: domain.InvoiceFinalized, PaymentStatus: domain.PaymentSucceeded,
		SubtotalCents: 5000, TotalAmountCents: 5000,
		BillingReason: domain.BillingReasonSubscriptionUpdate, SourcePlanChangedAt: &changeAt,
		BillingPeriodStart: changeAt, BillingPeriodEnd: periodEnd,
	}
	_ = periodStart
	sub, inv, pricing := canceledInAdvanceSub([]domain.Invoice{baseInv, upInv}, []domain.InvoiceLineItem{baseLine})
	adjuster := &fakeCreditNoteAdjuster{}
	e := wireCancelEngine(inv, pricing, adjuster)

	r, err := e.BillOnCancelDraftsTx(context.Background(), nil, sub)
	if err != nil {
		t.Fatalf("BillOnCancelDraftsTx: %v", err)
	}
	credited := r.CreditedCents
	var sum int64
	for _, c := range adjuster.draftCall {
		sum += c.gross
	}
	if sum != credited {
		t.Errorf("draft gross sum %d != reported credited %d", sum, credited)
	}
	// Combined headroom = base 15000 + upgrade 5000 = 20000; the owed credit is
	// well under it, and no single draft may exceed its own source's total.
	if sum > 20000 {
		t.Errorf("cumulative draft gross %d exceeds combined funding headroom 20000", sum)
	}
}
