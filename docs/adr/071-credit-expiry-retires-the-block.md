# ADR-071: Credit expiry retires the block — atomically, and retirement wins

**Date:** 2026-07-03
**Status:** Accepted (shipped in P14; designed by the P14 adversarial panel — 3 lenses, all SHIP-WITH-FIXES; consolidated protocol in the remediation plan §4.4; amended 2026-10-19, §2; amended 2026-10-22, §2: the boundary instant is live)

## Summary

When a credit grant expires, one transaction writes the expiry entry for the unused amount and marks the grant fully used (`consumed_cents = amount_cents`), so expired credit can never be spent again. The step, `ExpireGrantAtomic`, re-reads the unused amount under the same row lock that spending credit takes, so a concurrent spend cannot make it expire more than is left. Once expiry commits, a backdated apply draws nothing from that grant; there is no un-expire. So the sweep does not retire a grant while a period close or a period invoice that can still draw on it is owed (§2, amended 2026-10-19). A grant is live up to and including its `expires_at`, and the dashboard stores "valid through Nov 30" as the next local midnight (§2, amended 2026-10-22). A clawback (taking credit back) that unexpired credit cannot cover now fails instead of driving the ledger negative, and positive adjustments carry the same $1M cap as grants. Status: active.

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

   **Amended 2026-10-19: the sweep waits for the draws a grant is owed
   to.** Because retirement wins, retiring a grant that a period invoice
   still has to draw on charges the card for usage the grant should have
   paid. The real trigger is a close that fails on billing day and waits
   on the ADR-119 retry ladder (15m, 1h, 4h, up to about 3.7 days) while
   the grant passes `expires_at`. Lago reports the same failure shape
   (lago-api #4240: "On billing day there are sometimes invoices stuck in
   `generating` status."). A tax-deferred draft finalized hours later
   (#653) gives the same late collection. Velox follows the `expires_at`
   condition of Stripe's eligibility list: a grant applies to an invoice
   when "the invoice's `period_end` occurs before the credit grant's
   `expires_at` time, if set." The same list also requires that "the
   invoice's `period_end` is on or after the credit grant's
   `effective_at` time." Velox does not test that one:
   `drainPositiveBlocks` filters on `expires_at` only, so a period
   invoice collected late also draws a grant created after its period
   ended. This amendment does not change that.

   - **The hold.** Both candidate queries (`ListExpiredGrants`,
     `ListExpiredGrantsForClock`) skip a grant while
     `domain.CreditDrawOwedSQL` finds a draw still owed. (i) An `active`
     or `trialing` subscription of the customer is due at or before
     `expires_at` (`domain.SubscriptionDueInstantSQL`): its close draws on
     the grant. (ii) A `subscription_cycle` or `threshold` invoice of the
     customer, issued at or before `expires_at`, is a draft or is queued
     for collection (`auto_charge_pending`). "At or before" since the
     2026-10-22 amendment below.
   - **The draw.** A `subscription_cycle` or `threshold` invoice tests
     grant expiry at its `issued_at` (the period end or the fire instant)
     whenever it is collected: `ApplyToInvoiceAtomic` reads it on its
     locked invoice read. Only the expiry test moves. The usage entry,
     the invoice's `updated_at`, `paid_at` and the dunning start stay at
     the collection instant (#1281). `issued_at`, not
     `billing_period_end`: an in-advance cycle invoice stores the next
     period's end.
   - **A dunning retry draws the same way** (decided 2026-10-20). A retry
     of a period or threshold invoice judges a grant by that invoice's
     `issued_at`, the check its first collection made: a grant live at
     the period end and expired before the retry pays it. The retry runs
     `ApplyToInvoiceAtomic` like any collection, so no code differs; the
     usage entry is stamped at the retry. Stripe ties eligibility to the
     invoice's `period_end`, not to the payment attempt. Test:
     `TestCatchup_CreditExpiryRunsAfterDunningRetries_RealStore`.
   - **No gap in the hand-off, so discovery is enough.** The close moves
     the due instant and inserts the invoice (queued, or a draft) in one
     transaction (ADR-115). Finalize sets `auto_charge_pending` in the
     write that finalizes. Settle and void clear it in the write that
     ends collection. So at every committed instant (i) or (ii) holds
     until the invoice has drawn on the grant or is voided, and the
     sweep needs no lock and no re-check in `ExpireGrantAtomic`. A
     queued invoice holds a grant only until its next collection draws
     on it.
   - **It replaces the invoice-grace wait** (ADR-120 Decision 5, first
     version): retiring a grant only once `INVOICE_GRACE_PERIOD` had
     passed after `expires_at`. That covered a close that ran inside the
     grace, but not a close on the retry ladder or a late finalize.
   - **A draft that nothing finalizes holds grants until someone does.**
     While collection is paused (`keep_as_draft`, set by an operator or
     by dunning's `pause` final action, ADR-112), every close writes a
     draft. Resume and the tax retry leave it a draft. Arm (ii) then holds
     every grant of that customer that expires after the draft's
     `issued_at`, until the draft is finalized or voided: no time limit.
     Until then the ledger balance, `credit.balance_low` and
     `credit.balance_depleted` count the expired credit; the spendable
     balance does not. A draft finalized later draws on it, by the
     `expires_at` condition. Stripe's balance summary has the same split:
     the available balance "is equal to the ledger balance less any
     expired credits or unrecorded transactions." Holding only
     tax-pending drafts would bound it, but would open a gap. `RetryTax`
     commits the computed tax, then finalizes in a second write. A sweep
     between the two, or a crash there, would retire a grant the
     finalize still needs.
   - **Not covered: a scheduled cancel's final invoice.** The `cancel_at`
     arm of the due instant comes with the reused fragment. It holds a
     grant until that invoice is built, at least the grace after
     `cancel_at` (ADR-120). But the invoice (`subscription_cancel`) is
     issued when it is built and tests expiry when it is collected, not
     at `cancel_at`. So a grant that was live at `cancel_at` and expires
     before the build is not drawn, as before this amendment. Covering it
     means testing that invoice's draw at its `billing_period_end` (the
     `canceled_at`) and adding it to arm (ii). That is a separate
     decision.
   - **Not covered: the wall-clock sweep during dunning.** An invoice in
     dunning is finalized with `auto_charge_pending` cleared, so the hold
     does not cover it, and the hourly sweep retires a grant at its
     `expires_at` even when a later retry of a period invoice issued
     before then would draw it. A test-clock advance runs expiry after
     the retries (ADR-029, amended 2026-10-20), so there the retry draws
     it. Holding grants for invoices in dunning is a separate decision.
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

### Amendment 2026-10-22: a credit is live at its own expires_at

**One rule: a credit block is live at instant t when t <= `expires_at`.**
Every predicate uses it: the drain (`drainPositiveBlocks`, `expires_at >=
t`), the commit banner (`CommitGrantExposure`, the same), the spendable
balance (`GetSpendableBalance` counts a block as expired when `expires_at <
t`), both arms of the hold (`<=`), and the two sweeps, which already
retired only `expires_at < now`. Before, the drain, the banner and the
spendable balance treated `t == expires_at` as expired while the sweeps
treated it as live, so at that one instant neither took the grant.

**Why.** The dashboard's expiry picker sent the end of the picked day,
23:59:59.999 in the tenant zone. A period ends, and its invoice is issued,
at the next local midnight. So a grant "through Nov 30" was 1 ms short of
the invoice issued Dec 1 00:00 and paid nothing: the card was charged in
full. The dashboard now sends the local midnight after the picked day
(`startOfNextDayInTZ`). With that send, `expires_at` equals the issue
instant, so the boundary has to count as live.

**What the dashboard shows.** A stored `expires_at` that is a midnight in
the tenant zone reads as the day before it: Dec 1 00:00 reads "Valid
through Nov 30". Any other instant reads "Expires" with its date and time
(`creditExpiryLabel`), because the credit is not valid through the end of
that day. Two examples are an API grant that stops at 17:00 and a
dashboard grant saved at 23:59:59.999 before this change, which misses the
invoice issued at the next midnight.

**Why the hold arms move too.** With `expires_at` equal to the due
instant, a strict arm (i) is false. The hourly sweep (`expires_at < now`)
could then retire the grant between midnight and the close, or while the
close waits on the retry ladder, and the close would charge the card in
full. Arm (ii) moves for the same case when the close has run and its
invoice is a draft or queued.

**Stripe says "before".** Its condition is "the invoice's `period_end`
occurs before the credit grant's `expires_at` time", so it refuses a grant
that expires exactly at the period end. Velox includes the boundary: all
of the period's usage happened before its end.

**No backfill.** Grants saved by the dashboard before this change keep
23:59:59.999 and the 1 ms gap. No partner was live (2026-10-20), and
finding those rows would rest on a fingerprint (`.999` in some zone), not
on a fact. The CHANGELOG tells operators to grant them again if needed.
The API passes any instant through unchanged.

**In advance.** An in-advance cycle invoice issued Dec 1 also carries
December's base fee, and a draw takes the whole invoice. So a grant
"through Nov 30" can now pay December's base fee too. Before this change
no dashboard grant could pay the next period's in-advance base fee; now
every one that is live at the close can. An API grant that expires after
the close always could. Stripe's condition above works the same way: it
tests the invoice's `period_end` against `expires_at`.

A mid-period `cancel_at` invoice still tests expiry when it is collected
(the residual named in §2).

**Residual: a midnight that DST skips.** Where a zone skips midnight, the
dashboard (`fromZonedTime`, date-fns-tz 3.2.0) and Go's `time.Date` do not
always resolve it to the same instant:

| Zone and date | Go `time.Date` | `fromZonedTime` |
|---|---|---|
| Africa/Cairo, 2026-04-24 00:00 | 22:00Z (01:00 local) | 21:00Z (23:00 the day before) |
| Asia/Beirut, 2026-03-29 00:00 | 22:00Z (01:00 local) | 21:00Z (23:00 the day before) |
| America/Santiago, 2026-09-06 00:00 | 03:00Z (23:00 the day before) | 03:00Z (23:00 the day before) |
| America/Havana, 2026-03-08 00:00 | 04:00Z (23:00 the day before) | 04:00Z (23:00 the day before) |

In Cairo and Beirut the dashboard's instant is an hour before Go's period
end. A grant valid through the day before the change then pays nothing for
a period that ends at that midnight. That happens once a year, and only for
an anniversary period that ends on that day, since a calendar period ends
on the 1st. The dashboard reads such a grant as "Expires" with its time,
not "Valid through". Copying Go's resolution zone by zone in the client is
not worth it.

Tests (real Postgres): `TestApplyToInvoice_PeriodInvoiceIssuedAtTheExpiryInstantDrawsTheGrant`,
`TestExpireCredits_HoldsAGrantExpiringAtTheDueInstant`,
`TestExpireCredits_HoldsAGrantExpiringAtAPeriodInvoiceIssue`,
`TestLivenessAtTheExpiryInstant_DrainSpendableExposureAgree` (credit);
`TestCycleClose_GrantValidThroughTheLastDay_PaysThePeriodInvoice` and the
at-the-period-end case of
`TestFailedClose_ThenSuccess_DrawsTheGrantLiveAtThePeriodEnd` (billing).
Each failed on the commit that added them alone (#1332). Putting back
any one of the five old predicates fails at least one of them; that was
run, not only read.

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

The 2026-10-19 amendment (§2): `TestExpireCredits_HoldsAGrantWhileAFailedCloseIsOwed`,
`TestExpireCredits_HoldsAGrantWhileAPeriodInvoiceIsUncollected`,
`TestExpireCreditsForClock_HoldsAGrantAFailedCloseNeeds`,
`TestExpireCredits_NoGrace_HoldIsTheOwedClose` (credit), and
`TestFailedClose_ThenSuccess_DrawsTheGrantLiveAtThePeriodEnd`,
`TestSweepCollect_CycleInvoiceDrawsCreditLiveAtIssuedAt`,
`TestTaxDeferredClose_RetryTaxLater_DrawsTheGrant`,
`TestExpiryVsCloseCollect_NoGapInTheHandoff` (billing). Each failed
before the fix, on the commit that added them alone (#1305, CI run
39298980811). Each test's comment names the part it pins: a clause of
the hold, the clock query's hold, the `status = 'draft'` or
`auto_charge_pending` arm, or the draw at `issued_at`. Those mutations
were checked by reading the tests, not run.
