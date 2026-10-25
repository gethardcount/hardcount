package billing

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/tax"
)

// negativeRejectingProvider mirrors the real Stripe Tax constraint that
// caught issue #556: any negative line amount in the calculation request is
// rejected outright. Delegates the arithmetic to the manual provider so the
// tests assert the ENGINE's request shaping, not provider math.
type negativeRejectingProvider struct{ inner tax.Provider }

func (*negativeRejectingProvider) Name() string { return "stripe_tax" }
func (p *negativeRejectingProvider) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	for i, li := range req.LineItems {
		if li.AmountCents < 0 {
			return nil, fmt.Errorf("stripe tax: Invalid non-negative integer (param line_items[%d][amount])", i)
		}
	}
	return p.inner.Calculate(ctx, req)
}
func (*negativeRejectingProvider) Commit(_ context.Context, _, _ string) (string, error) {
	return "", nil
}
func (*negativeRejectingProvider) Reverse(_ context.Context, _ tax.ReversalRequest) (*tax.ReversalResult, error) {
	return &tax.ReversalResult{}, nil
}

// storedSplitPair is the ADR-048 Phase C line shape as it exists on a
// persisted proration invoice: the negative unused-time credit plus the
// positive remaining-time charge. This is what the retry family feeds back
// into ApplyTaxToLineItems.
func storedSplitPair() []domain.InvoiceLineItem {
	return []domain.InvoiceLineItem{
		{Description: "Unused time on Starter", AmountCents: -1200, Quantity: 1},
		{Description: "Remaining time on Pro", AmountCents: 3000, Quantity: 1},
	}
}

func TestCollapseTaxRequestLines_FastPathIsPositional(t *testing.T) {
	lines := []domain.InvoiceLineItem{
		{AmountCents: 1000, Quantity: 1, TaxCode: "txcd_a"},
		{AmountCents: 0, Quantity: 2},
		{AmountCents: 2500, Quantity: 1},
	}
	req, groups, err := collapseTaxRequestLines(lines, "txcd_default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req) != 3 || len(groups) != 3 {
		t.Fatalf("got %d req lines / %d groups, want 3/3", len(req), len(groups))
	}
	for i := range lines {
		if req[i].AmountCents != lines[i].AmountCents || len(groups[i]) != 1 || groups[i][0] != i {
			t.Errorf("line %d not positionally passed through: req=%+v group=%v", i, req[i], groups[i])
		}
	}
}

