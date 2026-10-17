# ADR-001: PaymentIntent-Only Stripe Integration

**Date:** 2026-04-15
**Status:** Accepted

## Status
Accepted

## Date
2026-04-14

## Summary

Velox charges cards through Stripe PaymentIntents only, never through Stripe Billing or Stripe Invoicing. Velox owns the invoice lifecycle, dunning (retrying failed payments) and payment status, which avoids Stripe Billing's percentage fee (0.5% when this was written, 0.7% on pay-as-you-go today) and keeps one record of each invoice. Each charge is an off-session PaymentIntent on the card Velox chose ([ADR-053](053-explicit-payment-method-at-charge.md)), and one settle step records its outcome ([ADR-049](049-payment-settlement-primitive.md)). The idempotency key is now `velox_inv_{id}_{attempt}`, seeded from a per-invoice attempt counter ([ADR-105](105-charge-idempotency-key-seed.md)), not the bare `velox_inv_{id}` described below. Status: active.

## Context
Velox is a usage-based billing engine that needs to collect payments from customers via Stripe. Stripe offers two paths: (1) use Stripe Billing/Invoicing, which handles invoice creation, payment collection, and dunning automatically, or (2) use the PaymentIntent API directly, managing invoices, dunning, and payment lifecycle ourselves.

Stripe Billing charges 0.5% on top of the standard processing fee. For a platform processing $10M/year in billing volume, that is $50K/year in additional fees passed through to our customers or absorbed. Since Velox is itself a billing engine, delegating invoice management to Stripe Billing would also create an awkward redundancy — two systems tracking the same invoices.

## Decision
Velox uses the Stripe PaymentIntent API exclusively. We create PaymentIntents with `Confirm: true` and `OffSession: true` against saved payment methods. Invoice lifecycle (draft, finalized, paid, voided), dunning (retry schedules, escalation), and payment status tracking are all managed by Velox.

The `payment.Stripe` adapter creates PaymentIntents with idempotency keys derived from invoice IDs (`velox_inv_{id}`), preventing duplicate charges on retries. Webhook handlers for `payment_intent.succeeded` and `payment_intent.payment_failed` close the loop — updating invoice payment status and triggering dunning on failures.

## Consequences

### Positive
- Eliminates the 0.5% Stripe Billing fee for all Velox tenants
- Full control over invoice presentation, numbering, PDF generation, and payment terms
- No split-brain between Stripe's invoice state and ours — Velox is the single source of truth
- Idempotent charge creation prevents duplicate PaymentIntents on retry

### Negative
- We own the entire dunning implementation: retry scheduling, escalation policies, email notifications
- Must handle edge cases Stripe Billing abstracts away: partial payments, webhook ordering, PI state transitions
- More surface area for payment-related bugs in a financially sensitive context

### Trade-offs
- More code to maintain in exchange for zero dependency on Stripe's billing opinions and pricing changes
- We accept the maintenance burden because billing logic is our core product, not a peripheral feature

## Alternatives Considered
- **Stripe Billing/Invoicing**: Rejected due to the 0.5% fee and redundant invoice management. Also creates tight coupling to Stripe's invoice model, limiting our ability to support alternative payment processors later.
- **Stripe Subscriptions API**: Rejected because Velox handles its own subscription lifecycle with usage-based metering. Stripe Subscriptions would fight our pricing models (tiered, graduated, volume, package).
- **Abstract payment provider interface**: Rejected for v1. Velox is Stripe-native today. Premature abstraction would add complexity without a concrete second provider to validate the interface against.
