# ADR-052: Customer tax status — engine-determined by default, manual flag is the override

**Date:** 2026-06-15
**Status:** Accepted

## Summary

The tax engine decides a customer's tax treatment by default. For B2B reverse charge, the operator leaves the customer `standard` and adds a tax ID, and Stripe Tax applies reverse charge only where it is valid (cross-border sales). The `reverse_charge` and `exempt` statuses stay as manual overrides that always force zero tax, and they are the only option with the `manual` and `none` providers. When `reverse_charge` is set on a customer in the seller's own country, Velox logs a warning but does not block the invoice. Status: active.

## Context

The customer billing-profile tax override (`customer_billing_profiles.tax_status` ∈ `standard | exempt | reverse_charge`, plus `tax_id` / `tax_id_type` / `tax_exempt_reason`) was audited end-to-end. The data model is sound and matches industry, but the **interaction between the manual `reverse_charge` flag and the tax engine** had a real correctness gap.

Today, for **every** provider, `reverse_charge` (and `exempt`) short-circuits to $0 **before any tax engine is consulted** (`internal/tax/stripe.go:92-98`, `manual.go:54-60`, `none.go:24-29` → shared `exemptResult`). That means setting a customer to `reverse_charge` forces a zero-rated invoice unconditionally — including when it's **wrong**.

