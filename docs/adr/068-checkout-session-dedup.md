# ADR-068: Checkout session dedup — claim-first ledger, choke-point closes, loud anomaly settlement

**Date:** 2026-07-02
**Status:** Accepted (shipped in P2b-a; two build-time adversarial panels — 3 lenses each — amended the plan's sketch, see plan §4.2; amended by [ADR-100](100-currency-coherence-guard-ring.md) on 2026-07-26 and on 2026-10-11)

## Summary

Velox records a hosted Stripe Checkout session (the customer's payment page) in `checkout_sessions` before creating it, so an invoice has at most one open session. The Stripe call uses an idempotency key from that record and a 1-hour expiry, and paying, voiding or reducing the amount due closes the record in the same transaction. A second charge on a paid invoice, a captured amount that differs from the amount due, or a payment on a voided invoice raises a critical alert; the operator does any refund. Amended 2026-10-11: a hosted session and a Velox charge (auto-charge, dunning retry or Collect) now exclude each other, and a reconciler sweep records how each session ended. Status: active, amended 2026-10-11; [ADR-100](100-currency-coherence-guard-ring.md) added a currency-mismatch alert at settle.

## Context

`POST /v1/public/invoices/{token}/checkout` minted a **fresh** Stripe
Checkout session on every call: no idempotency key, no `ExpiresAt`
(Stripe default = 24h), no expiry on settle. A customer paying on two
devices — or re-opening a dunning email — was charged twice, the second
charge visible only in Stripe; the settle path silently dropped the
duplicate success (audit HIGH #5).

## Decision

### Claim-first ledger (`checkout_sessions`, migration 0125)

A POST **claims before it mints**: an `open` row is inserted in a
transaction that re-verifies the invoice payable under `FOR SHARE`
(serializes against the settle's `FOR UPDATE` paid-flip — a session can
no longer be minted after expire-on-settle already ran), rejects
in-flight charges (`payment_in_progress` 409 — dunning auto-retry racing
the Pay click), and re-anchors the amount on the locked row. The Stripe
create runs post-commit with an **idempotency key derived from the claim
id**: a crash before the row is filled, a concurrent loser, or any retry
re-drives the create and receives the **same session**. The partial
unique index (one open row per invoice) is the concurrent-double-POST
guard; the loser reads the winner's claim **on a fresh tx** (the unique
violation aborts the claiming tx — found by test) and returns the same
URL. `ExpiresAt=1h` is set **on the Stripe create params** (a DB-only
TTL leaves a ~23h invisible payable session); Stripe's clamped actual
expiry is persisted, and the reuse read refuses rows at/past it
(CAS-supersede, exactly one racer remints).

**Amount drift** (a credit note legally changes `amount_due` while a
session is open — "open" is not "in flight", so ADR-059's CN guard does
not apply): the POST branches on the Stripe-side truth before minting —
a **completed** session means the customer just paid the old amount, so
no new session is minted (409; the webhook settle + escalation own it);
already-expired proceeds; a network failure is the accepted ≤1h residual.

### Exits from payable close claims at store choke points, in-tx

The panel counted 9+ scattered "exit from payable" writers vs the
sketch's four categories. Instead of per-call-site hooks, the claim rows
close **inside the exiting transaction** at the three store choke points
everything converges on: `markPaidReportingTransition` (every MarkPaid
variant), the void/uncollectible status transition, and **both**
`amount_due` reducers (`ApplyCreditNoteTx`, `ApplyCredits` — any
reduction supersedes the claim; its minted amount is stale). The
Stripe-side network expire is post-commit best-effort (settle-time sweep
over every unresolved session for the invoice, including previously
superseded ones); per-session `ExpiresAt` is the backstop.
`checkout.session.completed` truth-syncs the row.

### Loud anomaly settlement

`SettleSucceeded` now takes the **captured amount** (the webhook parses
it; previously discarded) and escalates three per-cause anomalies via
one channel — `slog.Error` + outbound event + a **durable marker** on the
invoice (migration 0126) that drives a **Critical attention banner which
pierces the classifier's terminal-state early-return** (a duplicate
charge fires on an invoice that is already *paid*; with auto-refund
deferred, the operator is the refund mechanism):

- `payment.duplicate_charge` — a second, *different* PI succeeded on a
  paid invoice. The `transitioned=false` skip point compares against the
  row **returned by the FOR-UPDATE transition**, never the caller's
  stale snapshot (checkout invoices record their PI only at settle — the
  stale comparison would false-alarm every routine concurrent same-PI
  redelivery). An empty recorded PI escalates (credits/offline-settled
  invoice hit by a live card charge).
- `payment.amount_mismatch` — captured ≠ booked at settle (the drift
  residual must be *detected*, never silently booked: `amount_paid`
  drives the refund cap).
- `payment.received_on_voided_invoice` — the transition's InvalidState
  on a voided target now escalates its own cause and **absorbs** (the
  webhook no longer retries a terminal invoice forever). The money is
  owed back — distinct from a double-collect.

## Accepted residuals (do not re-litigate)

≤1h drift/orphan windows bounded by Stripe-side ExpiresAt when a network
expire fails; pre-deploy sessions are outside the ledger (expire ≤24h —
upgrade note in CHANGELOG); Stripe-account rotation mid-session (expire
404s on the old account) — bounded by the same 1h; no auto-refund — the
attention banner + per-cause events are the operator's refund trigger. (A
session-sweeping reconciler was deferred here; it shipped with the
2026-10-11 amendment.)

