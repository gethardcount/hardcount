package pricing

import (
	"context"
	"fmt"
	"strings"

	"github.com/sagarsuperuser/velox/internal/errs"
)

// planRef names a plan in an operator-facing refusal.
type planRef struct {
	ID   string
	Code string
}

// guardMeterStaysPriced refuses a write that would leave the meter with no
// pricing rule and no default binding while a plan wiring it has a live
// subscription (#981). Usage on such a meter matches no price, so every
// invoice is built without it. Stripe refuses the same shape: a price that
// has been used cannot be deleted.
//
// The caller decides that the write would leave the meter unpriced; this
// only checks for live subscriptions. "Live" is the set the plan
// immutability guard already uses (CountLiveSubsByPlan): every status but
// canceled and archived, so draft, trialing and active, which can all
// still be invoiced.
//
// Plain check-then-act with no lock, on purpose. A subscription created
// between this check and the write does not touch the meter row, so locking
// the meter would not order the two. Losing that race ends where an
// unguarded write would: invoices are built without the usage, and each one
// still logs it and counts it in velox_usage_unpriced_quantity_total. The
// guard prevents the common mistake; it is not the money-safety net.
//
// It covers only the LAST price. Deleting one of several rules can leave a
// dimension value (a model, say) unmatched on a meter with no default
// price; that usage is reported the same way at invoice time.
func (s *Service) guardMeterStaysPriced(ctx context.Context, tenantID, meterID, meterKey string) error {
	if s.subPlanUsage == nil {
		return nil
	}
	plans, err := s.store.PlansWiringMeter(ctx, tenantID, meterID)
	if err != nil {
		return fmt.Errorf("unpriced-meter guard: plans wiring meter %s: %w", meterID, err)
	}
	var live []string
	for _, p := range plans {
		n, err := s.subPlanUsage.CountLiveSubsByPlan(ctx, tenantID, p.ID)
		if err != nil {
			return fmt.Errorf("unpriced-meter guard: live subscriptions on plan %s: %w", p.ID, err)
		}
		if n > 0 {
			live = append(live, fmt.Sprintf("%s (%d live subscription(s))", p.Code, n))
		}
	}
	if len(live) == 0 {
		return nil
	}
	return errs.InvalidState(fmt.Sprintf(
		"meter %q would have no price left, and plan(s) %s use it: that usage would be left off their invoices. "+
			"Add a default price or another pricing rule to the meter first, or move these subscriptions to a plan without this meter",
		meterKey, strings.Join(live, ", ")))
}