Verified industry shape (the override is real, but it's the *exception* on top of an *engine-determined default*):

- **Stripe** — *"Stripe Tax automatically applies the reverse-charge based on the presence of a tax ID and the jurisdictions involved"* ([docs.stripe.com/tax/zero-tax](https://docs.stripe.com/tax/zero-tax)); the Customer `tax_exempt` enum is `none | exempt | reverse` ([docs.stripe.com/api/customers/object](https://docs.stripe.com/api/customers/object)); the manual flag, *"when set, reverse charge applies regardless of jurisdiction"* — i.e. an unconditional exception. Reverse charge is **cross-border only**: *"supports reverse-charge only for cross-border sales, not within the same country"* ([docs.stripe.com/tax/supported-countries/european-union](https://docs.stripe.com/tax/supported-countries/european-union)). Stripe auto-validates EU VAT IDs against VIES ([docs.stripe.com/billing/customer/tax-ids](https://docs.stripe.com/billing/customer/tax-ids)).
- **Chargebee** — *"reverse charge applies only when the buyer possesses a valid VAT number and is registered in a different EU country"* ([chargebee EU VAT docs](https://www.chargebee.com/docs/billing/2.0/taxes/eu-vat)) — engine-determined from tax ID + cross-border, not a manual per-customer toggle.
- **Avalara** — exemptions are operator-asserted via **structured reason codes** (Streamlined Sales Tax letters A–R), not free text ([Avalara exemption docs](https://developer.avalara.com/avatax/handling-tax-exempt-customers/)).

The concrete bug: an operator who flags a **domestic** customer (same country as the seller's registration) as `reverse_charge` gets a silently zero-rated invoice for a sale that should be taxed — under-collection. Stripe, given the same customer as `standard` + a tax ID, would tax it correctly (reverse charge is cross-border only).

## Decision

Keep the three-status model and the compute-time short-circuit (they are Stripe-parity and correct as an *override* primitive). Reframe and guard:

1. **`standard` + Tax ID is the engine-determined B2B path** — and it already works: the engine forwards the buyer tax ID to Stripe (`internal/tax/stripe.go:308-317`), which auto-applies reverse charge only where valid and VIES-validates the ID. The dashboard now **steers operators to this path** (`web-v2/src/pages/CustomerDetail.tsx`): the `Standard` help text invites adding a Tax ID for B2B; `reverse_charge`/`exempt` are framed as explicit manual overrides ("Forces zero tax…").
2. **`reverse_charge` / `exempt` remain manual, unconditional overrides** — honored as the operator's explicit intent (and the *only* mechanism for `manual`/`none` providers, which have no engine to determine anything). No behavior change to the short-circuit.
3. **Non-blocking domestic-reverse-charge warning** at compute time — `internal/billing/engine.go` `ApplyTaxToLineItems` logs (does not block) when `reverse_charge` is set on a buyer in the seller's own registration country (`domesticReverseCharge(status, customerCountry, ts.CompanyCountry)`). It no-ops when either country is unknown.

No schema change. No new cross-domain wiring.

## Why this design

Reverse charge is a **jurisdiction determination**, and Velox is tax-neutral (`project_tax_neutrality`) — it does not re-implement jurisdiction rules; it defers them to the engine (Stripe). So the long-term-correct path is "assert the fact (tax ID), let the engine decide," with the manual flag reserved for what the engine can't see. This is exactly Stripe's two-layer model, and it's a strict superset (`feedback_stripe_parity_framing`): we keep the manual override Stripe also has, but stop presenting it as the primary mechanism.

The warning (not a block) is deliberate. An adversarial design review (2026-06-15) showed a save-time hard 400 is the wrong mechanism in the wrong layer: it can't fire correctly when `CompanyCountry` is unset (the fresh-tenant default), a naive equality check passes the *least* coherent case (reverse_charge + empty customer country), and `CompanyCountry` is a single scalar that cannot model multi-registration — so a block would wrongly reject legitimate cross-registration reverse charge. We honor operator intent (same trust posture as `exempt`) and surface the likely error rather than silently zero-rating it (`feedback_no_silent_fallbacks`), without the false-positive footguns of a write-time guard.

## Alternatives considered

- **A. Save-time 400 when customer country == `CompanyCountry`.** Rejected: useless when `CompanyCountry` is unset (fires only on the empty/empty coincidence, backwards), blind to multi-registration (false confidence + wrongly blocks legitimate cross-registration RC), and adds a tenant-settings dependency to `customer.Service` (crosses the narrow-interface boundary) for marginal value at zero customers. Both the industry research and the adversarial critique independently said "warning, not save-time rejection."
- **B. Stop short-circuiting `reverse_charge` for `stripe_tax`; defer to Stripe.** Rejected: Stripe's manual `tax_exempt=reverse` is *also* unconditional, so mapping the flag through to Stripe doesn't fix the domestic case — it just moves the same footgun. The jurisdiction-aware path is `standard` + tax ID, which already works; the flag stays a local override.
- **C. Real domestic enforcement + multi-registration model.** Deferred — requires a true seller-registration model (querying Stripe registrations), which is the same class as the deferred VIES work. Trigger: first EU design partner.

## Consequences

### Positive
- Operators are steered to the correct, VIES-validated, jurisdiction-aware B2B path (`standard` + Tax ID) instead of a footgun flag.
- A domestic reverse-charge override no longer zero-rates silently — it leaves an operator-actionable log signal.
- Zero schema change, zero new cross-domain coupling, no behavior change to existing correct flows.

### Risks / open items
- The warning is **log-only** for now; it is not surfaced on the invoice or dashboard. A first-class operator-facing surface is deferred (pairs with the tax-field-propagation consolidation, `project_tax_field_propagation_drift`).
- The signal uses `CompanyCountry`, a **single-registration proxy** — it catches the home-country domestic case only. **Deferred (trigger: first EU DP):** real multi-registration enforcement via a Stripe-registration query; VIES validation (still format-only); per-jurisdiction/per-product exempt scoping + structured exempt reason codes (trigger: first US sales-tax DP).
- `exempt` remains a blanket per-customer override (not per-jurisdiction) — unchanged, pre-existing, same deferral.

## References

- ADR-041 (no silent tax fallbacks), ADR-038 (manual flat-rate, dropped per-customer rate model)
- Memory: `project_reverse_charge_override_vs_engine`, `project_tax_flow_audit_2026_06_14`, `project_tax_neutrality`, `feedback_stripe_parity_framing`, `feedback_no_silent_fallbacks`, `feedback_settings_aspirational_runtime_enforcement`
- [Stripe zero-tax / reverse charge](https://docs.stripe.com/tax/zero-tax), [Stripe EU reverse charge](https://docs.stripe.com/tax/supported-countries/european-union), [Stripe customer tax IDs / VIES](https://docs.stripe.com/billing/customer/tax-ids), [Chargebee EU VAT](https://www.chargebee.com/docs/billing/2.0/taxes/eu-vat), [Avalara exemptions](https://developer.avalara.com/avatax/handling-tax-exempt-customers/)
