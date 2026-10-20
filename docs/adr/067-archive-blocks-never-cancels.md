# ADR-067: Archive blocks new business — it never cancels, and it never lies

**Date:** 2026-07-02
**Status:** Accepted (shipped in P2a; amended by [ADR-100](100-currency-coherence-guard-ring.md) on 2026-07-26: the billing-profile currency guard)

## Summary

Archiving a customer is refused with 409 while any of their subscriptions still bills (active or trialing, including one with a scheduled cancel); archive never cancels anything. Auto-cancel would run the immediate-cancel path, which issues a final invoice and charges the saved card, so "archive to stop billing" would itself charge cards. Archived customers and archived plans accept no new subscriptions, while existing unpaid invoices stay collectible and dunning continues. Since 2026-10-18 an archived plan also cannot be added as a new item to an existing subscription (#1194); items already on it keep billing. The checker that finds billing subscriptions also refuses a billing-profile currency that differs from their plan currency, and since [ADR-100](100-currency-coherence-guard-ring.md) that currency check counts draft subscriptions too. Status: active, amended 2026-07-26 by ADR-100.

## Context

Archiving a customer did nothing to their subscriptions — billing,
auto-charge, and dunning continued — while the dashboard dialog promised
"billing will stop for their subscriptions" (audit HIGH #4: unauthorized
charges against explicit operator intent). Two candidate fixes: cancel
subscriptions server-side on archive, or block the archive until the
operator cancels them.

## Decision

**Archive = 409-BLOCK while any subscription still bills. Never auto-cancel.**

Auto-cancel is a trap: `Cancel()` runs the immediate-cancel billing path —
final prorated invoice plus **auto-charge of the saved card** — so "archive
to stop billing" would itself charge cards, N times in a bulk archive loop,
and drags the full ADR-056/057 proration machinery into a hide-this-customer
action. Blocking keeps archive a pure visibility/intent flag:

- `customer.Update` rejects active→archived while any subscription is
  non-terminal (active or trialing — including active with a scheduled
  cancel, which bills until the boundary), via a narrow
  `SubscriptionChecker` wired from `api/router.go` (zero peer-imports;
  `SetStripeSyncer` precedent). **Fails closed** when unwired. The 409 names
  the blocking subscription ids.
- `subscription.Create` rejects archived customers (archive = no new
  business). Archived **plans** get the same guard at Create and both
  plan-swap paths (`UpdateItem`/`UpdateItemTx`); existing items on an
  archived plan keep billing (archive ≠ cancel).
- Existing unpaid invoices remain collectible and dunning continues — both
  are invoice-driven and deliberately survive archive.
- The dialog copy now states exactly that. No backfill for already-archived
  customers (pre-launch); no belt-and-suspenders engine-side skip.

**Billing-profile currency guard (same checker):** the profile currency
overrides the plan currency at every invoice writer, so an unvalidated
currency silently re-denominated plan prices ($100 plan invoiced as €100 —
no conversion). `UpsertBillingProfile` now normalizes to canonical
UPPERCASE, format-validates ISO-4217 alpha-3, and 409-rejects a currency
that mismatches a billing subscription's plan currency.

**Hosted-invoice settle poll (shipped alongside, audit HIGH #6):** with
`?paid=1` the public invoice page polls every 3s until the payload's
`payment_status` turns terminal — authoritative backend state, no
heuristics. Pay is suppressed for the entire settle (past the 3-minute cap
the copy turns honest and Pay STAYS hidden; async methods legitimately run
long, and a live Pay button during settle is how cards get charged twice).
Pay returns only on authoritative `payment_status=failed` (no charge
exists). Complements P2b's Checkout session reuse.

## Test locks (mutation-verified)

`TestArchive_BlockedWhileSubsBill` (+ per-status subtests, message names the
sub), `TestArchive_AllowedWhenOnlyTerminalSubs` (+ unarchive),
`TestArchive_FailsClosedWithoutChecker`, `TestBillingProfileCurrency_Guard`
(mismatch 409 / case-normalize / format 400),
`TestCreate_RejectsArchivedCustomer`, `TestCreate_RejectsArchivedPlan`
(+ active-plan control). Each guard deleted → its test fails.
