# ADR-005: Integer Cents for Money

**Date:** 2026-04-15
**Status:** Accepted (amended 2026-06-01 — partially superseded by ADR-045 for per-unit RATE fields)

## Status
Accepted

## Date
2026-04-14

## Summary

Money amounts are stored and computed as `int64` cents (the currency's smallest unit), never as floats, so sums and comparisons are exact. This covers invoice and line amounts and totals, fixed fees and credit-ledger amounts. Amended 2026-06-01: per-unit price rates are now decimals, so sub-cent prices such as $3 per million tokens can be expressed ([ADR-045](045-decimal-per-unit-pricing-rates.md)). The charged amount is still rounded to a whole cent, and tax rates are `NUMERIC(7,4)` decimals ([ADR-042](042-tax-rate-decimal-precision.md)), so the float tax formula below is no longer used. Status: active, amended.

## Context
A billing engine performs thousands of arithmetic operations on monetary values: multiplying unit prices by quantities, computing tiered/graduated pricing, applying tax rates, summing line items, applying credits. The representation of money must be correct under all these operations.

IEEE 754 floating-point arithmetic is inherently imprecise for decimal values. `0.1 + 0.2 = 0.30000000000000004` in every language that uses doubles. In a billing system, these rounding errors compound across line items, tax calculations, and credit applications — producing invoices that are off by a cent, failing reconciliation, and eroding customer trust.

## Decision
All monetary values in Velox are stored and computed as `int64` representing cents (or the smallest currency unit). The `Invoice` struct uses `SubtotalCents`, `TaxAmountCents`, `TotalAmountCents`, `AmountDueCents`, `AmountPaidCents`, `CreditsAppliedCents`. Line items use `UnitAmountCents`, `AmountCents`, `TotalAmountCents`. Credit ledger entries use `AmountCents` and `BalanceAfter` in cents.

Conversion to display format (dollars, euros) happens exclusively at the API boundary — JSON serialization in handlers. All internal computation, storage, and comparison uses integer cents. Tax calculation is the one place where floating-point appears: `int64(math.Round(float64(subtotal) * taxRate / 100))`, converting back to integer immediately after the multiplication.

> **Amendment (2026-06-01, ADR-045 — partial supersession).** The per-unit RATE fields multiplied by a (decimal) quantity — `RatingRuleVersion.FlatAmountCents`, `RatingTier.UnitAmountCents`, `RatingRuleVersion.OverageUnitAmountCents` — are now arbitrary-precision `decimal.Decimal` (PostgreSQL `NUMERIC`), not `int64` cents. Sub-cent-per-unit rates (e.g. $3.00 / 1M tokens = 0.0003¢/token) are inexpressible as integer cents; ADR-045 adopts the Stripe `unit_amount_decimal` model. Everything else in this ADR stands: fixed fees (`base_amount_cents`, `package_amount_cents`), **all invoice line amounts and totals**, credit-ledger amounts, and tax remain `int64` cents, and `ComputeAmountCents` still rounds the final charge to a whole cent. Only the RATE gained precision — you still cannot bill a fractional cent.

## Consequences

### Positive
- Zero floating-point drift across any chain of additions, subtractions, and comparisons
- Equality checks on monetary values are exact (`==` not approximate)
- Database storage as `BIGINT` — no precision/scale configuration, no `NUMERIC` type needed
- Stripe's API uses integer cents natively, so no conversion needed at the payment boundary

### Negative
- Developers must remember to work in cents and convert at boundaries, which is unintuitive
- Division operations (e.g., computing unit price from total and quantity) can lose precision — we accept truncation and document where it occurs
- Multi-currency support requires knowing the smallest unit per currency (cents for USD/EUR, but yen for JPY has no subunit)

### Trade-offs
- `int64` gives us a maximum of ~$92 quadrillion in cents, which is sufficient. We trade human readability in the database (`4999` vs `49.99`) for computational correctness.

## Alternatives Considered
- **`float64`**: The default in many systems. Rejected outright — billing is one domain where floating-point errors are not acceptable. A 1-cent error on 10,000 invoices is a $100 reconciliation problem.
- **`decimal` / `NUMERIC` type**: Arbitrary-precision decimals avoid IEEE 754 issues but add complexity (Go has no native decimal type; libraries like `shopspring/decimal` introduce allocations and API friction). PostgreSQL `NUMERIC` works but is slower than `BIGINT` for aggregations. Rejected for v1 in favor of the simpler int64 approach that Stripe, Square, and most payment processors use.
- **String-encoded decimals**: Used by some financial APIs. Avoids precision issues but makes arithmetic impossible without parsing. Rejected.
