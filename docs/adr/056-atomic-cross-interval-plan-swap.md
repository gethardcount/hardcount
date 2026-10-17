# ADR-056: Cross-interval plan swap restructures the cycle atomically

**Date:** 2026-06-19
**Status:** Accepted

## Summary

An immediate plan swap across billing intervals (for example monthly to yearly) restructures the cycle in one transaction. The plan change, the period close, the relief for the unused old period ([ADR-118](118-cancel-and-swap-relief-in-the-change-transaction.md)) and the new period's first invoice commit together, or none do. Calls to Stripe, such as the new invoice's tax commit and charge, run after commit. This closed a silent revenue loss: a tax failure on the new invoice used to leave the cycle moved forward with nothing billed. Status: active; the refund double-credit deferred below was fixed on 2026-07-05.

## Context

An adversarial audit of the proration/billing write paths (2026-06-18) found the immediate **cross-interval** plan swap (e.g. monthly→yearly "upgrade to annual", same cadence) ran as **four non-atomic writes across two domain stores** in `applyCrossIntervalPlanSwap`:

1. `BillOnPlanSwapImmediate` — refund the unused OLD in_advance prepayment (credit/credit-note)
2. `ApplyItemPlanImmediately` — write the new plan onto the item
3. `UpdateBillingCycle` — advance the watermark to `(now, newPeriodEnd)`
4. `BillOnCreate` — bill the NEW in_advance period (day-1 invoice)

No shared transaction wrapped them. Two verified money-failure modes:

- **Silent revenue drop (reachable, no crash needed).** Step 3 advanced the watermark *before* step 4 billed. If `BillOnCreate` failed — and it fails on a *routine* tax-calc hiccup (`ApplyTaxToLineItems` → Stripe Tax `customer_data_invalid`, missing address, provider blip) — the code logged `"scheduler catchup will retry"` and returned success. **That comment was false.** The cycle close skips in_advance base segments (`engine.go` Pass-1 guard: `if segPlan.BaseBillTiming == BillInAdvance { continue }`, assuming they were prepaid by `BillOnCreate`), and the watermark had already moved past `(now, newPeriodEnd)`, so the new plan's day-1 base was **never billed — a permanent, silent revenue leak.**
- **Double-credit on full retry (narrow, deferred).** The refund (step 1) carries no retry-stable dedup key. If step 1 commits and step 2 fails, a retry re-enters the swap and re-credits. This needs a precise crash window between two sequential writes *plus* a retry, and is bounded per source invoice by the credit-note headroom cap.

**Reachability.** Both require `base_bill_timing = in_advance`, which is a live, validated API field (default `in_arrears`, ADR-031). Cross-interval same-cadence swaps are a core operation; cross-**cadence** (in_advance↔in_arrears) is rejected upstream. So the silent drop is genuinely reachable; the double-credit is narrow + bounded.

The add / quantity / same-interval plan-change path was **already atomic** — the handler owns one `*sql.Tx` and threads it through the subscription store's `*Tx` methods + `CreateInvoiceWithLineItemsTx` (ADR-030). Cross-interval was the one path left non-atomic, documented as a follow-up.

## Decision

Make the cross-interval swap **atomic via the same coordinator-owned-`*sql.Tx` pattern** the add/qty path already uses — no cross-domain import (the coordinator holds the tx and passes it into each domain store's `*Tx` method). Fix the **reachable** silent drop; **defer** the narrow double-credit (Bug B) with a written trigger.

**One transaction (rolls back fully on any failure):**

```
handler.atomicUpdateItemWithProration owns the tx
  └─ svc.UpdateItemTx → applyCrossIntervalPlanSwapTx(tx, …)
       ├─ ApplyItemPlanImmediatelyTx   (plan write)
       ├─ UpdateBillingCycleTx          (watermark advance)
       └─ BillOnCreateTx                (new in_advance day-1 invoice)   ← in_advance only
```

The watermark can no longer advance unless the new-period invoice commits in the **same** tx. A failed in-tx bill aborts the commit and rolls back the plan write + watermark — **the silent drop is structurally impossible.**

**External steps run POST-commit** (Stripe must never ride a DB tx):

- the OLD-period **refund** (`BillOnPlanSwapImmediate`, computed from the pre-swap snapshot so plan lookups resolve the outgoing rate), and
- the new invoice's **tax commit + auto-charge** (`FinalizeOnCreateInvoice`),

both via `FinalizeCrossIntervalSwap` after `tx.Commit()`. (The tax *calculation* is computed inside the tx, matching the existing atomic proration path — only the tax *commit*, charge, and refund are deferred.)

**Engine factoring.** `BillOnCreate` is split into `buildOnCreateInvoice` (compute + tax, no writes/number) + a slim `BillOnCreate` (build → mint number → `CreateInvoiceWithLineItems` → `FinalizeOnCreateInvoice`, behavior-identical for its existing callers) + `BillOnCreateTx` (build → mint number → `CreateInvoiceWithLineItemsTx`, no finalize) + exported `FinalizeOnCreateInvoice`. `applyCrossIntervalPlanSwapTx` builds a *prospective* in-memory subscription (swapped plan, new period) for `BillOnCreateTx`, because `store.Get` cannot observe the uncommitted tx writes.

**Loud-fail.** The non-atomic fallback (reached only when the handler tx is unwired — tests) no longer logs the false "catchup will retry" WARN; it returns the error.

## What is explicitly deferred

- **Bug B — refund double-credit on full retry.** The post-commit refund is not itself idempotent, so a full operator/SDK retry of a swap could re-credit. It is narrow (precise crash window + retry) and bounded (per-invoice credit-note headroom cap), and closing it cleanly needs either an idempotency-key middleware (closes the class generally) or folding the credit-note's DB write into the tx with a split external tax-reversal. **Trigger to revisit:** a real in_advance design partner, or when idempotency-key middleware lands. Moving the refund post-commit (vs the old refund-before-swap order) does not worsen B and removes the old "tx-rollback strands a refund" failure mode.

## Consequences

- No migration. New `*Tx` store/engine methods + interface additions (`InvoiceWriter.CreateInvoiceWithLineItemsTx`, `Biller.BillOnCreateTx` / `FinalizeOnCreateInvoice`); `*billing.Engine` already satisfies them.
- A rolled-back swap may leave a harmless invoice-number gap (`NextInvoiceNumber` is monotonic; gaps are acceptable, as on the cycle path). A duplicate-period retry inside the tx surfaces as a loud error + rollback (no false "idempotent skip").
- Cross-interval immediate swaps take the atomic path **unconditionally** — including at the period boundary (`remainingDays == 0`), where the handler forces it rather than falling to the non-atomic fallback (`applyCrossIntervalPlanSwapTx` derives the new period from `now`, not from `remainingDays`). The non-atomic `applyCrossIntervalPlanSwap` remains only for the handler-tx-unwired (test) path and now fails loud on a bill failure.
- A uniform-interval guard is enforced on the atomic path too: a multi-item swap that would leave items on mixed intervals is rejected (mirrors the non-atomic path) before any restructure, so unchanged items are never billed over the wrong period.
- Related: ADR-030 (atomic proration), ADR-031 (in_advance bill timing), ADR-055 (anniversary anchor — reused for the swap's re-anchor).
