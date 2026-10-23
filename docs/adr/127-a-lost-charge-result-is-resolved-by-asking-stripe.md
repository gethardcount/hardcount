# ADR-127: A lost charge result is resolved by asking Stripe before anything irreversible

**Date:** 2026-10-19
**Status:** Accepted (amended 2026-10-20; amended 2026-10-23: a written-off park keeps its hold when Stripe refuses)
**Supersedes:** [ADR-106](106-charge-intent-ledger.md) (the charge-intent
ledger, parked since 2026-07-31), and since the 2026-10-20 amendment
[ADR-108](108-parked-invoices-search-and-adopt.md) (the Stripe Search sweep). PR #1291 drafted a second record-based
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
clears the hold. A lost result is checked on the first billing tick at least
10 minutes after its hold ends: with the hourly tick, usually within about an
hour (R6). No migration.

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

**Paths that send no charge release.** The zero-due settle and the refresh
failure in the auto-charge sweep, the credit-covered settle in the dunning
retry, and Collect's credit-covered settle all release their hold. So does a
charge that sent nothing to Stripe: its re-check refused, the breaker was
open (it refuses before any request), or the credentials could not be read.
The breaker is shared by every tenant; kept, one 30-second open window held
every invoice charged in it until the sweep reached it. A 429 keeps the hold:
stripe-go resends after a network error, and a 429 to the resend can hide a
first request Stripe is still running. A release runs to completion even
after its caller's context ends (a closed tab, a shutdown).

**The sweep** (`Reconciler.sweepStaleChargeHolds`, first in `Run`, on the
billing leader). It lists invoices of any status whose hold ended at least
10 minutes ago, open invoices (finalized, pending or failed) first, then
rotating by `provider_synced_at`. It drains: page after page, each hold
checked once, until none is left. It stops early when its 5-minute budget
for the mode is spent. It also stops when three checks in a row got no
answer from Stripe (a network error or the time limit); a 5xx or a 429 is an
answer and comes back at once. Either way the rest wait for the next tick. Each check's
Stripe calls are cut at 45 seconds (stripe-go alone allows about 90 s a page
while Stripe is unreachable). For each hold, it lists the Stripe customer's PaymentIntents
created since an hour before the hold, through List, never Search, and
directly, never through the charge breaker. It keeps the ones whose
`velox_invoice_id` is this invoice, then:

| Found | Does |
|---|---|
| succeeded, not refunded in full | settles every one, oldest first (the first pays the invoice, each later one raises `payment.duplicate_charge`, money on a voided invoice raises `payment.received_on_voided_invoice`); clears, unless a processing one the invoice does not follow is left (next rows) |
| succeeded, refunded in full | counts it as not paid (ERROR log) |
| processing, invoice open | stamps the newest on the invoice (`processing`), so the webhook and the processing sweep take over; clears |
| processing, and the invoice is already processing or `unknown` with that same PaymentIntent | clears: the processing or unknown sweep follows it |
| processing, any other (the invoice written off, voided or paid meanwhile; a second one) | keeps the hold, stamped, until it ends: a later check settles it or flags it. Cleared, nothing would record it if its webhook were lost |
| Velox off-session `requires_*` not already the invoice's in-flight payment | cancels it at Stripe; keeps the hold for the next check |
| nothing else, invoice open | clears and moves the counter (a fresh key for the next charge) |
| nothing else, any other status | clears |
| no Stripe customer on file | as "nothing": no charge can have been sent |
| a read that may succeed later | keeps; stamps the row so it rotates (not an `unknown` invoice that names its PaymentIntent: see below) |
| Stripe not connected, key rejected (401) or without permission (403), customer not in the account | keeps on an open invoice (ERROR every check; write-off is the exit) and on a written-off park (2026-10-23); clears on any other closed one |

A parked invoice (an unclear answer that named no PaymentIntent) is checked
here too, with one difference for "nothing": see the 2026-10-20 amendment.

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
- **R3.** A cancel or plan-swap void of an unused prebill is refused while
  that prebill holds a lost charge's hold (R6; was 5 minutes). The cancel or
  swap rolls back, and the retry depends on who asked:
  - an operator's cancel or swap answers 409 and is retried by hand;
  - the scheduled-cancel scan retries on its next tick;
  - dunning's final cancel is re-attempted 24 hours later (its failed-action
    retry), not after the hour.

  Dunning's own invoice is never the one refused. The final action does not
  run while that invoice's hold is set, and it runs the cancel as that hold's
  owner.
