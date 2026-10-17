# ADR-071: Credit expiry retires the block — atomically, and retirement wins

**Date:** 2026-07-03
**Status:** Accepted (shipped in P14; designed by the P14 adversarial panel — 3 lenses, all SHIP-WITH-FIXES; consolidated protocol in the remediation plan §4.4)

## Summary

When a credit grant expires, one transaction writes the expiry entry for the unused amount and marks the grant fully used (`consumed_cents = amount_cents`), so expired credit can never be spent again. The step, `ExpireGrantAtomic`, re-reads the unused amount under the same row lock that spending credit takes, so a concurrent spend cannot make it expire more than is left. Once expiry commits, a backdated apply draws nothing from that grant; there is no un-expire. A clawback (taking credit back) that unexpired credit cannot cover now fails instead of driving the ledger negative, and positive adjustments carry the same $1M cap as grants. Status: active.

## Context

The credit ledger uses Orb's block model: positive entries (grants,
positive adjustments, reversals) are drainable blocks whose
`consumed_cents` tracks FIFO-attributed drains. The expiry sweep,
though, only *journaled*: it appended the `-remaining` expiry entry and
never touched the grant's `consumed_cents`. Two money bugs followed:

1. **Stale-snapshot over-expiry (negative ledger).** The sweep computed
   `remaining` from a candidate list read on an already-closed
   transaction, then appended in a new one. A backdated
   `ApplyToInvoiceAt` (`at < expires_at` passes the block-eligibility
   filter on a not-yet-retired grant) could drain the grant between the
   list and the append — the sweep then expired headroom that no longer
   existed, driving `SUM(amount_cents)` negative.
2. **Backdated re-drain of "expired" grants.** After the expiry entry
   existed, the grant still matched `consumed_cents < amount_cents`, so
   any backdated apply could re-drain it — spending credit the ledger
   had already written off, surfacing as the drainable-vs-balance drift
   WARN at best.

The only replay guard was a `NOT EXISTS (… description LIKE 'Expired
grant %…')` filter — operator-visible display copy doubling as an
idempotency key, evaluated check-then-act across transactions.

## Decisions

1. **Expiry retires the block: `consumed_cents = amount_cents`, in the
   same transaction as the expiry entry.** This does not conflict with
   the append-only doctrine — `consumed_cents` is a mutable *read-model*
   column (every drain already updates it); event history stays
   append-only. `ExpireGrantAtomic` is the single primitive: take the
   customer advisory lock (balance_after correctness among
   AppendEntry-path writers), take the same `FOR UPDATE` row lock the
   apply/adjust paths hold (the ONLY cross-discipline mutual exclusion),
   re-read `remaining` under that lock, no-op cleanly when `remaining <=
   0`, CAS-flip `consumed_cents` (`WHERE consumed_cents < amount_cents`
   is the exactly-once gate), and append the `-remaining` entry stamped
   `created_at = expires_at`. Any two-transaction split strands money:
   entry-then-flip leaves phantom headroom the candidate queries never
   re-list; flip-then-entry leaves balance that can never drain or
   expire.
2. **Retirement wins over backdated apply.** Once the retirement
   commits, a backdated apply drains $0 from the grant for ANY `at` —
   there is no un-expire. Before retirement commits, a backdated apply
   may legitimately drain the grant (the sweep recomputes and expires
   only what's left). The residual entitlement loss — a replayed
   cross-advance apply that *would* have drained the grant pre-expiry
   instead pays from other blocks or cash — is accepted and bounded to
   partial-failure replays that interleave with the sweep.
3. **`consumed_cents` is the single structural exclusion.** Migration
   0127 retro-retires every grant the pre-fix sweep expired (exact-match
   join on the legacy `'Expired grant <id>'` description, used one last
   time — this also closes headroom already re-opened by bug 2), after
   which both candidate queries drop the description-LIKE filter. The
   sweep's candidate lists are discovery only; correctness lives in the
   locked re-read.
4. **Clawbacks must fully attribute or fail loud.** `AdjustAtomic`'s
   raw-SUM balance gate counts expired-but-unswept headroom that
   `drainPositiveBlocks` rightly refuses to drain; it previously
   discarded the drain result and inserted the deduction anyway —
   sequential negative ledger, no race required. It now errors when the
   drained amount doesn't cover the deduction ("the rest of the balance
   is expired credit pending the expiry sweep").
5. **Positive adjustments carry Grant's $1M cap** — they are
   grant-shaped drainable inflows and previously bypassed the
   fat-finger cap entirely. Negative adjustments stay uncapped (bounded
   by the balance gate).

## Accepted residuals

- **Chronological display dip:** expiry entries stamp `created_at =
  expires_at` while backdated usage stamps the caller's `at`; a
  backdated drain of a *later* block can make the Credits tab's
  chronological running balance transiently negative on one row even
  though every commit-time balance was non-negative. Display artifact
  only — not a money bug; do not re-flag.
- **Void-reversal re-grants lose the original expiry** (named backlog
  item, out of P14's scope): `reverseForInvoice` re-grants voided-invoice
  credit with no `ExpiresAt`, permanently laundering expiring credit
  into never-expiring credit. Stripe reinstates into the original grant
  and expires immediately if past expiry. Divergence recorded here so it
  becomes a decision, not an accident.
- No structured `source_expired_grant_id` column: with the CAS as the
  exactly-once gate, a dedup index would be a redundant second guard
  (no-belt-and-suspenders). The description remains display copy only.

## Test locks (all real-Postgres, mutation-verified 2026-07-03)

`TestExpireGrantAtomic_RecomputesUnderLock` (stale snapshot → expiry is
-$40, not -$100), `TestExpireGrant_RetirementWinsOverBackdatedApply`,
`TestExpireCredits_ReplayAndConcurrentSweeps` (exactly one entry per
grant), `TestExpireGrant_ConcurrentBackdatedApply` (8-round collision:
SUM ≥ 0, conservation, full retirement),
`TestAdjustAtomic_ClawbackRequiresEligibleBlocks`,
`TestMigration0127_RetiresLegacyExpiredGrants` (executes the real
migration file). Mutations killed: no-op flip; pre-fix snapshot-append
shape; tx split (flip committed, entry crashed); disabled clawback
check.
