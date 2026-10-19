# ADR-127: A lost charge result is resolved by asking Stripe before anything irreversible

**Date:** 2026-10-19
**Status:** Accepted
**Supersedes:** [ADR-106](106-charge-intent-ledger.md) (the charge-intent
ledger, parked since 2026-07-31). PR #1291 drafted a second record-based
design under this number; it never reached `main` and was closed for this one.
**Amends:** [ADR-105](105-charge-idempotency-key-seed.md) (one more write moves
the counter), [ADR-107](107-unknown-is-terminal-until-a-human.md) (a parked
invoice's stale hold is checked too), [ADR-112](112-dunning-exhaustion-settles-two-questions.md)
(the final action takes the hold).

## Summary

Every charge claim already writes a five-minute hold on the invoice,
`auto_charge_claimed_until`, before it calls Stripe. A hold that has expired
with no saved result now means "a charge may have been sent and its result
was lost", and no longer "free". While any hold is set, nothing takes the
invoice, charges it, voids it or records an offline payment. Dunning's final
action does not run either. Write-off is the one action allowed on a stale
hold. One sweep in the payment reconciler asks Stripe what the lost charge
did. It records a payment it finds, adopts one still processing, and cancels
an off-session one stuck waiting on the customer. When Stripe has nothing, it
clears the hold. A lost result is resolved within about 70 minutes. No migration.

## Context

Issue #976. A charge's result can be lost. The process crashes after Stripe
took the money, or the caller's deadline ends during the call: dunning 15 s,
auto-charge 30 s, a Collect request whose browser went away. The invoice was
left `pending` or `failed`, exactly like one never charged. Its hold expired after
five minutes, and the next charge went out. The idempotency key only dedupes
the same initiator, card and amount within Stripe's 24 hours, so four routes
took a second payment:

- another charge kind (auto-charge, then a dunning retry or Collect: a
  different key by design);
- a changed card (the card is part of the key at Stripe);
- a key Stripe has pruned after 24 hours ("We generate a new request if a key
  is reused after the original is pruned");
- the hosted page, which has no key at all.

Only one trace survives an interrupted charge. The charge counter, the
attempt row and `payment_status` are written after the call, on the same
context, so a crash or a dead context leaves them unchanged. The hold is
written by the claim, in its own commit, before the call, and is cleared only
after a definite answer or on a path that never reached Stripe.

Stripe facts the design rests on (docs.stripe.com, fetched 2026-10-19):

- List is the read-after-write API; Search is not. "Don't use search for
  read-after-write flows… For read-after-write flows that require immediate
  data availability, use the various list APIs… These APIs aren't subject to
  the availability delays mentioned above."
- "Search isn't available to businesses in India." List has no such limit.
- List PaymentIntents takes `customer` and `created[gte]`, up to 100 a page.
- Idempotent results are saved "after endpoint execution begins… including 500
  errors".

## Decision

**The rule.** `auto_charge_claimed_until` is NULL (no charge outstanding),
in the future (its owner is working), or in the past (a charge may have been
sent and its result was not saved). Nothing takes the hold, charges, or does
anything irreversible while it is set, except its owner. Only the owner
clears its own hold, by comparing the value its claim wrote. Only the sweep
clears a stale one.

**Writers** (all lock the invoice row first):

- The three charge claims (`ClaimAutoCharge`, `ClaimChargeForDunningRetry`,
  `ClaimChargeForManualCollect`) admit only `auto_charge_claimed_until IS
  NULL` and return the value they wrote, the claim's token
  (`domain.ChargeHold`).
- `ClaimChargeHoldForFinalAction`: dunning's final action takes the hold too.
- `ReleaseChargeHold(…, token)` clears only the hold its owner wrote.
- `ClearStaleChargeHold(…, hold, seq, moveSeq)`, called only by the sweep: a
  compare-and-set on the hold and the charge counter it read.

**The charge's re-check.** Every caller passes its token to `ChargeInvoice`.
Right before sending, one SELECT confirms the hold is still the caller's with
at least two minutes left, the counter is unchanged, and the invoice is still
finalized and unpaid; otherwise nothing is sent. The Stripe call is bounded
by two minutes. So no request for an attempt can start after its hold ends
(`TestChargeHoldTiming`).

**Readers.** The hosted-page claim refuses any set hold. The dunning due-run
scans skip a run whose open invoice has a hold. Void and offline payment
refuse a stale hold as they refuse a live one; a cancel's or plan swap's
prebill void too (it rolls back and is retried). Write-off is allowed on a
stale hold. Collect answers 409 `payment_in_flight` with the same words.

**Settles without a charge release.** The zero-due settle and the refresh
failure in the auto-charge sweep, the credit-covered settle in the dunning
retry, and Collect's credit-covered settle all release their hold.

**The sweep** (`Reconciler.sweepStaleChargeHolds`, first in `Run`, on the
billing leader). It lists invoices of any status whose hold ended at least
10 minutes ago, rotating by `provider_synced_at`. For each, it lists the
Stripe customer's PaymentIntents created since an hour before the hold,
through List, never Search, and directly, never through the charge breaker.
It keeps the ones whose `velox_invoice_id` is this invoice, then:

| Found | Does |
|---|---|
| succeeded, not refunded in full | settles every one, oldest first (the first pays the invoice, each later one raises `payment.duplicate_charge`, money on a voided invoice raises `payment.received_on_voided_invoice`); clears |
| succeeded, refunded in full | counts it as not paid (ERROR log) |
| processing, invoice open | stamps it on the invoice (`processing`), so the webhook and the processing sweep take over; clears |
| Velox off-session `requires_*` not already the invoice's in-flight payment | cancels it at Stripe; keeps the hold for the next check |
| nothing else, invoice open | clears and moves the counter (a fresh key for the next charge) |
| nothing else, any other status | clears |
| no Stripe customer on file | as "nothing": no charge can have been sent |
| a read that may succeed later | keeps; stamps the row so it rotates |
| Stripe not connected, key rejected, customer not in the account | keeps on an open invoice (ERROR every check; write-off is the exit); clears on a closed one |

## Why this design

Ask the one party that knows, at the one place that can act on it. The hold
already had the lifecycle the problem needed: written before the call in its
own commit, cleared only on a definite answer or a path that never sent. So
the fix is a predicate change on an existing column plus one resolver, not a
new record.

"Stripe has nothing" is safe to act on, for five reasons:

- List is consistent;
- no request for the attempt can start after its hold ends;
- the sweep waits 10 more minutes;
- every Velox PaymentIntent carries `velox_invoice_id` and the invoice's
  Stripe customer;
- that customer id is set once (`customer.SetStripeCustomerID` refuses to
  overwrite a different one).

Matching by invoice id, not by attempt key, also finds a charge sent before
this deploy, and works for the hosted page's PaymentIntents.

## Alternatives considered

- **A. An attempt record (PR #1291).** Four new columns written before each
  call, a seven-exit clearing rule in the charge, a key-matching resolver and
  a park after 24 hours of failed reads. Same guarantee for charges and
  dunning, about three times the code and three migrations, and it cannot
  find a PaymentIntent created before its new metadata existed. Rejected:
  the hold already carries the fact the record would store.
- **B. Check Stripe inline in each caller** (the design's first version):
  Collect, the hosted page, void and dunning each run the check when their
  claim meets a stale hold. Resolves in about 15 minutes instead of 70, but
  puts Stripe calls in request paths, wires the invoice service to the
  payment check across domains, and gives every caller a "credits after the
  check" rule. Rejected for one resolver.
- **C. Search by `velox_invoice_id`** (ADR-108's API). Not offered in India,
  and eventually consistent, so an empty answer proves nothing. Rejected.
- **D. Keep "an expired hold is free".** That is #976.

## Consequences

### Positive

- None of the four routes takes a second payment after a crash, a timeout, a
  429 or a lost webhook. Dunning's final action never cancels a customer whose
  lost charge succeeded, and void and offline payment never act on one.
- A lost webhook self-heals within about an hour without a PaymentIntent id.
- No Stripe call is added to a normal charge, only one SELECT; one list call
  per stale hold.

### Risks / open items (residuals)

- **R1.** A definite decline whose save fails still releases the hold, so
  nobody collects. No money is taken; same as before. Tracked separately.
- **R2.** Credit notes, operator and automatic, are not held by a stale hold.
  A payment found later is flagged (`payment.duplicate_charge` or
  `payment.amount_mismatch`), not silent.
- **R3.** A cancel or plan-swap void of an unused prebill is refused for up to
  about 70 minutes after a lost charge (was 5 minutes); the caller retries.
- **R4.** A charger frozen after its re-check (a VM pause) can still send
  late; a later success is flagged as a duplicate.
- **R5.** During a mixed-version rolling deploy, old replicas treat an expired
  hold as free until the rollout ends.
- **R6.** The wait: up to about 70 minutes after a lost result (the hourly
  tick plus 10 minutes). Meanwhile the dashboard shows Pending or Failed and
  refusals say the charge is being confirmed with Stripe.
- **R7.** One list call per hold left by `processing`, unknown, the breaker or
  a 429. Old leftover holds drain through the rotating limit after deploy.
- **R8.** A lost success from before the deploy, already re-claimed under the
  old rules, leaves no trace. A hold still set at deploy is found.
- **R9.** An invoice whose Stripe customer was deleted, or whose tenant
  disconnected Stripe, can only be written off while its hold is set; an ERROR
  is logged every check until then.
- The sweep's scan has no index on `auto_charge_claimed_until` (no
  migration); it runs once per mode per tick. Add a partial index if it shows
  up in query timings.
- A failed check is retried after 30 minutes (`staleHoldRecheckAfter`), not
  the hour ADR-108's search waits, so a stamp taken during one hourly tick
  does not hide the row from the next.
- Not in this ADR: unclear answers with no PaymentIntent id still park when
  the caller's context is alive (follow-up B, ADR-107). A recorded off-session
  `requires_*` PaymentIntent still never ends (follow-up C).

## References

- [ADR-105](105-charge-idempotency-key-seed.md), [ADR-106](106-charge-intent-ledger.md),
  [ADR-107](107-unknown-is-terminal-until-a-human.md), [ADR-108](108-parked-invoices-search-and-adopt.md),
  [ADR-112](112-dunning-exhaustion-settles-two-questions.md); #975 (the hold
  refuses void, write-off and offline payment), #1264, #1284.
- Code: `internal/domain/charge_hold.go`, `internal/invoice/postgres.go`
  (claims, `ConfirmChargeHold`, `ListStaleChargeHolds`, `ClearStaleChargeHold`),
  `internal/payment/reconciler_stale_hold.go`, `internal/payment/stripe.go`
  (`chargeInvoice`).
- Tests: `internal/payment/stale_hold_integration_test.go` (real Postgres, a
  fault-injecting Stripe fake), `internal/dunning/stale_hold_integration_test.go`,
  `internal/payment/stripe_client_list_test.go`.
- Stripe: [Search, data freshness](https://docs.stripe.com/search#data-freshness),
  [List PaymentIntents](https://docs.stripe.com/api/payment_intents/list),
  [Idempotent requests](https://docs.stripe.com/api/idempotent_requests).