- **R4.** A charger frozen after its re-check (a VM pause) can still send
  late; a later success is flagged as a duplicate.
- **R5.** During a mixed-version rolling deploy, old replicas treat an expired
  hold as free until the rollout ends.
- **R6.** The wait. A hold is checked on the first billing tick that starts
  at least 10 minutes after it ends. With the hourly tick that is within about
  75 minutes of the claim. Add the run time of a tick already under way when
  the hold ends, since the sweep runs at the start of a tick. It takes longer
  only when Stripe cannot be read, or when more stale holds are queued than
  one run's 5-minute budget covers (open invoices go first). Meanwhile the dashboard shows
  Pending or Failed, and refusals say the charge is being confirmed with
  Stripe, "within about an hour".
- **R7.** One list call per hold left by `processing`, unknown or a 429. Old
  leftover holds (every expired hold, in any status, at deploy) drain through
  the budgeted runs after deploy, open invoices first.
- **R8.** A lost success from before the deploy, already re-claimed under the
  old rules, leaves no trace. A hold still set at deploy is found.
- **R9.** An invoice whose Stripe customer was deleted, or whose tenant
  disconnected Stripe, can only be written off while its hold is set; an ERROR
  is logged every check until then.
- The sweep's scan has no index on `auto_charge_claimed_until` (no
  migration); it runs once per page of 50 per mode per tick. Add a partial
  index if it shows up in query timings.
- A kept hold is stamped (`provider_synced_at`) and checked again after 30
  minutes (`staleHoldRecheckAfter`), not the hour ADR-108's search waits, so a
  stamp taken during one hourly tick does not hide the row from the next. The
  stamp means "Velox last asked Stripe", whether or not Stripe answered. An
  `unknown` invoice that names its PaymentIntent is never stamped by this
  sweep: the column is the unknown sweep's rotation key, and that sweep stamps
  it. A parked invoice is stamped here since the 2026-10-20 amendment (it
  was not while the Search sweep used the column as its cool-off).
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
  `internal/billing/dunning_cancel_voids_prebill_integration_test.go`,
  `internal/invoice/stale_hold_store_integration_test.go`,
  `internal/payment/stripe_client_list_test.go`,
  `internal/payment/charge_nothing_sent_test.go`.
