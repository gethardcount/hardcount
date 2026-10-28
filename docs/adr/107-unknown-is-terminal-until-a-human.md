# ADR-107: An unnameable charge attempt parks the invoice; it is never settled failed

**Status:** Accepted (amended 2026-08-01, 2026-08-02, 2026-10-18, 2026-10-19, 2026-10-20)
**Date:** 2026-07-31
**Supersedes:** [ADR-106](106-charge-intent-ledger.md) as the answer to this
problem. ADR-106's charge-intent ledger is **parked, not deleted** — see
"Why not the ledger" and its trigger.
**Builds on:** ADR-105 (`charge_attempt_seq` as the idempotency-key seed, shipped
in #678 and unaffected by this decision), ADR-049 (payment reconciler).

## Summary

When a Stripe charge ends with an unclear result and no PaymentIntent id, Hardcount no longer marks the invoice `failed`. That used to make the invoice claimable again under a new idempotency key (`charge_attempt_seq`), so a retry could charge the customer twice. Now the invoice is parked at `payment_status='unknown'`, which no charge path accepts, and `TestUnknownPaymentIsUnchargeableByEveryClaimPath` fails the build if one ever does. That exclusion is the whole safeguard: the write that records `unknown` also bumps `charge_attempt_seq`, so the original attempt cannot be replayed (amendment of 2026-08-01). The park is logged once, and the unknown sweep skips parked rows. Since 2026-10-20 the park ends without a person. The stale-hold sweep lists the customer's payments in Stripe and records or follows a payment that appears. With none 24 hours after the attempt, it puts the invoice back to `pending` or `failed` to be charged again ([ADR-127](127-a-lost-charge-result-is-resolved-by-asking-stripe.md), last amendment). `MarkUncollectible` stays allowed, for a customer who paid another way; it leaves `unknown` in place, so a charge that later succeeds still marks the invoice paid. Credit-note refunds follow the same rule since 2026-10-18: an unclear refund result is `unknown`, and **Retry refund** resends the same request under the same key (see the last amendment).

## Context

A Stripe charge can create a PaymentIntent that Hardcount never learns the name of:
the process dies between the call and the outcome write (a **timeout** does this
with no crash at all), or the call returns an ambiguous error carrying no
PaymentIntent id.

`payment.reconcileOne`'s empty-PI branch then settled the invoice **failed**,
under the comment *"a safe retry generates a new PI"*. That comment described
the bug. Settling failed does two things at once:

- it bumps `charge_attempt_seq`, which **rotates the Stripe idempotency key**, so
  the next attempt can no longer dedup against the first;
- it moves `payment_status` out of `unknown` into a state every charge-claim
  predicate admits.

So the next retry opens a **second PaymentIntent beside one that may be live and
charging**. The customer is charged twice.

## Decision

**Delete the give-up write.** When an ambiguous outcome leaves no PaymentIntent
id, `reconcileOne` logs CRITICAL and does nothing else. The invoice stays at
`payment_status='unknown'` until a webhook names the PaymentIntent (the ordinary
case) or a human resolves it.

That closes the hole **by construction**, because `unknown` is a state no charge
path admits — verified against the code, not assumed:

| Path | Predicate |
|---|---|
| `ClaimAutoCharge` — the background sweep | `payment_status = 'pending'` |
| `ClaimChargeForManualCollect` — operator "Collect payment" | `payment_status IN ('pending','failed')` |
| `ClaimChargeForDunningRetry` — dunning | `payment_status IN ('pending','failed')` |

An independent audit of every writer confirmed the closure is total: there are
exactly three production writers of `payment_status`, `unknown → pending` is
unreachable in Go (nothing passes `domain.PaymentPending` to `UpdatePayment`),
no `retry-payment` / `charge-now` / `reset` route exists, the customer portal has
no pay route, and `Void`, `MarkUncollectible` and `RecordOfflinePayment` all
refuse an in-flight invoice via `IsInFlight()`.

Because that closure IS the guarantee — three separate SQL predicates that a
fourth charge path or one careless widening could break — it is mechanised:
`TestUnknownPaymentIsUnchargeableByEveryClaimPath` claims each path against a
claimable invoice (the control, so the negative means something) and then against
an `unknown` one. Admitting `unknown` anywhere fails the build, naming the path.

### What ships with it

- **The hosted page stops lying.** `payEnabled` had no `payment_status` term, so
  a live "Pay" button rendered on an `unknown` invoice and answered 409 when the
  customer pressed it. Now gated on `IsInFlight()`.
- **The checkout claim** refuses `unknown`, not merely `processing` —
  `IsInFlight()` has always meant both.
- **Two unguarded writers deleted.** `Service.RecordPayment` and
  `RecordPaymentFailure` wrote `payment_status` with no state guard and had zero
  production callers. Under this ADR they were the single easiest way to reopen
  the hole by accident: convenient names, obvious signatures, no guard.
- **Migration 0009's rollback carries a warning.** `0009_payment_unknown.down.sql`
  rewrites every `unknown` row across all tenants in one statement, and is
  reachable from `hardcount migrate rollback`. It predates this invariant and is now
  load-bearing for it.

## Consequences

**A stuck invoice needs a human — and the human needs a lever.** The first
version of this ADR said "a human must act" without checking that they could.
They could not: `Void`, `MarkUncollectible` and `RecordOfflinePayment` all refuse
an in-flight payment, and `unknown` is in-flight, so the complete set of
state-changing operator actions on a parked invoice was EMPTY. An invoice that
can reach no terminal state violates the every-invoice-terminates rule, and the
CRITICAL log was instructing operators to "settle or void it by hand" — both
refused. Corrected the same day (the parked-invoice honesty sweep):

- **`MarkUncollectible` is now allowed** on a parked invoice. It is the one safe
  exit: it moves no money, and if the charge did succeed the provider webhook
  still marks the invoice paid through the ordinary recovery path. `Void` stays
  refused (a voided-then-succeeded invoice is a contradiction) and
  `RecordOfflinePayment` stays refused (it would label a card charge as
  out-of-band). The invoice page already carried the button; only the service
  refused it.
- **The banner tells the truth.** It rendered at Info severity saying "Hardcount
  re-checks automatically and resolves" — of an invoice that will never resolve,
  on day 1 and day 400. A parked invoice now raises Critical, states that it
  will not resolve on its own, explains that no further charge is attempted
  deliberately, and names the write-off.
- **No collection promise goes out.** `resend-setup-link` was allowed on a
  parked invoice and emails the customer "add a payment method and we'll collect
  it automatically" — a promise the engine will never keep. Now refused.
- **The Collect refusal distinguishes the two cases.** "Retry after it resolves"
  is right when a PaymentIntent id exists and wrong when one does not.

**And a liveness sink this ADR introduced.** `ListUnknownPayments` is
oldest-first with a batch limit, and a parked row's `updated_at` is frozen
because nothing ever writes it again — so parked rows permanently headed the
queue, and once a batch-size accumulated the sweep would never reach a NEW
ambiguous charge that DOES carry an id and that the provider would resolve in one
call. The safety fix had quietly converted into a liveness sink for exactly the
invoices the reconciler exists to save. Parked rows are now excluded from that
sweep at the SQL level — they are not reconcilable by definition — and the parked
state is announced ONCE, where it is created, rather than as an identical
CRITICAL every tick forever.

**Every oldest-first queue over these rows is a starvation sink.** The
`ListUnknownPayments` case above was found first and treated as one bug. It was
an instance of a shape: a parked row's ordering timestamp is frozen (nothing
writes it again), so in any `ORDER BY … ASC LIMIT n` scan parked rows migrate to
the head and stay there, and at `n` of them the queue stops serving anyone else.
Two more were found by looking for the shape rather than the bug (2026-07-31):

