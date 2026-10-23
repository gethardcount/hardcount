package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/doctor"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/invoice"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// A written-off spend-threshold invoice keeps its window billed; only a void
// reopens it (ADR-066 amendment 2026-10-23). Before, every reader of the
// threshold watermark skipped a written-off invoice, so the period's next
// closer billed the window again from the period start: the in_arrears base
// and the pre-fire usage, a second time. A written-off invoice can still be
// paid (an offline payment, or a card charge that settles late), and its
// credits stay applied, so the customer paid for the window twice.
//
// The rig is thresholdFired: April on one subscription with a 3000c
// in_arrears base, 100 calls on April 10 (1c each) and a peak of 5 on a max
// meter. A spend threshold of 100c with no reset fires on April 20 and bills
// April 1 to 20: base 3000 + 100 calls. The max bucket is deferred to the
// close (ADR-066 §4), so the April close bills the 5 peak in every case.

// invoiceAction posts one operator action on an invoice and wants it to
// succeed.
func (r *lagRig) invoiceAction(id, action string, at time.Time) {
	r.t.Helper()
	if code, body, _ := r.doH(http.MethodPost, "/v1/invoices/"+id+"/"+action, at, map[string]any{"note": "wire"}); code >= 300 {
		r.t.Fatalf("%s invoice %s: %d %v", action, id, code, body)
	}
}

// liveUnbilledUsage is the customer's live credit balance's
// unbilled_usage_cents at `at`: what the open period's close will bill.
func (r *lagRig) liveUnbilledUsage(at time.Time) int64 {
	r.t.Helper()
	out := r.must(http.MethodGet, "/v1/credits/balance/"+r.custID+"/live", at, nil, http.StatusOK)
	// One entry per currency (ADR-129); the rig bills in USD.
	for _, b := range flowList(out, "balances") {
		if flowStr(b, "currency") == "USD" {
			return flowInt(b, "unbilled_usage_cents")
		}
	}
	r.t.Fatalf("live balance has no USD entry: %v", out)
	return 0
}

// wantSubUsageBilledOnce runs the doctor and wants no usage instant of the
// rig subscription's meters on two live invoices.
func (r *lagRig) wantSubUsageBilledOnce() {
	r.t.Helper()
	res := doctor.Run(context.Background(), testutil.AdminPool(r.t), doctor.Checks)
	for _, e := range res.Errors {
		r.t.Errorf("doctor check failed to run: %v", e)
	}
	for _, v := range res.Violations {
		if v.Check.Name == usageOnceCheck && strings.Contains(v.Detail, r.subID) {
			r.t.Errorf("doctor: usage billed twice: %s %s", v.RowID, v.Detail)
		}
	}
}