func TestCollapseTaxRequestLines_SplitPairCollapsesToNet(t *testing.T) {
	req, groups, err := collapseTaxRequestLines(storedSplitPair(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req) != 1 {
		t.Fatalf("got %d request lines, want 1 (pair collapsed)", len(req))
	}
	if req[0].AmountCents != 1800 {
		t.Errorf("collapsed amount = %d, want 1800 (the net the create path taxed)", req[0].AmountCents)
	}
	if len(groups) != 1 || len(groups[0]) != 2 || groups[0][0] != 1 || groups[0][1] != 0 {
		t.Errorf("groups = %v, want [[1 0]] (absorber first)", groups)
	}
}

func TestCollapseTaxRequestLines_NoAbsorberFailsLoud(t *testing.T) {
	_, _, err := collapseTaxRequestLines([]domain.InvoiceLineItem{
		{Description: "orphan credit", AmountCents: -500, Quantity: 1},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "no positive line") {
		t.Fatalf("want loud no-absorber error, got %v", err)
	}
}

func TestCollapseTaxRequestLines_TaxCodeMismatchFailsLoud(t *testing.T) {
	_, _, err := collapseTaxRequestLines([]domain.InvoiceLineItem{
		{AmountCents: -500, Quantity: 1, TaxCode: "txcd_services"},
		{AmountCents: 3000, Quantity: 1, TaxCode: "txcd_goods"},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "txcd_services") {
		t.Fatalf("want loud cross-tax-code error naming the code, got %v", err)
	}
}

func TestCollapseTaxRequestLines_NetNegativeFailsLoud(t *testing.T) {
	_, _, err := collapseTaxRequestLines([]domain.InvoiceLineItem{
		{AmountCents: -3000, Quantity: 1},
		{AmountCents: 1200, Quantity: 1},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "nets negative") {
		t.Fatalf("want loud net-negative error, got %v", err)
	}
}

func TestExpandTaxResultLines_PartitionMatchesCreateTimeSplit(t *testing.T) {
	lines := storedSplitPair()
	_, groups, err := collapseTaxRequestLines(lines, "")
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	// Provider taxed the collapsed net 1800 at 10% → 180, exclusive mode
	// (NetAmountCents == request amount).
	res := []tax.ResultLine{{Ref: "line_0", NetAmountCents: 1800, TaxAmountCents: 180, TaxRate: 10}}

	out := mustExpand(t, res, groups, lines)
	if len(out) != 2 {
		t.Fatalf("got %d expanded lines, want 2", len(out))
	}
	// splitUpgradeProration's rule: creditTax = RHTE(T×credit, net) = −120;
	// chargeTax = T − creditTax = 300. The retried invoice must reproduce
	// the create-time split to the cent.
	if out[0].TaxAmountCents != -120 || out[1].TaxAmountCents != 300 {
		t.Errorf("partition = (%d, %d), want (−120, 300)", out[0].TaxAmountCents, out[1].TaxAmountCents)
	}
	if out[0].NetAmountCents != -1200 || out[1].NetAmountCents != 3000 {
		t.Errorf("exclusive nets = (%d, %d), want stored amounts (−1200, 3000)", out[0].NetAmountCents, out[1].NetAmountCents)
	}
	if out[0].TaxRate != 10 || out[1].TaxRate != 10 {
		t.Errorf("rates = (%g, %g), want both 10", out[0].TaxRate, out[1].TaxRate)
	}
	if out[0].TaxAmountCents+out[1].TaxAmountCents != 180 {
		t.Errorf("per-line taxes sum to %d, want the invoice tax 180", out[0].TaxAmountCents+out[1].TaxAmountCents)
	}
}

func TestExpandTaxResultLines_BankersRemainderStaysOnAbsorber(t *testing.T) {
	lines := storedSplitPair()
	_, groups, _ := collapseTaxRequestLines(lines, "")
	// An odd total that doesn't divide evenly: T=181 on net 1800.
	// creditTax = RHTE(181×−1200, 1800) = RHTE(−120.67) = −121; charge = 302.
	res := []tax.ResultLine{{Ref: "line_0", NetAmountCents: 1800, TaxAmountCents: 181}}
	out := mustExpand(t, res, groups, lines)
	if out[0].TaxAmountCents != -121 || out[1].TaxAmountCents != 302 {
		t.Errorf("partition = (%d, %d), want (−121, 302)", out[0].TaxAmountCents, out[1].TaxAmountCents)
	}
	if out[0].TaxAmountCents+out[1].TaxAmountCents != 181 {
		t.Errorf("sum = %d, want 181 exactly", out[0].TaxAmountCents+out[1].TaxAmountCents)
	}
}

func TestExpandTaxResultLines_InclusiveScalesNets(t *testing.T) {
	lines := storedSplitPair()
	_, groups, _ := collapseTaxRequestLines(lines, "")
	// Inclusive: provider carved net 1636 (+164 tax) out of the gross 1800.
	res := []tax.ResultLine{{Ref: "line_0", NetAmountCents: 1636, TaxAmountCents: 164}}
	out := mustExpand(t, res, groups, lines)
	if got := out[0].NetAmountCents + out[1].NetAmountCents; got != 1636 {
		t.Errorf("nets sum to %d, want the carved 1636", got)
	}
	if got := out[0].TaxAmountCents + out[1].TaxAmountCents; got != 164 {
		t.Errorf("taxes sum to %d, want 164", got)
	}
	if out[0].NetAmountCents >= 0 {
		t.Errorf("credit member net = %d, want negative", out[0].NetAmountCents)
	}
}

func TestExpandTaxResultLines_MissingProviderLinesLeaveRefEmpty(t *testing.T) {
	lines := []domain.InvoiceLineItem{{AmountCents: 1000, Quantity: 1}, {AmountCents: 2000, Quantity: 1}}
	_, groups, _ := collapseTaxRequestLines(lines, "")
	out := mustExpand(t, []tax.ResultLine{{Ref: "line_0", NetAmountCents: 1000, TaxAmountCents: 100}}, groups, lines)
	if out[0].Ref == "" {
		t.Errorf("produced line 0 lost its ref")
	}
	if out[1].Ref != "" {
		t.Errorf("unproduced line 1 has ref %q, want empty (leave-untouched marker)", out[1].Ref)
	}
}

// TestApplyTaxToLineItems_RetrySplitPair_ProviderNeverSeesNegative is the
// issue #556 regression: the retry family re-runs the chokepoint against the
// STORED split pair; a provider with the real Stripe Tax constraint must
// still succeed, and the per-line taxes must land exactly where the
// create-time split put them. Reverting collapseTaxRequestLines turns this
// red at the provider boundary — the mutation-verify is structural.
func TestApplyTaxToLineItems_RetrySplitPair_ProviderNeverSeesNegative(t *testing.T) {
	e := &Engine{
		settings: &taxSettings{provider: "stripe_tax", rateBP: 1000, name: "Sales Tax"},
		taxProviders: stubResolver(&negativeRejectingProvider{
			inner: tax.NewManualProvider(10, "Sales Tax"),
		}),
	}
	lines := storedSplitPair()

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 1800, 0, lines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxStatus == domain.InvoiceTaxPending {
		t.Fatalf("deferred (%s: %s) — the provider saw a negative line", r.TaxErrorCode, r.TaxPendingReason)
	}
	if r.TaxAmountCents != 180 {
		t.Errorf("invoice tax = %d, want 180 (10%% of the net 1800)", r.TaxAmountCents)
	}
	if lines[0].TaxAmountCents != -120 || lines[1].TaxAmountCents != 300 {
		t.Errorf("per-line = (%d, %d), want the create-time split (−120, 300)",
			lines[0].TaxAmountCents, lines[1].TaxAmountCents)
	}
	if lines[0].TotalAmountCents != -1320 || lines[1].TotalAmountCents != 3300 {
		t.Errorf("per-line totals = (%d, %d), want (−1320, 3300)",
			lines[0].TotalAmountCents, lines[1].TotalAmountCents)
	}
}

// TestApplyTaxToLineItems_ManualRetryPreservesSplitPartition pins the second
// #556 instance: pre-fix, a manual-provider retry on a stored split pair
// clamped the negative base and redistributed the whole tax onto the charge
// line (0 / 180), silently rewriting the create-time split (−120 / 300).
func TestApplyTaxToLineItems_ManualRetryPreservesSplitPartition(t *testing.T) {
	e := newManualEngine(1000, "Sales Tax", nil)
	lines := storedSplitPair()

	r, err := e.ApplyTaxToLineItems(context.Background(), "t1", "cus_1", "USD", 1800, 0, lines)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.TaxAmountCents != 180 {
		t.Errorf("invoice tax = %d, want 180", r.TaxAmountCents)
	}
	if lines[0].TaxAmountCents != -120 || lines[1].TaxAmountCents != 300 {
		t.Errorf("per-line = (%d, %d), want (−120, 300) — the clamp-and-redistribute bug is back",
			lines[0].TaxAmountCents, lines[1].TaxAmountCents)
	}
}

func mustExpand(t *testing.T, res []tax.ResultLine, groups [][]int, lines []domain.InvoiceLineItem) []tax.ResultLine {
	t.Helper()
	out, err := expandTaxResultLines(res, groups, lines)
	if err != nil {
		t.Fatalf("expandTaxResultLines: %v", err)
	}
	return out
}
