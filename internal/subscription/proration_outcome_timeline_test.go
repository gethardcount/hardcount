package subscription

import (
	"strings"
	"testing"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/activity"
)

// TestAddProrationMeta_StampsOutcome locks the audit-metadata shape the
// timeline reads: addProrationMeta must write proration_type / amount /
// invoice_id / currency, and be a no-op on nil (no proration).
func TestAddProrationMeta_StampsOutcome(t *testing.T) {
	t.Run("populates from detail", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, &ProrationDetail{Type: "invoice", AmountCents: 5000, InvoiceID: "inv_1"}, "USD")
		if p["proration_type"] != "invoice" || p["proration_amount_cents"] != int64(5000) ||
			p["proration_invoice_id"] != "inv_1" || p["currency"] != "USD" {
			t.Errorf("stamped payload wrong: %+v", p)
		}
	})
	t.Run("nil detail is a no-op", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, nil, "USD")
		if len(p) != 0 {
			t.Errorf("nil detail must stamp nothing, got %+v", p)
		}
	})
	t.Run("credit/adjustment with no invoice omits invoice_id", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, &ProrationDetail{Type: "adjustment", AmountCents: 5444}, "USD")
		if _, has := p["proration_invoice_id"]; has {
			t.Errorf("adjustment with empty InvoiceID must omit proration_invoice_id: %+v", p)
		}
	})
	t.Run("adjustment names the open invoice it reduced", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, &ProrationDetail{Type: "adjustment", AmountCents: 5444, ClawbackPieces: []domain.CreditShare{{InvoiceID: "inv_src", GrossCents: 5444}}}, "USD")
		if p["adjusted_invoice_id"] != "inv_src" {
			t.Errorf("adjustment must stamp adjusted_invoice_id: %+v", p)
		}
	})
}

// TestDescribeSubscriptionAction_RendersProrationOutcome proves the timeline
// shows WHAT a mid-period change billed — across plan change, quantity
// change, item add, and item remove — once the metadata is present. Pre-fix
// only item_updated rendered it, and the metadata never reached the row at
// all (the atomic wrappers dropped the detail). Amounts arrive as float64
// (audit JSON round-trip), matching the production read path.
func TestDescribeSubscriptionAction_RendersProrationOutcome(t *testing.T) {
	planNames := map[string]string{"pln_pro": "Pro", "pln_starter": "Starter", "pln_seat": "Seat"}

	cases := []struct {
		name, action string
		meta         map[string]any
		wantDesc     string
		wantContains string
	}{
		{
			name:   "plan downgrade → adjustment on unpaid invoice (S6)",
			action: "subscription.item_updated",
			meta: map[string]any{
				"action": "item_plan_changed", "immediate": true,
				"old_plan_id": "pln_pro", "new_plan_id": "pln_starter",
				"proration_type": "adjustment", "proration_amount_cents": float64(5444), "currency": "USD",
			},
			wantDesc: "Plan changed", wantContains: "Open invoice adjusted by $54.44",
		},
		{
			name:   "quantity increase → proration invoice",
			action: "subscription.item_updated",
			meta: map[string]any{
				"action": "item_quantity_changed", "immediate": true, "quantity": float64(3),
				"proration_type": "invoice", "proration_amount_cents": float64(20000), "currency": "USD",
			},
			wantDesc: "Quantity changed", wantContains: "Proration invoice $200.00",
		},
		{
			name:   "item add → proration invoice (was: no monetary outcome)",
			action: string(domain.AuditActionUpdate),
			meta: map[string]any{
				"action": "item_added", "plan_id": "pln_seat", "quantity": float64(2),
				"proration_type": "invoice", "proration_amount_cents": float64(10000), "currency": "USD",
			},
			wantDesc: "Item added", wantContains: "Proration invoice $100.00",
		},
		{
			name:   "item remove → clawback credit (was: 'Item removed' bare)",
			action: string(domain.AuditActionUpdate),
			meta: map[string]any{
				"action": "item_removed", "plan_id": "pln_pro",
				"proration_type": "credit", "proration_amount_cents": float64(5444), "currency": "USD",
			},
			wantDesc: "Item removed", wantContains: "Credit $54.44",
		},
		{
			name:   "a credit limited by the invoices' room says so (ADR-118)",
			action: "subscription.item_updated",
			meta: map[string]any{
				"action": "item_quantity_changed", "immediate": true, "quantity": float64(3),
				"proration_type": "credit", "proration_amount_cents": float64(1000), "uncredited_cents": float64(1500), "currency": "USD",
			},
			wantDesc: "Quantity changed", wantContains: "Credit $10.00 (limited to what the invoices had left)",
		},
		{
			name:   "no room at all: no credit, and why",
			action: string(domain.AuditActionUpdate),
			meta: map[string]any{
				"action": "item_removed", "plan_id": "pln_pro",
				"proration_type": "credit", "proration_amount_cents": float64(0), "uncredited_cents": float64(667), "currency": "USD",
			},
			wantDesc: "Item removed", wantContains: "No credit: the invoices had nothing left to credit",
		},
		{
			name:   "a cancel's limited credit says so",
			action: string(domain.AuditActionCancel),
			meta: map[string]any{
				"prorated_credit_cents": float64(1000), "uncredited_cents": float64(1500), "currency": "USD",
			},
			wantDesc: "Subscription canceled", wantContains: "Prorated credit $10.00 (limited to what the invoices had left)",
		},
		{
			name:   "non-USD currency renders the right symbol, not $",
			action: "subscription.item_updated",
			meta: map[string]any{
				"action": "item_plan_changed", "immediate": true,
				"old_plan_id": "pln_starter", "new_plan_id": "pln_pro",
				"proration_type": "invoice", "proration_amount_cents": float64(5000), "currency": "EUR",
			},
			wantDesc: "Plan changed", wantContains: "€50.00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := describeSubscriptionAction(tc.action, tc.meta, lineLabels{plans: planNames})
			desc, detail := l.Title, activity.MetaText(l)
			if desc != tc.wantDesc {
				t.Errorf("desc: got %q, want %q", desc, tc.wantDesc)
			}
			if !strings.Contains(detail, tc.wantContains) {
				t.Errorf("detail %q: want it to contain %q (proration outcome must show on the timeline)", detail, tc.wantContains)
			}
		})
	}
}