- **Dunning retries.** `ClaimChargeForDunningRetry` requires `payment_status IN
  ('pending','failed')`, so it declines, the adapter returns `ErrTransientSkip`,
  and the transient handler deliberately does *not* reschedule — right for a
  Stripe blip, endless for an invoice that can never be charged. The run was
  re-selected every tick forever, each tick writing an attempt and rewinding it,
  while its frozen `next_action_at` held the head of a `LIMIT 20` queue.
  In-flight sources are now excluded from `ListDueRuns` and its sim-time twin.

  **This shipped keyed on the wrong question first.** The exclusion originally
  tested the PARKED shape — `unknown` with no PaymentIntent id — and carried a
  comment asserting that a `processing` charge "does resolve on its own, so
  skipping it is correct". Both halves were wrong, and the second was wrong in a
  way that mattered: `reconcileOne` settles only TERMINAL provider outcomes and
  skips `processing` / `requires_action` / `requires_confirmation` /
  `requires_capture` on every sweep. An `unknown` invoice **with** an id whose PI
  sits at `requires_action` therefore never resolved either, and spun exactly
  like a parked one. (The first version of this paragraph blamed "off-session
  SCA nobody completes" — wrong, and worth recording because the phrase was
  inherited from a code comment and repeated without checking. Off-session SCA
  is a DECLINE: Stripe's `authentication_required` is a decline code, so it
  settles the invoice FAILED with dunning started. A live `requires_action` PI
  comes from the hosted checkout.) "Do we hold an id"
  answered the wrong question. The right one is "can the claim succeed", and the
  claim's own predicate answers it for every in-flight shape at once — so the
  exclusion is now `domain.IsInFlight` expressed in SQL, matching the clawback
  scan's predicate rather than special-casing beside it. Nothing is lost by the
  widening: the moment a payment leaves flight the row is selectable again.