- Stripe: [Search, data freshness](https://docs.stripe.com/search#data-freshness),
  [List PaymentIntents](https://docs.stripe.com/api/payment_intents/list),
  [Idempotent requests](https://docs.stripe.com/api/idempotent_requests).

## Amendment 2026-10-20 — parked invoices are finished by this sweep; Stripe Search is gone (REC-5)

Decided by Sagar on 2026-10-19: move parked invoices onto List ("do the list
api changes everywhere then if this is the robust choice"), wait by cause
(below), and the banner copy.

**Why.** Two facts, both from Stripe or the code:

- Stripe's Search page: "Don't use search for read-after-write flows… the
  data won't be immediately available to search." And: "Propagation of new
  or updated data could be delayed during an outage." An outage is what
  parks an invoice, so the ADR-108 sweep searched at the worst time, and an
  empty answer proved nothing.
- A park whose request never reached Stripe had no exit except a write-off
  and a new invoice made by hand.

A parked invoice already keeps its claim's hold (unclear outcomes never
release it), so this sweep already listed it. It now finishes it.

**What the sweep does with a parked invoice:**

| Found for this invoice | Open park (`finalized`) | Written-off park (`uncollectible`) |
|---|---|---|
| succeeded, not refunded in full | settles, oldest first; clears (as before) | same (`uncollectible` → paid) |
| processing | puts the newest on the invoice (`processing`, its id, the counter moves); clears. The webhook and the processing sweep follow it | keeps the hold until it ends (as before) |
| Velox off-session `requires_*` | cancels it at Stripe; keeps the hold (as before) | same |
| nothing, or only failed, canceled or refunded in full, less than 24 hours after the hold | keeps the hold, stamps the row (`waiting`) | same |
| the same, 24 hours or more after the hold | `UnparkAfterNoPayment` (`released`) | same; nothing charges a written-off invoice (ADR-113), so this only ends the park |
| no Stripe customer on file | as "nothing" | same |
| a read refused for good | keeps; ERROR every check; write-off is the exit (R9) | keeps; ERROR every check (changed 2026-10-23, below) |

`UnparkAfterNoPayment` is one compare-and-set on the hold, the counter and
the parked shape. It sets `payment_status` back to `failed` when the parked
attempt's row says a dunning retry made it. The run then retries at once: the
parked attempt was never counted. Otherwise (auto-charge, Collect, or no
attempt row) it sets `pending`. It clears `last_payment_error` (Stripe's 500
message, which the failure banner would print as the bank's words) and the
hold, and moves the counter, so the next charge sends a new idempotency key.
In the same transaction the parked attempt's row becomes `failed`, with the
reason "Stripe showed no payment for this attempt 24 hours later." The
Activity card prints the reason as the problem, so the saved answer is not
kept. For a timed-out call it is Go's error; for a 500 or a 409 it is
Stripe's words about the request. Collect on a `failed` invoice comes back
`pending`; the status shown differs, who may charge does not.

**Why 24 hours for "nothing".** Every park answer is one where Stripe got the
request or may have: a 500, a 409, a network error, a timed-out call. Stripe's
[advanced error handling](https://docs.stripe.com/error-low-level) page,
fetched 2026-10-19:

- "Treat the outcome of a request that returns `500` as indeterminate."
- "During incident recovery, Stripe attempts to reconcile failed operations
  and send webhook events for any objects created during reconciliation."
- "Limit retries of an unresolved operation to the first 24 hours after the
  original request, using the same key and parameters. If the outcome is
  still unknown after that window, don't retry with either the old key or a
  new one. Retrieve the relevant object, inspect the request in the Dashboard
  and use webhook events to reconcile the outcome. If you can't establish
  what happened, contact Stripe support before repeating the operation."

Velox retrieves: it lists the customer's payments every tick. When
the list still shows none for the invoice 24 hours after the hold, Velox
charges again under a new key, without contacting Stripe support. That last
step is a deliberate departure from Stripe's text, decided by Sagar; R10 is
its price. A payment the list shows is acted on at the first check. The gate
is `domain.ParkedNothingWait`, read on the database clock the hold was
written with (`ListStaleChargeHolds`). A test-clock park waits 24 real hours.

**By cause.** The callers' deadlines are dunning 15 s, auto-charge 30 s, and
**Collect payment** 30 s (the `/v1` routes' request timeout). One that ends
the Stripe call is a timed-out call: Stripe was slow, which is what an
incident looks like. `chargeInvoice` saves every unclear answer, its attempt
row and its park, on a context detached from the caller, unless the caller
was cancelled. So a timed-out call gets the 24 hours. So does a 500 that
arrives just before the deadline, when the deadline passes while it is
saved. A cancellation (a shutdown, a lost lease, a closed **Collect
payment** tab) or a crash is Velox interrupting itself, so nothing is saved.
Its stale hold is checked about 75 minutes after the claim, as before.

**Why the next charge does not reuse the parked attempt's key.** Stripe
removes keys "after they're at least 24 hours old. We generate a new request
if a key is reused after the original is pruned", so the protection ends when
the wait does. Before that, "Retries with the same key return the saved
response, including `500` errors", so the invoice would park again. And a
changed card, amount or charge kind is a 409.

**The stamp.** `MarkStaleHoldChecked` stamps a parked invoice, so a park
rotates instead of heading the list every tick.

**Deleted.** The ADR-108 sweep and everything only it used: `sweepParked`,
`SearchPaymentIntentsByInvoiceID` (the one call to Stripe Search),
`ListParkedSearchable`, `AdoptPaymentIntentIfParked`, `CountParkedInvoices`,
the latch for accounts without Search, and the metric
`velox_parked_search_errors_total`.

**Upgrade.** Parks made by v0.8.x keep their claim's hold, so the sweep
finishes them at the first tick after the upgrade. A payment found is
recorded. An open park older than 24 hours with none is charged again.
Before upgrading, write off any parked invoice the customer paid another
way. Migration `00186_hold_parked_invoices` is for one install only: one
that ran `main` between this ADR (#1299, 2026-10-19) and this amendment. No
release has #1299 without it. That code cleared a park's hold at its first
check when Stripe showed nothing, and left the park to the Search sweep, so
with the Search sweep gone nothing would look at it again. The migration
gives each such park a hold at its newest parked attempt's `created_at`
(written by the database after Stripe answered), or 2026-01-01 when that row
was lost. Where no park lacks a hold it changes nothing.

**Product copy.** The `payment.unidentifiable` notice is a Warning: "Stripe
didn't confirm this charge and may still complete it. Velox checks Stripe
every hour and records the payment if it appears. If none appears within 24
hours, the invoice is charged again as usual." Write-off stays as a secondary
action, for a customer who paid another way.

**Monitoring.** `velox_parked_invoices` counts only the parks the sweep has
not ended on schedule. Those are parks still parked 26 hours after the hold,
or with no hold. The 26 hours are the 24, the hourly tick that ends the
park, and an hour for a late tick. A count of every park would stay above 0 for as long as parks kept
arriving. So during a Stripe incident an alert on it, with any `for`, would
page on parks that were on schedule. Alert on `disposition="open"` above 0.
Such a park is one Stripe cannot be asked about (R9, R17), one Stripe could
not be read about for a day, or one with no hold (R13).

**Residuals.**

- **R10.** Stripe creates the original payment more than 24 hours after the
  attempt. Only its webhook records it: a later hold lists from its own hour.
  If the invoice was charged again by then, the second payment raises
  `payment.duplicate_charge` (ADR-068). Trigger to revisit: the first
  duplicate traced to a Stripe incident.
- **R11.** A park made before #1299 whose customer's Stripe id changed (it was
  overwritten freely then): List on the current customer misses a payment on
  the old one. It needs that payment to have succeeded and its webhook to be
  lost too. Parks made after #1299 are not exposed.
- **R12.** A lost decline webhook on a parked attempt: a decline counts as no
  payment, so the charge 24 hours later declines again and records it. For a
  hard decline that is one more attempt on a refused card (#966).
- **R13.** A mixed-version rolling deploy: an old replica leading the tick
  after the migration can still clear a new park's hold. That park stays
  parked until a webhook or a write-off; the gauge counts it at once, since
  it has no hold.
- **R14.** A network error before the request left: nothing reached Stripe,
  but Velox cannot tell that from a 500, so it waits 24 hours. Follow-up B
  below shortens it. Trigger: the first partner invoice that waits a day on a
  park from a network error.
- **R15.** A cancellation while Stripe is slow (a shutdown, a closed tab
  during an incident) is a lost result, checked about 75 minutes after the
  claim, though Stripe may still create the payment. After a crash the cause
  cannot be known.
- **R16.** Two parks on one invoice, when Stripe answers 500 to creates for
  more than a day. A late decline of the first park's payment can record the
  second park as failed: Velox cannot tell two unnamed attempts apart. The
  sweep then clears that hold at about 75 minutes, without the 24 hours.
- **R17.** A customer deleted in Stripe: the list is refused, so the park
  stays, with an ERROR every check, until someone writes it off. Not checked
  against Stripe yet: what List returns for a deleted customer's id. If it
  returns an empty list, the park ends as "nothing" after 24 hours, and the
  charge after it fails on the deleted customer.
- **R18.** A 429 on stripe-go's resend. stripe-go resends a request after a
  network error, and a 429 to the resend can hide a first request Stripe is
  still running. The charge treats a 429 as transient: it saves nothing and
  keeps the hold. So the stale-hold sweep decides about 75 minutes after the
  claim, without the 24 hours, though Stripe may still complete that first
  request. Trigger to revisit: a duplicate payment traced to a 429 on a
  charge.

**Not in this amendment:** follow-up B (an unclear answer with no PaymentIntent
id saves nothing and becomes a lost result) and follow-up C, as above.

## Amendment 2026-10-23: a written-off park keeps its hold when Stripe refuses

When Stripe refuses the check for good (not connected, key rejected,
customer missing), the sweep keeps a written-off park's hold, stamped, with
an ERROR every check, as it does an open park's. It used to clear it: the
invoice stayed parked with no hold, and nothing looked at it again, even
once Stripe could be asked. Since the
[ADR-050 amendment](050-unpaid-source-proration-policy.md#amendment-2026-10-23-a-written-off-invoice-is-unpaid)
a relief credit note on a parked written-off invoice waits for the park to
end, so it would have waited for good. The kept hold refuses nothing new: a
parked invoice already refuses void and offline payment, and nothing
charges a written-off one (ADR-113). Once Stripe answers, the table above
applies. Test: `TestParked_WrittenOff_StripeRefusedForGood_KeptUntilStripeAnswers`.