// TestThresholdWriteOff_CloseBillsOnlyTheResidual is the bug's three
// reproduced forms. The threshold invoice is written off, April closes, and
// then money lands on the written-off invoice: an offline payment, a card
// charge that settles late (MarkPaidCardSettlementTransition, the chokepoint
// of the webhook, the reconciler, the stale-hold sweep and ADR-108
// adoption), or credit applied before the write-off with no payment at all.
// In every form April's close must bill neither the base nor the pre-fire
// calls again: the window is billed once, on the threshold invoice.
//
// The operator preview and the live credit balance must agree with the
// close: writing the invoice off does not change what April's invoice will
// bill.
//
// Mutation check: put 'uncollectible' back in the watermark predicate →
// April's invoice bills base 3000 and 105c of usage in all three forms.
func TestThresholdWriteOff_CloseBillsOnlyTheResidual(t *testing.T) {
	skipShort(t)
	for _, tc := range []struct {
		name       string
		grantCents int64
		pay        func(r *lagRig, thr string, at time.Time)
	}{
		{"offline payment after the close", 0, func(r *lagRig, thr string, at time.Time) {
			r.invoiceAction(thr, "record-payment", at)
		}},
		{"card charge settles after the close", 0, func(r *lagRig, thr string, at time.Time) {
			if _, won, err := invoice.NewPostgresStore(r.db).MarkPaidCardSettlementTransition(uwCtx(at), r.tenantID, thr, "pi_settles_late", at, nil); err != nil || !won {
				r.t.Fatalf("card settle on the written-off invoice: won=%v err=%v, want the uncollectible → paid transition", won, err)
			}
		}},
		{"credit applied before the write-off, nothing paid", 1500, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fireAt, thr := thresholdFiredWithCredit(t, tc.grantCents)
			applied := flowCount(t, r.db, `SELECT credits_applied_cents::int FROM invoices WHERE id = $1`, thr)
			if int64(applied) != tc.grantCents {
				t.Fatalf("credit applied to the threshold invoice at the fire = %dc, want %dc", applied, tc.grantCents)
			}

			before := r.previewUsage(fireAt.Add(30 * time.Minute))
			if live := r.liveUnbilledUsage(fireAt.Add(30 * time.Minute)); live != 5 {
				t.Errorf("live balance unbilled usage before the write-off = %dc, want 5c (the deferred peak)", live)
			}
			r.invoiceAction(thr, "mark-uncollectible", fireAt.Add(time.Hour))
			if after := r.previewUsage(fireAt.Add(90 * time.Minute)); after != before {
				t.Errorf("preview usage after the write-off = %dc, want %dc as before it: a write-off does not reopen the window", after, before)
			}
			if live := r.liveUnbilledUsage(fireAt.Add(90 * time.Minute)); live != 5 {
				t.Errorf("live balance unbilled usage after the write-off = %dc, want 5c as before it: a write-off does not reopen the window", live)
			}

			r.run(lagB.Add(2 * time.Hour))
			if tc.pay != nil {
				tc.pay(r, thr, lagB.Add(3*time.Hour))
				if s := r.invoiceStatus(thr); s != string(domain.InvoicePaid) {
					t.Fatalf("threshold invoice status after the late payment = %s, want paid", s)
				}
			}

			invs := r.invoices()
			apr := r.cycleInvoice(invs, lagP0)
			if apr.sum("base_fee") != 0 || apr.sum("usage") != 5 {
				r.logInvoices(invs)
				t.Errorf("April close billed base %dc and usage %dc, want 0c and 5c (the deferred peak): the written-off threshold invoice already billed the base and April 1-20", apr.sum("base_fee"), apr.sum("usage"))
			}
			if got := usageBilled(invs); got != 100+5 {
				r.logInvoices(invs)
				t.Errorf("usage on all live invoices = %dc, want 105c: 100 on the threshold invoice, the 5 peak at the close", got)
			}
			if tc.grantCents > 0 {
				if got := flowCount(t, r.db, `SELECT credits_applied_cents::int FROM invoices WHERE id = $1`, thr); int64(got) != tc.grantCents {
					t.Errorf("credit applied to the written-off invoice = %dc, want %dc: it stays where it was applied", got, tc.grantCents)
				}
				if bal := r.creditBalance(); bal != 0 {
					t.Errorf("credit balance = %dc, want 0c", bal)
				}
			}
			r.wantSubUsageBilledOnce()
		})
	}
}

// TestThresholdWriteOff_VoidStillReopensTheWindow is the control the fix must
// not change: a voided threshold invoice can never be paid, so April's close
// bills its window again, the base and the pre-fire calls. That holds when
// the invoice was written off first and voided after (uncollectible → voided
// is allowed), as long as the void lands before the close.
func TestThresholdWriteOff_VoidStillReopensTheWindow(t *testing.T) {
	skipShort(t)
	for _, tc := range []struct {
		name    string
		actions []string
	}{
		{"void", []string{"void"}},
		{"write-off, then void", []string{"mark-uncollectible", "void"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fireAt, thr := thresholdFired(t)
			for i, a := range tc.actions {
				r.invoiceAction(thr, a, fireAt.Add(time.Duration(i+1)*time.Hour))
			}
			r.run(lagB.Add(2 * time.Hour))
			invs := r.invoices()
			apr := r.cycleInvoice(invs, lagP0)
			if apr.sum("base_fee") != 3000 || apr.sum("usage") != 100+5 {
				r.logInvoices(invs)
				t.Errorf("April close billed base %dc and usage %dc, want 3000c and 105c: a voided threshold invoice bills nothing, so the close bills the whole month", apr.sum("base_fee"), apr.sum("usage"))
			}
			r.wantSubUsageBilledOnce()
		})
	}
}

