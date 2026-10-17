# ADR-053: Charge the explicit payment method — Velox owns PM selection, Stripe does not pick

**Date:** 2026-06-17
**Status:** Accepted

## Summary

Velox decides which card to charge and names it on every PaymentIntent; Stripe never picks one. The card is the customer's default in Velox's `payment_methods` table, and the same lookup both allows the charge and selects the card. Stripe's `invoice_settings.default_payment_method` is only a display copy, and with no card ID the charge fails loudly instead of guessing. Detaching the default card promotes the newest remaining card in the same transaction, then copies it to Stripe for display (follow-up, 2026-06-17). Status: active.

## Context

Auto-charging an invoice resolves *which saved card to charge* in two independent places that have to agree:

- **Velox** — the `payment_methods` row with `is_default = true AND detached_at IS NULL` (the canonical default card).
- **Stripe** — `customer.invoice_settings.default_payment_method`.

Pre-fix, the charge path passed **only the Stripe customer id** to the PaymentIntent (`internal/payment/stripe.go` `chargeInvoice`) and let **Stripe** select the card: it used `invoice_settings.default_payment_method`, and if that was empty, fell back to the **most-recently-created card** (`internal/payment/stripe_client.go` `CreatePaymentIntent`). Meanwhile the engine *gated* the charge on **Velox's** `is_default` row (`billing.PaymentReadiness.ResolveForCharge` returned a `hasDefaultPM` bool). So one store decided *whether* to charge and a different store decided *which card* — three selectors in total (Velox `is_default`, Stripe `invoice_settings`, Stripe newest-card fallback), and "correct" meant all three happened to coincide.

They can diverge. The operator "Make default" action syncs both stores (`paymentmethods.Service.SetDefault` → `stripe.SetDefaultPaymentMethod`, added 2026-05-29 precisely because an off-session charge had hit a card the operator had just demoted). But the **auto-promotion on detaching the default** (`rebalanceDefault`) updated only Velox's DB, never Stripe — and any future code path that sets the default could forget the same sync. The convergence was emergent ("both sides independently pick newest"), not enforced. This is the same bug class fixed once in May, leaking through a path that was missed.

Industry shape: Stripe's own guidance for off-session/saved-card charges is to **specify the `payment_method` explicitly** on PaymentIntent confirmation rather than rely on implicit customer-default selection ([docs.stripe.com/payments/save-during-payment](https://docs.stripe.com/payments/save-during-payment), [docs.stripe.com/api/payment_intents/create](https://docs.stripe.com/api/payment_intents/create)). Relying on `invoice_settings.default_payment_method` is the convenience path, not the system-of-record path.

## Decision

**Velox is the single source of truth for which card to charge. The charge names that exact card; Stripe never picks.**

1. `PaymentReadiness.ResolveForCharge` now returns the default card's **Stripe PaymentMethod id** (empty when there's no chargeable default) instead of a `hasDefaultPM` bool. The same value is the gate (`!= ""` → ok to charge) **and** the selection.
2. The resolved PM id is threaded through `InvoiceCharger.ChargeInvoice` / `ChargeInvoiceForDunningRetry` → `chargeInvoice` → `PaymentIntentParams.PaymentMethodID`, and `CreatePaymentIntent` sets `PaymentMethod` to it explicitly. The card gated on is the card charged.
3. The Stripe-side selection in `CreatePaymentIntent` — read `invoice_settings.default_payment_method`, else the most-recently-created card — is **deleted**. There is no implicit selector left.
4. **No silent fallback.** An empty PM id at charge time fails loud (`chargeInvoice` returns an error; `CreatePaymentIntent` returns "customer has no payment method on file") rather than guessing a card. The engine's charge gates already require a default PM before reaching the charge, so an empty id here is a wiring bug, surfaced — not absorbed.

Both PM-resolution paths (`paymentReadinessAdapter` for the engine; `compositePaymentSetupStore.GetPaymentSetup` for the manual-charge and dunning-retry paths) already read the same default `payment_methods` row, so every charge entry point — cycle finalize, subscription-create, final-on-cancel, threshold scan, auto-charge sweep, manual operator charge, dunning retry — now charges the explicit PM.

## Why this design

The divergence existed because the *system of record* for PM selection was ambiguous. Picking one owner (Velox, which already holds the gate, the `is_default` flag, and the `detached_at` lifecycle) and having it drive the charge collapses three selectors into one and makes the disagreement **structurally impossible**, rather than patching each new path that must remember to sync Stripe (`feedback_no_belt_and_suspenders`, `feedback_longterm_fixes`). It also matches Stripe's documented off-session pattern and is a strict superset (`feedback_stripe_parity_framing`): we keep Stripe's `invoice_settings` mirror for Stripe-hosted surfaces, it just no longer decides anything.

## Consequences

### Positive
- The card Velox shows as default and gates on is exactly the card charged — no edge case (Stripe-only card, heuristic drift) can charge a different one.
- `customer.invoice_settings.default_payment_method` is demoted to a best-effort cosmetic mirror (Stripe dashboard / Checkout display). A stale or unsynced value can never cause a mischarge.
- The `rebalanceDefault`-doesn't-sync-Stripe seam is neutralised: it was only a bug *because* Stripe's default drove charges. With explicit-PM charging it's cosmetic.
- Failing loud on a missing PM removes the last implicit-selection path (`feedback_no_silent_fallbacks`).

### Risks / open items
- ~~**Cosmetic Stripe sync on auto-promotion**~~ and ~~**detach + promote atomicity**~~ — **both resolved (2026-06-17, follow-up).** Detaching the default and promoting the newest replacement now run in **one store transaction** (`Store.DetachAndRebalance`), so the transient "active cards but no default" window — reachable by a crash *or* a transient DB error on the promote — can no longer be committed; the detach rolls back too if the promote fails. The promoted default is then **best-effort synced to Stripe** (`invoice_settings.default_payment_method`) for dashboard/Checkout display parity — cosmetic only, since the charge path reads Velox's default directly, so a Stripe sync failure logs + audits a `stripe_sync_error` and never blocks the detach. Guarded by `TestDetach_PromotesNewestAndSyncsStripe`, `TestDetach_PromotionStripeSyncFailureStillSucceeds`, and the real-Postgres `TestDetachAndRebalance_AtomicPromote`.

## References
- ADR-041 (no silent tax fallbacks), and the 2026-05-29 `SetDefault` → Stripe sync fix it parallels.
- Memory: `feedback_no_silent_fallbacks`, `feedback_no_belt_and_suspenders`, `feedback_longterm_fixes`, `feedback_stripe_parity_framing`, `feedback_exhaustive_caller_audit`.
- [Stripe: save card during payment](https://docs.stripe.com/payments/save-during-payment), [Stripe PaymentIntent create (`payment_method`)](https://docs.stripe.com/api/payment_intents/create).
