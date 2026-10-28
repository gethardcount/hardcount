package subscription

import (
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/activity"
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
		addProrationMeta(p, &ProrationDetail{Type: "adjustment", AmountCents: 5444, ClawbackPieces: []domain.CreditShare{{InvoiceID: "inv_src", GrossCents: 5444, DueCents: 5444}}}, "USD")
		if p["adjusted_invoice_id"] != "inv_src" {
			t.Errorf("adjustment must stamp adjusted_invoice_id: %+v", p)
		}
	})
	// A credit that went partly to the balance and partly to an open invoice
	// records the open-invoice part, so the line can show both (T2's state:
	// $15.00 off the unpaid upgrade invoice, $1.67 to the balance).
	t.Run("a mixed credit records the open-invoice part", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, &ProrationDetail{Type: "credit", AmountCents: 1667, ClawbackPieces: []domain.CreditShare{
			{InvoiceID: "inv_up", GrossCents: 1500, DueCents: 1500}, {InvoiceID: "inv_base", GrossCents: 167, ToBalance: true, BalanceCents: 167}}}, "USD")
		if p["adjusted_invoice_id"] != "inv_up" || p["adjusted_cents"] != int64(1500) {
			t.Errorf("mixed credit must stamp the unpaid invoice and its 1500c: %+v", p)
		}
	})
	t.Run("a credit wholly to the balance records no adjustment", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, &ProrationDetail{Type: "credit", AmountCents: 1667, ClawbackPieces: []domain.CreditShare{
			{InvoiceID: "inv_up", GrossCents: 1500, ToBalance: true, BalanceCents: 1500}, {InvoiceID: "inv_base", GrossCents: 167, ToBalance: true, BalanceCents: 167}}}, "USD")
		if _, has := p["adjusted_cents"]; has {
			t.Errorf("all-balance credit must not stamp adjusted_cents: %+v", p)
		}
		if _, has := p["adjusted_invoice_id"]; has {
			t.Errorf("all-balance credit must not stamp adjusted_invoice_id: %+v", p)
		}
	})
	// An open invoice mostly paid by customer credits: only what was due
	// came off it; the rest went back to the balance.
	t.Run("an open invoice records only what came off its amount due", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, &ProrationDetail{Type: "credit", AmountCents: 1333, ClawbackPieces: []domain.CreditShare{
			{InvoiceID: "inv_base", GrossCents: 1333, BalanceCents: 833, DueCents: 500}}}, "USD")
		if p["adjusted_invoice_id"] != "inv_base" || p["adjusted_cents"] != int64(500) {
			t.Errorf("must stamp the 500c that came off the invoice: %+v", p)
		}
	})
	t.Run("a share held on a draft records the draft", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, &ProrationDetail{Type: "adjustment", AmountCents: 1419, ClawbackPieces: []domain.CreditShare{
			{InvoiceID: "inv_draft", GrossCents: 1419, HeldCents: 1419}}}, "USD")
		if p["held_invoice_id"] != "inv_draft" || p["held_cents"] != int64(1419) {
			t.Errorf("must stamp the draft and its 1419c: %+v", p)
		}
		if _, has := p["adjusted_cents"]; has {
			t.Errorf("a held share reduced no open invoice: %+v", p)
		}
		if _, has := p["adjusted_invoice_id"]; has {
			t.Errorf("a held share reduced no open invoice: %+v", p)
		}
	})
	t.Run("two open invoices: no single invoice is named", func(t *testing.T) {
		p := map[string]any{}
		addProrationMeta(p, &ProrationDetail{Type: "adjustment", AmountCents: 2000, ClawbackPieces: []domain.CreditShare{
			{InvoiceID: "inv_u2", GrossCents: 1500, DueCents: 1500}, {InvoiceID: "inv_u1", GrossCents: 500, DueCents: 500}}}, "USD")
		if _, has := p["adjusted_invoice_id"]; has || p["adjusted_invoice_count"] != 2 {
			t.Errorf("two open invoices must stamp a count, not one id: %+v", p)
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
			name:   "a mixed credit shows the balance part and the open-invoice part",
			action: "subscription.item_updated",
			meta: map[string]any{
				"action": "item_quantity_changed", "immediate": true, "quantity": float64(3),
				"proration_type": "credit", "proration_amount_cents": float64(1667), "adjusted_cents": float64(1500), "currency": "USD",
			},
			wantDesc: "Quantity changed", wantContains: "Credit $1.67, open invoice adjusted by $15.00",
		},
		{
			name:   "a credit held on a draft invoice says it waits for the draft",
			action: "subscription.item_updated",
			meta: map[string]any{
				"action": "item_quantity_changed", "immediate": true, "quantity": float64(1),
				"proration_type": "adjustment", "proration_amount_cents": float64(1419), "held_cents": float64(1419), "currency": "USD",
			},
			wantDesc: "Quantity changed", wantContains: "Credit $14.19 held until the draft invoice is finalized",
		},
		{
			name:   "a credit partly to the balance and partly held names both",
			action: "subscription.item_updated",
			meta: map[string]any{
				"action": "item_quantity_changed", "immediate": true, "quantity": float64(1),
				"proration_type": "credit", "proration_amount_cents": float64(1919), "held_cents": float64(1419), "currency": "USD",
			},
			wantDesc: "Quantity changed", wantContains: "Credit $5.00, $14.19 held until the draft invoice is finalized",
		},
		{
			name:   "an adjustment across two open invoices does not name one",
			action: "subscription.item_updated",
			meta: map[string]any{
				"action": "item_quantity_changed", "immediate": true, "quantity": float64(3),
				"proration_type": "adjustment", "proration_amount_cents": float64(2000), "adjusted_invoice_count": float64(2), "currency": "USD",
			},
			wantDesc: "Quantity changed", wantContains: "2 open invoices adjusted by $20.00",
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
