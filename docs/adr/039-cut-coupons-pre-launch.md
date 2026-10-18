# ADR-039: Cut coupons pre-launch

**Status:** Accepted
**Date:** 2026-05-30
**Supersedes:** none. Coupon feature (`internal/coupon`, dashboard
`Coupons.tsx` / `CouponDetail.tsx`, migrations 0025 / 0042-0046)
shipped between Week 4 and Week 8.

## Summary

Coupons are removed: the `internal/coupon` package, its API endpoints, its billing-engine hooks and its dashboard pages were deleted. Velox targets AI and usage-based billing, where peers use credits rather than promo codes, and the credit ledger already covers every discount need seen so far. The `coupons`, `coupon_redemptions` and `customer_discounts` tables stay, so a rebuild can start from them if a signed design partner needs promo codes. Status: active.

## Context

Velox positions as an **AI-native billing engine you can self-host**.
First-design-partner target is AI infra Series A-B — token-meter,
cost-table, LLM-provider-ingestion, embeddable cost dashboards,
commits-and-draw-down territory. Per `project_positioning_wedge`,
Velox is explicitly NOT competing as a generic OSS Stripe Billing
clone — Lago owns that lane.

The coupon surface (`/v1/coupons`, customer-scope assignment,
invoice apply-coupon, redemption ledger, plan/min-amount/duration/
first-time/stackability restrictions) shipped during the Stripe-
parity sprint. It was added because the SaaS-shaped peer set ships
it, not because a DP asked for it.

Two rounds of industry research on 2026-05-29 confirmed coupons are
vestigial for Velox's positioning:

**Traditional SaaS peers (Stripe Billing / Chargebee / Recurly / Lago)**:
ship coupons as table-stakes; zero load-bearing customer evidence in
case studies; treated as parity-required, not differentiator.

**AI-native peers (Orb / Metronome / Lago AI framing / Stripe Token Billing)**:
diverge sharply.
- **Metronome**: no coupon product. Discount intent flows through
  contract overrides + prepaid credits.
- **Orb**: subordinates coupons to "adjustments" — primary discount
  surfaces are amount/percentage/usage discounts at the rate-card
  level, not promo codes.
- **Lago's AI guidance** (getlago.com/solutions/industries/ai +
  getlago.com/blog/credit-based-pricing + the wiki "Refunds, Coupons
  & Credit Notes: why they are different"): explicitly recommends
  **credits over coupons** for usage-based billing.
- **Stripe Token Billing** (docs.stripe.com/billing/token-billing,
  the newest AI-native Stripe product): zero mention of coupons.
  Credit packs + auto-top-up only.

Direct Lago wiki quote: *"A coupon is not a refund. It's an
engagement to discount future invoices."* Their AI billing pages
recommend *"prepaid credits, which customers can purchase and
consume over time"* — coupons absent.

Velox already ships the better primitive: the event-sourced credit
ledger (`internal/credit`) supports prepaid allowances, manual
grants, three-channel allocation on credit notes, and atomic
apply-to-invoice. Coupons duplicate the discount-intent surface
without adding capability the credit ledger lacks.

## Decision

**Cut coupons completely.**

- Delete `internal/coupon/` (package, store, service, handler, tests,
  errors).
- Remove coupon hooks from the billing engine
  (`CouponApplier` interface, `SetCouponApplier`, `RedeemForInvoice`,
  `MarkPeriodsApplied`, `MarkCustomerDiscountPeriodsApplied`,
  `ApplyCouponToInvoice`, the redemption-tracking arrays in cycle
  close + threshold scan).
- Remove the proration coupon applier from `internal/subscription`.
- Remove the credit-note coupon redemption voider.
- Delete dashboard pages `Coupons.tsx` / `CouponDetail.tsx`, the
  `AssignCouponDialog` block on `CustomerDetail.tsx`, the
  `ApplyCouponDialog` block on `InvoiceDetail.tsx`, the
  `/coupons` nav entry and command-palette entry, all coupon
  endpoints from `lib/api.ts`, all `Coupon*` TypeScript types.
- Remove `RecordCouponRedemption` metric + `velox_coupon_redemptions_total`
  counter.
- Remove `AuditActionApplyCoupon` constant + 8 `EventCoupon*` /
  `EventCustomerCoupon*` / `EventInvoiceCouponApplied` webhook event
  constants.
- Delete `docs/coupons-semantics.md`.
- Cut `MANUAL_TEST.md` FLOW C3 (replaced with placeholder pointing
  here).
- Update `README.md` to drop coupons from the shipped-primitives
  list.

**Schema stays.** The `coupons` / `coupon_redemptions` /
`customer_discounts` tables and migrations 0025 / 0042-0046
remain in place. Destructive drops have asymmetric risk pre-launch
(forward migration easy, rollback hard); the tables are cheap to
keep, and a future rebuild would start from the existing schema if
a DP names a coupon use case.

## Consequences

- **Dashboard simplifies.** Coupons nav entry, customer-detail
  Active Discount card, invoice-detail Apply Coupon button all gone.
  Operator surface area shrinks; the credit ledger is the single
  discount/allowance primitive.
- **API surface simplifies.** `/v1/coupons*`, `/v1/customers/{id}/coupon`,
  `/v1/invoices/{id}/apply-coupon` gone. SDK surface shrinks.
- **Net code reduction.** ~2k lines deleted across Go + TypeScript
  + tests.
- **Reversal cost.** A rebuild starts from the existing schema (tables
  intact). Code-side rebuild would be ~1 week if a DP demands it.
- **Audit log historical rows.** Pre-cut `audit_log` rows carrying
  `action=apply_coupon` and `EventCoupon*` events remain readable
  but render as unknown action types in the dashboard's
  AuditLog page. Operator can ignore (pre-launch local DBs only;
  production never ran this code).

## Revisit trigger

Rebuild coupons when:
- A signed design partner names a load-bearing promo-code use case
  (e.g. "we run referral codes" / "Black Friday discount" / "annual
  pre-pay discount" tied to a fixed coupon).
- AI-native peer convergence shifts (e.g. if Orb / Metronome promote
  promo codes to a first-class surface alongside credits).

Until then, the credit-ledger primitive covers every observed
discount intent: operator wants to give the customer X off → operator
issues a credit grant, the engine applies it to the next invoice.

## Industry research sources

- Stripe Billing: https://docs.stripe.com/billing/subscriptions/coupons
- Stripe Token Billing: https://docs.stripe.com/billing/token-billing
- Chargebee Coupons: https://www.chargebee.com/docs/billing/2.0/product-catalog/coupons
- Recurly Coupons: https://docs.recurly.com/docs/coupons
- Lago Coupons (general): https://docs.getlago.com/guide/coupons
- Lago wiki — Refunds vs Coupons vs Credit Notes: https://github.com/getlago/lago/wiki/Refunds,-Coupons-&-Credit-Notes:-why-they-are-different
- Lago AI Billing: https://getlago.com/solutions/industries/ai
- Lago credit-based pricing for AI: https://getlago.com/blog/credit-based-pricing
- Orb Adjustments: https://docs.withorb.com/product-catalog/adjustments
- Metronome Prepaid Credits: https://docs.metronome.com/guides/pricing-packaging/billing-model-guides/prepaid-credits
- Metronome Commits & Draw-Down: https://metronome.com/blog/a-practical-guide-to-enterprise-commit-contracts