## Test locks (mutation/fault-verified)

`TestClaimOpen_ConcurrentDoublePOST_OneWinner` (real PG),
`TestClaimOpen_PayableRecheck`, `TestSupersede_CASExactlyOnce`,
`TestChokePointCloses` (MarkPaid/void/credit; strip a choke statement →
fail), the four `TestSettleSucceeded_*` escalation seams (incl. the
stale-snapshot false-alarm mutation), and the attention-pierce test.
Stripe-interaction legs (idempotency-key convergence, ExpiresAt params,
drift branching) are covered by the MANUAL_TEST flow — no HTTP-level
Stripe stub harness exists in-repo yet; building one is named follow-up
work, not silently skipped.

## Amendment 2026-10-11: one payer at a time

**What changed.** The ledger stopped two hosted sessions paying one
invoice, but nothing stopped a hosted session and a Velox charge (the
automatic charge, a dunning retry, or Collect) from both taking money. Now
they exclude each other:

- **Velox charges skip an invoice the customer may be paying.** The three
  charge claims refuse while `domain.HostedCheckoutLiveSQL` holds: an
  unfilled claim for 65 minutes, a recorded `open`/`superseded` session
  until the hosted sweep records how it ended, and a `completed` session
  until its payment settles. Recorded sessions stop blocking 24 hours after
  expiry regardless, so one the sweep cannot resolve delays collection
  instead of stopping it. Each claim locks the invoice row before its
  UPDATE: the hosted claim holds `FOR SHARE` but never changes the row, so
  a single-statement claim would proceed on a snapshot taken before the
  session existed (reproduced by
  `TestChargeClaim_SeesHostedClaimCommittedWhileItWaited`). The dunning
  due-run scans apply the same fragment, so a blocked run does not hold
  the head of the queue.
- **No hosted session while Velox charges, or after an unrecorded hosted
  payment.** The hosted claim also refuses while the charge lease is held
  (a charge on its way to Stripe is still `pending`), and while a
  `completed` session's payment is unrecorded.
- **The hosted sweep** (`payment/reconciler_hosted.go`, part of the
  payment reconciler) replaces the deferred session reconciler above. On
  every run it asks Stripe about each unresolved session that has gone 5
  minutes unchecked (the webhook's head start): a paid session is final at
  Stripe as soon as the customer pays, so it is recorded on the next run
  rather than after expiry. It also settles any `completed` session whose
  invoice is still unpaid. It calls
  `SettleSucceeded` directly, so a hosted payment on an invoice Velox
  already charged raises `payment.duplicate_charge` even when no webhook
  ever arrived, and skips a payment the webhook already escalated (the
  invoice's anomaly marker names its PaymentIntent) so one charge raises
  one alert.
- **Fixes on the session path:** the Stripe expiry is fixed at claim
  creation + 1h (a clock-relative expiry changed the params under a fixed
  idempotency key, so every re-drive failed); a session Velox failed to
  record is never handed out, nor is one whose claim was replaced while
  its create was in flight (it is expired instead); a pending claim too old
  to re-drive (Stripe requires an expiry at least 30 minutes ahead) is
  superseded; and a
  session a credit apply superseded is expired at Stripe before a new one
  is minted.

**Peers.** Stripe Billing confirms one PaymentIntent per invoice from both
the hosted page and auto-collection; Kill Bill's hosted page only saves the
card and Kill Bill charges under a per-account lock; Recurly offers hosted
pay only on manual-collection invoices. Lago delays the first automatic
charge 10 minutes, which narrows the race without closing it. A hold column
with its own release reconciler and operator action was designed and
rejected as heavier than the risk: the session table already carries the
state. An abandoned page blocks Velox charges until the first reconciler
run after its expiry (runs are hourly in production); a paid one until the
payment is recorded, by its webhook or the next run. The 24-hour cap is the
backstop.

**Residual.** An operator records an offline payment while the customer
completes a hosted session and that payment's webhook is lost: the
`completed` row's invoice is paid, so the sweep does not look at it and no
duplicate alert fires.