- **Clawback drafts.** Covered under ADR-059's amendment: the draft's
  eligibility tested payment state alone, so a parked source deferred it
  permanently — no issue, no void, no log, and a gauge counting it forever.

- **A fourth instance, NOW FIXED AT THE SOURCE (migration 0167).**
  `ListUnknownPayments` was `ORDER BY updated_at ASC LIMIT 50`, and a PI sitting
  at `requires_action` is a never-moving row it still admitted: `reconcileOne`
  skipped those with a bare `return false, nil`, writing nothing, so
  `updated_at` stayed frozen and the row headed the queue exactly as parked rows
  once did.

  This one was first registered as accepted-with-a-trigger, on the reasoning
  that fixing it needed an ordering column for a hazard requiring ~50
  concurrently-stuck invoices. That reasoning missed the real defect. The
  reconciler was FETCHING the provider's status every sweep and throwing it
  away — so the starvation, the banner's false promise of automatic resolution,
  and the absence of any terminal state were three symptoms of one omission, not
  three independent problems to be triaged separately. Recording the observation
  fixes all three, and the ordering column falls out of it for free.

  The sweep now orders by least-recently-observed, so a row it cannot resolve
  rotates to the back instead of jamming the head. Ordering, not exclusion, on
  purpose: these rows must keep being polled, because the sweep is what notices
  when the customer finally authenticates.

