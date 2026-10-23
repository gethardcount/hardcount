package pricing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// The meter-in-use guard (D05). Usage is priced at billing time from the
// meter as it is then: its pricing rules, its default price and its
// aggregation. A rule-version publish waits for the next period (ADR-070),
// but an edit to these fields reprices the period already open. So while a
// subscription uses the meter, an edit that would change the price of usage
// the meter already prices is refused with 409 meter_in_use:
//
//   - editing or deleting a pricing rule;
//   - adding a pricing rule that could take events from a price that bills
//     them now: the meter has a default price, or an existing rule that an
//     event could match together with the new one has the same or a lower
//     priority (equal counts too: ties go to the older rule by created_at,
//     and replicas' clocks can disagree);
//   - pointing the default price at another price, or removing it;
//   - changing the aggregation.
//
// Allowed: a first default price, and a new rule every overlapping rule
// outranks, on a meter with no default price. Both price only usage no price
// on the meter bills now, which is how a new model gets a price. Also allowed:
// pointing the default at another version of the same price (usage resolves a
// price by key as of the period start, ADR-070), and writing values the meter
// already has.
//
// Concurrency: the check runs on the edit's own transaction. LockMeterTx
// orders edits on one meter; CountSubsUsingMeterTx's plan lock orders the
// count against a subscription created on, or moved onto, a plan wiring the
// meter. Not ordered: a plan gaining the meter (plan create or meter attach)
// while a subscription is created on it, all between this check and the
// edit's commit. That subscription's period would be priced as if the edit
// came first.
//
// Version 0.10 replaces this with next-period semantics for every pricing
// edit (D08); until then this guard keeps the open period's prices fixed.

// repricing is what a change func reports: whether the edit changes the
// price of usage the meter already prices, and whether it is a new rule
// (the refusal then says when a new rule is allowed).
type repricing struct {
	reprices bool
	newRule  bool
}

// meterEditGuard returns the in-tx check for an edit on meter: it locks the
// meter, asks change whether the edit changes the price of usage the meter
// already prices, and if so refuses while subscriptions use the meter.
// change gets the meter as locked, not a pre-transaction read.
func (s *Service) meterEditGuard(ctx context.Context, meterID string, change func(tx *sql.Tx, locked domain.Meter) (repricing, error)) func(tx *sql.Tx) error {
	return func(tx *sql.Tx) error {
		locked, err := s.store.LockMeterTx(ctx, tx, meterID)
		if err != nil {
			return err
		}
		r, err := change(tx, locked)
		if err != nil || !r.reprices {
			return err
		}
		n, err := s.store.CountSubsUsingMeterTx(ctx, tx, locked.ID)
		if err != nil {
			return err
		}
		if n > 0 {
			return errs.InvalidState(meterInUseMessage(locked.Key, n, r.newRule, !postgres.Livemode(ctx))).WithCode(errs.CodeMeterInUse)
		}
		return nil
	}
}

// ruleUpsertReprices reports whether upserting rule changes the price of usage
// the locked meter already prices.
func (s *Service) ruleUpsertReprices(ctx context.Context, tx *sql.Tx, locked domain.Meter, rule domain.MeterPricingRule) (repricing, error) {
	existing, err := s.store.GetMeterPricingRuleByPairTx(ctx, tx, locked.ID, rule.RatingRuleVersionID)
	if err == nil {
		same, err := sameRuleTerms(existing, rule)
		return repricing{reprices: !same}, err
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return repricing{}, fmt.Errorf("read pricing rule: %w", err)
	}
	if locked.RatingRuleVersionID != "" {
		// The default price bills every event no rule claims.
		return repricing{reprices: true, newRule: true}, nil
	}
	n, err := s.store.CountRulesOutrankedTx(ctx, tx, locked.ID, rule.DimensionMatch, rule.Priority)
	if err != nil {
		return repricing{}, err
	}
	return repricing{reprices: n > 0, newRule: true}, nil
}

// defaultChangeReprices reports whether pointing the locked meter's default
// price at newID changes what it bills. "" removes the default.
func (s *Service) defaultChangeReprices(ctx context.Context, tenantID string, locked domain.Meter, newID string) (bool, error) {
	oldID := locked.RatingRuleVersionID
	switch {
	case oldID == newID:
		return false, nil
	case oldID == "":
		return false, nil // a first default prices only unclaimed usage
	case newID == "":
		return true, nil
	}
	oldRule, err := s.store.GetRatingRule(ctx, tenantID, oldID)
	if err != nil {
		return false, fmt.Errorf("get rating rule %s: %w", oldID, err)
	}
	newRule, err := s.store.GetRatingRule(ctx, tenantID, newID)
	if err != nil {
		return false, fmt.Errorf("get rating rule %s: %w", newID, err)
	}
	return oldRule.RuleKey != newRule.RuleKey, nil
}

// sameRuleTerms reports whether an upsert would leave the rule as it is.
// dimension_match is compared as JSON, so 1 and 1.0 match as they do in the
// claim.
func sameRuleTerms(existing, rule domain.MeterPricingRule) (bool, error) {
	if existing.AggregationMode != rule.AggregationMode || existing.Priority != rule.Priority {
		return false, nil
	}
	a, err := canonicalMatch(existing.DimensionMatch)
	if err != nil {
		return false, err
	}
	b, err := canonicalMatch(rule.DimensionMatch)
	if err != nil {
		return false, err
	}
	return a == b, nil
}

func canonicalMatch(m map[string]any) (string, error) {
	if m == nil {
		m = map[string]any{}
	}
	// Decode-then-encode so numbers from YAML (int) and JSON (float64) take
	// one form; encoding/json sorts map keys.
	raw, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal dimension_match: %w", err)
	}
	var norm map[string]any
	if err := json.Unmarshal(raw, &norm); err != nil {
		return "", fmt.Errorf("unmarshal dimension_match: %w", err)
	}
	out, err := json.Marshal(norm)
	if err != nil {
		return "", fmt.Errorf("marshal dimension_match: %w", err)
	}
	return string(out), nil
}

// meterInUseMessage is the D05 refusal. It names the count and the way that
// waits for the next period: a new version of the price. In test mode it
// adds the test-clock exception: a version is stamped with real time
// (clock.Now of the catalog call), and a test clock moved forward can open a
// period in simulated time after that stamp, so GetRuleByKeyAsOf resolves
// the new version for the period already open (ADR-070 §4). Live mode has no
// test clocks. For a new rule it also says when one is allowed.
func meterInUseMessage(meterKey string, subs int, newRule, testMode bool) string {
	msg := fmt.Sprintf("%s meter %q, so the prices and aggregation that bill its usage are locked: "+
		"a change now would change what they are billed for usage already recorded. "+
		"To change a rate, publish a new version of the price; it applies from each subscription's next period.",
		subscriptionsUse(subs), meterKey)
	if testMode {
		msg += " For a subscription on a test clock, it can apply to the current simulated period."
	}
	if newRule {
		msg += " A new price can be added only when the meter has no default price " +
			"and no price with the same or a lower priority matches the same events."
	}
	return msg
}

// subscriptionCount reads "1 subscription" or "3 subscriptions".
func subscriptionCount(n int) string {
	if n == 1 {
		return "1 subscription"
	}
	return fmt.Sprintf("%d subscriptions", n)
}