// TestThresholdWriteOff_ImmediateCancelBillsOnlyTheResidual: the immediate
// cancel is the period's other closer and reads the same watermark. After a
// write-off (an operator, or dunning's mark_uncollectible final action that
// can also cancel the subscription), the cancel's final invoice must not bill
// the base or the pre-fire calls again.
//
// Mutation check: put 'uncollectible' back in the watermark predicate → the
// final invoice bills a prorated base and the 100 calls again.
func TestThresholdWriteOff_ImmediateCancelBillsOnlyTheResidual(t *testing.T) {
	skipShort(t)
	r, fireAt, thr := thresholdFired(t)
	r.invoiceAction(thr, "mark-uncollectible", fireAt.Add(time.Hour))
	r.cancelNow(fireAt.Add(2 * time.Hour))

	invs := r.invoices()
	var final []lagInvoice
	for _, i := range invs {
		if i.reason == string(domain.BillingReasonSubscriptionCancel) {
			final = append(final, i)
		}
	}
	if len(final) != 1 {
		r.logInvoices(invs)
		t.Fatalf("%d final cancel invoices, want 1 (the deferred peak bills there)", len(final))
	}
	if final[0].sum("base_fee") != 0 || final[0].sum("usage") != 5 {
		r.logInvoices(invs)
		t.Errorf("final cancel invoice billed base %dc and usage %dc, want 0c and 5c: the written-off threshold invoice already billed the base and April 1-20", final[0].sum("base_fee"), final[0].sum("usage"))
	}
	r.wantSubUsageBilledOnce()
}

// TestThresholdWriteOff_CrossIntervalSwapBillsOnlyTheResidual: an immediate
// in_advance swap to the yearly plan closes [Apr 1, swap) on its own invoice
// (BillOnPlanSwapTx), from the threshold watermark read on the swap's tx.
// After a write-off it must not bill the pre-fire calls again.
//
// Mutation check: put 'uncollectible' back in the watermark predicate → the
// swap invoice bills the 100 calls again.
func TestThresholdWriteOff_CrossIntervalSwapBillsOnlyTheResidual(t *testing.T) {
	skipShort(t)
	r := newLagRig(t, "in_advance", false)
	r.ingest(time.Date(2026, 4, 10, 9, 0, 0, 0, time.UTC), 100)
	fireAt := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	r.must(http.MethodPut, "/v1/subscriptions/"+r.subID+"/billing-thresholds", fireAt.Add(-time.Hour),
		map[string]any{"amount_gte": 100, "reset_billing_cycle": false}, http.StatusOK)
	if n := r.scanThresholds(fireAt); n != 1 {
		t.Fatalf("threshold scan fired %d invoices, want 1", n)
	}
	var thr string
	for _, inv := range r.invoices() {
		if inv.reason == string(domain.BillingReasonThreshold) {
			thr = inv.id
		}
	}
	r.invoiceAction(thr, "mark-uncollectible", fireAt.Add(time.Hour))

	swapAt := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	r.must(http.MethodPatch, "/v1/subscriptions/"+r.subID+"/items/"+r.itemA, swapAt,
		map[string]any{"new_plan_id": r.planY, "immediate": true}, http.StatusOK)

	invs := r.invoices()
	var swapInv []lagInvoice
	for _, i := range invs {
		if i.reason == string(domain.BillingReasonSubscriptionCreate) && i.start.Equal(swapAt) {
			swapInv = append(swapInv, i)
		}
	}
	if len(swapInv) != 1 {
		r.logInvoices(invs)
		t.Fatalf("%d swap invoices headed at %v, want 1", len(swapInv), swapAt)
	}
	if got := swapInv[0].sum("usage"); got != 0 {
		r.logInvoices(invs)
		t.Errorf("swap invoice usage = %dc, want 0c: the written-off threshold invoice already billed April 1-14 and nothing came after it", got)
	}
	r.wantSubUsageBilledOnce()
}