**A terminal invoice closes the payment question; it does not answer it.** Both
fixes are scoped to invoices that are still OPEN, and that scoping is the load-
bearing part. A write-off deliberately leaves `payment_status='unknown'` — we
never learned whether the card was charged, and recording `failed` on an
operator's click would assert the provider declined, which we do not know. So
the release has to key on invoice STATUS, and the excluded rows have to become
visible again once the invoice is terminal: dunning's own processing path is
what resolves the run (mark-uncollectible's resolve is explicitly best-effort,
documented as relying on "dunning runs scan the invoice status on next tick
anyway"), and the clawback's orphan guard is what voids the draft. An exclusion
that also hid written-off invoices would fix one starvation by creating another
— stranding the run active and the draft pending forever. Both directions are
mechanised, in each scan, by mutation-verified tests.

**The reconciler now records what it observes (migration 0167).** It polls
Stripe for every in-flight invoice on every sweep and, for a non-terminal
PaymentIntent, used to return without writing anything. That discarded
observation was a liveness bug: the sweep is `ORDER BY updated_at ASC LIMIT n`,
so a row nothing ever writes never ages, heads the queue permanently, and at
batch-size of them the sweep stops reaching newly-ambiguous charges entirely.
Not a hypothetical shape — this package's own contract notes that "async methods
legitimately sit in processing for days", and those are precisely the rows that
froze. The sweep now orders by least-recently-observed, so an unresolvable row
rotates to the back. Ordering, NOT exclusion, deliberately: these rows must keep
being polled, because the sweep is what notices when they finally resolve.

**What was deliberately NOT built.** An earlier version of this change added an
operator "cancel payment attempt" action, then replaced it with the reconciler
cancelling unreachable attempts automatically. Both were cut, and the reasoning
is worth keeping because it nearly shipped twice:

- The case they served is an ENGINE-minted PaymentIntent stalled on
  `requires_action`. Every engine charge is `Confirm:true` + `OffSession:true`
  with no `return_url`, and no `client_secret` is ever exposed to a customer
  (verified: the frontend has no payment-confirmation code at all), so such an
  attempt is unreachable by anyone — a genuine dead end.
- But Hardcount never sets Stripe's `error_on_requires_action`, whose documented
  purpose is exactly *"fail the payment attempt if the PaymentIntent transitions
  into requires_action"*. **The right fix is preventing the state, not
  remediating it** — one parameter instead of a cancel path, a provider-status
  allow-list, an invariant guard for hosted checkout, and their tests.
- And nobody could demonstrate the state occurs. Building remediation for a
  state we can neither produce nor rule out is how a codebase accretes.

**Open, with the experiment that settles it:** set `error_on_requires_action` on
the engine charge — gated on first confirming which error type it raises.
`classifyStripeError` maps `card_error`/`invalid_request` to a definite failure
(invoice `failed`, dunning retries — correct), but anything else falls to the
ambiguous default and would stamp `unknown`, trading a stuck `processing` for a
parked invoice. That is worse, so the flag is not set blind. The check is a
test-mode charge against `4000002500003155` (authentication-required) with
`off_session`, reading back what Stripe returns.

**How often:** only when the response is lost *and* the `payment_intent.*`
webhook never arrives. The webhook names the PaymentIntent and settles the
invoice through the ordinary path, which is what happens in nearly every real
occurrence. At zero customers the expected count is zero.

**Stuck-and-loud beats silently-double-charged.** That is the whole trade, and
it is the same one the no-silent-fallbacks rule makes everywhere else in billing.

## Still open: the same defect on the REFUND path

**Closed 2026-10-18** by [the amendment below](#amendment-2026-10-18--the-refund-half-is-done). This section is kept as written.

This ADR closed the "record a definite outcome for something we do not know"
class on the CHARGE path. The refund path still has it, and it is recorded here
rather than left in a branch description because an undocumented known defect on
a money path is how the same lesson gets paid for twice.

`creditnote/service.go` settles **any** refund error — including an ambiguous
timeout or 5xx — as `RefundFailed`. That is the identical false statement
`reconcileOne` used to make about charges.

It is materially less dangerous, and the reason is worth stating precisely: the
refund idempotency key is `hardcount_cn_<id>`, **invariant** — no attempt counter,
so settling `failed` does NOT rotate it. A retry therefore dedups against the
original and Stripe returns the existing refund. **A double refund is not
reachable.** What remains is (a) a credit note asserting "refund failed" when it
may have succeeded, which an operator and an auditor both read as fact, and (b)
no self-recovery — a human must press retry.

**Parked as #680**, alongside ADR-106's ledger. **Trigger:** the first ambiguous
refund outcome observed in reality, or production cutover — whichever comes
first. Resume it on top of this ADR: the shape of the fix is the same one that
worked here (stop asserting the unknown; let the provider's own answer settle
it), and the charge-side machinery — provider-status observation, sweep
rotation — is now built and can be reused rather than re-invented.

## Why not the ledger (ADR-106)

ADR-106 solved this properly: record the attempt before calling Stripe, recover
by replaying its idempotency key. The design is sound. The implementation did not
converge:

| Review round | Reviewed | Confirmed defects | Criticals |
|---|---|---|---|
| 1 | the original implementation | 14 | 4 |
| 2 | round 1's fixes | 30 | 4 |
| 3 | round 2's fixes | 34 | 10 |

Defect density **rose** each round, and each round found the previous round's
fixes broken — including one headline fix that was completely **inert** (a
deliberately non-transient error still classified as transient downstream,
because the classifier tests a type it is not), and two individually-correct
fixes that **cancelled each other out**. Several findings were "this fix has zero
executing coverage": fakes that had drifted from the SQL they stood for, so
mutation checks agreed with themselves.

~1,900 lines and a dozen interacting guards, versus ~30 lines that make the
failure unreachable. The standing rule is that accreting containment means the
model is wrong and the answer is the simplest COMPLETE abstraction. This is that
abstraction.

**Parked, not deleted.** The branch and ADR-106 remain. What the ledger buys
over this is *automatic* recovery — no human, no stuck invoice. That becomes
worth its complexity when stuck invoices are frequent enough to cost more than
the guards do.

**Amended 2026-08-01 — the parking is FORCED, not chosen.** This ADR reads as
though refusing to act were judged safer than replaying the attempt. It was not
a judgement: replay is *impossible* here. The parked write goes through
`UpdatePayment`, which bumps `charge_attempt_seq` unconditionally, so recording
the ambiguous outcome **rotates the derived idempotency key** — and `main`
persists the sent key nowhere. The only handle that could ever reach that
PaymentIntent is destroyed by the very write that records the problem.

That reframes ADR-106 as well. Its ledger is not an alternative safety posture;
it is the thing that *retains the handle*, which is why it can recover an invoice
this design must park. Its key derivation, which looked like incidental coupling,
is load-bearing — it doubles as a staleness detector, since there is no separate
seq column. See ADR-106's 2026-08-01 amendment, which corrects an earlier
version of this note that recommended self-keying the intent; that change would
have removed a signal, not a guard.

Also worth stating plainly, because this ADR's framing obscured it:
`classifyStripeError`'s ambiguous/definite boolean is the single point of failure
for the design below too — calling an ambiguous outcome definite settles
`failed`, bumps the seq, rotates the key and makes the invoice claimable again.
Parking is not immune to that; it merely stacks fewer decisions on top of it.

**A cheaper experiment than resuming the ledger — RUN AND REFUTED (2026-08-02):**
the no-bump-on-parked idea was enumerated, designed, and adversarially attacked;
a grounded BREAKS (a stale attempt row reconstructs a never-sent key and mints a
second PaymentIntent) plus collapsed economics (the wire key's PM suffix is
persisted nowhere) killed it. ADR-106 records the full refutation.

**And the "cannot be reconciled" premise of this ADR is now amended (2026-08-02,
[ADR-108](108-parked-invoices-search-and-adopt.md)).** This ADR reasoned as if
an unnamed PaymentIntent were unreachable. It is not: Stripe's PaymentIntent
Search API finds it by the `hardcount_invoice_id` metadata every engine PI carries
(SDK v82 surface verified; search works in test mode; 20 reads/sec, separate
per mode). What survives of this ADR's conclusion — on the TRUE ground — is
that search is eventually consistent, indexing "could be delayed during an
outage" (Stripe's words), and the outage that mints a parked row is the same
weather that delays its indexing. So ABSENCE from search results can never
carry a money write: the give-up write stays deleted, exactly as this ADR
decided. What changes is the other half: a PI search DOES find is a named PI,
and naming it is what this ADR always treated as the resolution. ADR-108 adopts
found PIs and writes nothing on absence; the parked state keeps this ADR's
floor (gauge, banner, write-off exit) and gains recovery as pure upside.

**Trigger to revisit:** the first real stuck `unknown` invoice, or production
cutover — whichever comes first. Resume it **on top of** this ADR, where its two
worst round-3 criticals (the give-up gate un-gating, and the reconciler treating
a never-sent request as proof of nothing) are unreachable code, because there is
no give-up write left to un-gate.

## Amendment 2026-10-18 — migration 0009's rollback stops shipping

[ADR-125](125-goose-migrations-from-one-v081-baseline.md) replaces the 178
migration files with one baseline at version 180, from v0.9.0. From that
release, `0009_payment_unknown.down.sql` is no longer in the binary.
`hardcount migrate rollback` runs only when `APP_ENV` is `local`, and it never
goes below version 180. So the warning under "What ships with it" applies
only to v0.8.1 and older binaries. The invariant itself does not change.

## Amendment 2026-10-18 — the refund half is done

The refund path now makes the charge path's split
([#965](https://github.com/gethardcount/hardcount/issues/965)). Its trigger,
production cutover, fired: the first design partners go live with real
cards.

- **An unclear refund result is `unknown`.** A credit-note refund whose
  Stripe call times out, loses its connection, gets a Stripe 5xx, a 409 or
  a 429 is recorded as `refund_status='unknown'`. A 409 means another
  request with the same idempotency key is still running. A 429 is
  unclear because stripe-go resends a request lost on the wire, under the
  same key, and stops at a 429, so the 429 can follow an attempt that ran.
  A refusal Stripe ran, such as a card error or an invalid request, is
  still `failed`. So is a 401 or 403: Stripe refused the API key before
  running anything, and every attempt of one call carries the same key.
  `classifyRefundError` is `classifyStripeError` with these rules read from
  the HTTP status.
- **The result is recorded even when the caller has gone.** A refund asked
  for through the API runs on the request context, which ends after 30
  seconds; when Stripe hangs, that happens during the call. The write that
  records the result runs detached from that context, with its own
  10-second limit, as the card settle does.
- **The key is kept, so a retry resends the same request.** The write
  that records `unknown` also stores the request's idempotency key in
  `credit_notes.refund_unknown_key`. That is the handle the charge path
  could not keep (amendment of 2026-08-01).
- **Retry refund looks first, then resends.** It searches Stripe for a live
  refund that carries the note's `hardcount_cn_id` metadata, and adopts one if
  found. Otherwise it resends the same request under the same key. Stripe
  then returns the first request's saved result, or answers 409 while that
  request still runs. Only a key Stripe never ran makes a new refund
  ([Stripe](https://docs.stripe.com/error-low-level#content-errors): "A new
  key can start a second operation"). A saved result can be days old, so
  Retry refund then reads the refund's current status. A note still at
  `none` is resent the same way, under Issue's key `hardcount_cn_<id>`: Issue
  may have sent it, and its result was never recorded.
- **A resend Stripe never ran stays `unknown`.** Stripe saves no result for
  a request it refused before running it, such as a 401 or a 403. On a
  resend, that answer says nothing about the earlier request, so the note
  keeps (or, from `none`, gets) `unknown` and the earlier request's key.
- **Only Retry refund moves `unknown`.** Refund webhooks match a stored
  refund id. An unknown note stores either no id or the id of a refund that
  Stripe already reported failed, so webhooks leave it alone. No sweep
  re-drives it, as with a parked invoice. It shows in refunds needing
  attention at once.

**Still open.** Stripe keeps an idempotency key for at least 24 hours and
may drop it after that. A resend after that runs as a new request. The
search before it adopts a refund Stripe already made. It cannot see a
refund that Stripe makes later, during incident recovery, for a request
that got a 5xx. For an outcome still unknown after 24 hours, Stripe advises
to reconcile, not resend
([Stripe](https://docs.stripe.com/error-low-level#post-requests)). Hardcount
does not stop a resend at that age yet. **Trigger:** the first unknown
refund older than 24 hours in production.

## Amendment 2026-10-19 — a parked invoice's stale hold is checked too

[ADR-127](127-a-lost-charge-result-is-resolved-by-asking-stripe.md) leaves a
charge's hold (`auto_charge_claimed_until`) set whenever its result is
unclear. A sweep in the reconciler asks Stripe about every hold that ended
more than 10 minutes ago, whatever the invoice's status. A parked invoice keeps
the hold its charge took, so the sweep lists the Stripe customer's
PaymentIntents for it (List, not Search, so it works in India too). A succeeded
payment it finds settles the parked invoice, as ADR-108's search would; anything
else only clears the hold and leaves the invoice parked. The park itself, and
the write-off as its exit, are unchanged; a parked invoice stays writable-off
whatever its hold says.

Unclear answers with no PaymentIntent id still park when the caller's context
is alive. Moving them onto the stale-hold check instead of parking is the
follow-up named in ADR-127.

## Amendment 2026-10-20 — a park ends without a person

[ADR-127](127-a-lost-charge-result-is-resolved-by-asking-stripe.md)'s
2026-10-20 amendment finishes every parked invoice in its stale-hold sweep,
through Stripe's List API. A payment the list shows is recorded (succeeded)
or followed (processing) at the first check. When the list shows none 24
hours after the attempt, the invoice goes back to `pending`, or `failed` when
a dunning retry parked it, and is charged again as usual under a new
idempotency key. Stripe may still create the payment for that long while it
recovers from an incident, which is why absence waits and a found payment
does not.

What stays: a parked invoice is never charged while it is parked
(`TestUnknownPaymentIsUnchargeableByEveryClaimPath`), absence never settles it
`failed`, and write-off stays allowed. Write-off is no longer "the exit": it
is the action for a customer who paid another way, and for a park Stripe
cannot be asked about (the key revoked, the customer deleted there). The
notice is a Warning that says what happens next, not a Critical that asks for
a write-off. The title stays: ADR titles are history.

A caller's deadline that ends the Stripe call now parks too: its outcome is
saved on a context detached from the caller (ADR-127, "By cause").
