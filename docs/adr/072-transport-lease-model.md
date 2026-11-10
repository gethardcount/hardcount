# ADR-072: Outbox transport — claim-lease with detached CAS marks, derived constants

**Date:** 2026-07-03
**Status:** Accepted (shipped in P5; designed by the P5 adversarial panel — 2 lenses, both SHIP-WITH-FIXES; consolidated protocol in the remediation plan §4.5) (amended 2026-08-30, 2026-10-13, 2026-11-10)

## Summary

The email and webhook outboxes (tables of messages waiting to be sent) claim a batch in one short transaction that stamps a time-limited lease, then send each row outside it. Each row's result is saved in its own small update, so a rollback never re-sends a delivered email. A retry or a give-up applies only to a row still `pending` under the claim that made the attempt. An email's sent mark applies to any row not already sent. A webhook event commits together with its row's sent mark, or not at all. Delivery is at-least-once, with at most one duplicate per crash or expired lease, and a `failed` row waits for an operator to requeue it. Amended 2026-08-30 and 2026-10-13: no retention window exists, and a sent or skipped email row drops its links and PDF from the stored payload. Amended 2026-11-10: the retry, give-up and sent-mark rules above ([amendment](#amendment-2026-11-10--what-each-mark-may-change)). Amended 2026-11-10 again, for the webhook delivery worker, which POSTs each event to its endpoints. It saves each result on a detached context, and counts a panic as one failed attempt. It gives up a delivery still pending 50 hours 36 minutes after it was created ([amendment](#amendment-2026-11-10--guards-for-the-delivery-worker)). Amended 2026-11-10 a third time: in both outboxes, a handler panic is one failed attempt on the retry schedule, logged with its stack, and an email is no longer given up on its first panic ([amendment](#amendment-2026-11-10--a-handler-panic-is-one-failed-attempt)). Status: active, amended; each worker now runs on one replica under a leader lease ([ADR-114](114-leader-leases-tick-scoped-fencing.md)).

## Context

Both outboxes (email, webhook events) drained batches inside ONE
transaction: claim rows FOR UPDATE, run every handler (real SMTP/HTTP
round-trips), then commit all marks together. Two consequences:

1. **Lost marks re-sent delivered messages.** The 60s batch timeout
   versus a 10×~55s worst-case batch meant a slow row cancelled the tx
   after earlier rows had already been DELIVERED — their dispatched
   marks rolled back, and the next tick re-sent real invoices.
2. **The webhook event fan-out dual-wrote.** `Dispatch` committed the
   event, then each goroutine inserted its own delivery row (a crash
   between = an event no row references, never delivered, nothing to
   retry) with `next_retry_at = NULL` (the retry worker racing the
   goroutine = double-POST); the producing `webhook_outbox` row was
   marked in yet another tx (a crash between = a duplicate event with a
   fresh id receivers cannot dedupe).

## Decisions

1. **Claim-lease batches.** One short claim tx per tick:
   `attempts = attempts + 1, next_attempt_at = now() + ClaimLease`
   over `FOR UPDATE SKIP LOCKED` rows, committed immediately. The lease
   is both the crash-recovery bound and the cross-worker exclusion; the
   claim is the **single attempts-increment site** (a claim-then-crash
   row still advances toward DLQ; marks never increment).
2. **Derived constants, never hand-tuned.** Email
   `PerRowBudget = 60s` (suppression 5s — newly bounded — + branding 5s
   + dial 10s + SMTP exchange 30s + bounce report 5s, enforced by a
   per-row ctx); `ClaimLease(n) = n×PerRowBudget + 60s`;
   dispatcher defaults `BatchSize=5`, `BatchTimeout=300s`. The invariant
   chain `BatchSize×PerRowBudget ≤ BatchTimeout < ClaimLease` is
   CI-locked (`TestP5_LeaseInvariantChain`). The webhook side SPLITS
   what was one constant: `birthLeaseWindow = 120s` leases a
   freshly-created delivery row to its in-process first attempt;
   `retryClaimLease = claimBatch×13s + 60s` (claim batch 10) covers a
   claimed retry batch. One constant provably cannot serve both — sized
   for the batch it starves first delivery for ~23 minutes; sized for
   birth it double-POSTs under a slow batch.
3. **Marks are detached and CAS.** Every per-row mark runs on
   `context.WithoutCancel + 5s` — work already performed is always
   recorded, which is what actually kills the delivered-email-re-sent
   bug (the row-start budget gate is only a duplicate-window shrink) —
   and guards `WHERE status='pending'` so a stale writer (lease expired,
   a sibling already resolved the row) is dropped and logged, never
   applied. `UpdateDelivery` returns `ErrStaleDeliveryMark`; no caller
   discards mark errors anymore.
4. **Atomic fan-out + handler-owns-mark.** `DispatchFromOutbox` creates
   the event, every matching delivery row (born leased —
   `next_retry_at` is never NULL), and the producing outbox row's
   dispatched-mark in ONE tx. The outbox drainer's own success mark is a
   CAS no-op backstop. Replay batch-creates its born-leased rows in one
   tx against the already-committed clone (an interrupted replay is
   operator-visible and re-clickable).
5. **Redelivery semantics per row state**: `pending` = at-least-once —
   exactly one duplicate possible per crash/lease-expiry event;
   `dispatched` = terminal-sent; `failed` = terminal-unsent until an
   operator requeues:
   `UPDATE email_outbox SET status='pending', attempts=0, next_attempt_at=now() WHERE id='…' AND status='failed'`.
   Permanent errors (payload decode, suppressed recipient, permanent
   SMTP 5xx) go to `failed` immediately instead of riding 15 backoff
   slots over ~72h.
6. **One SMTP path, strict by default.** All three modes flow through a
   single `deliver()`: `DialContext(10s)`, exchange deadline
   `min(30s, ctx remaining)`, **strict STARTTLS** (the old
   `smtp.SendMail` path upgraded opportunistically — a MITM stripping
   the advertisement silently downgraded invoice traffic to plaintext),
   AUTH only when configured AND advertised, `SMTP_TLS=none` forbidden
   when `APP_ENV=production` (fails loudly at send). **Breaking**: a
   config pointing `starttls` at a no-TLS sandbox now errors — set
   `SMTP_TLS=none` explicitly (local compose/docs already do); the boot
   log prints the active mode for self-diagnosis.
7. **Retry worker leader-gated** (`LockKeyWebhookRetry`) — the claim
   lease alone is a correct multi-replica guard, but gating makes its
   sizing non-critical, matching both outbox dispatchers.

## Accepted residuals

- **Deferred from P5's Closes list (recorded per the amend-decisions
  rule): `outbox_sender.go` raw tokens at rest.** Password-reset and
  member-invite emails store their single-use token URLs in
  `email_outbox.payload` jsonb until dispatch (and after: see the
  2026-08-30 amendment — no retention window exists). The
  fix is payload-field encryption (the customer-PII encryptor is the
  natural tool) — deferred as a named backlog item rather than rushed
  into this transport rewrite; mitigations today: tokens are single-use,
  reset tokens expire in 1h, and DB access is already the crown jewels.
- Pre-P5 pending webhook deliveries may carry NULL `next_retry_at`; the
  claim SQL's NULL branch is retained (with a legacy-rollout comment +
  regression test) until no such rows can exist.
- The retry path re-signs with a fresh timestamp per attempt (unchanged
  behavior); receivers must tolerate at-least-once delivery, as
  documented since ADR-040.

## Test locks (real Postgres, mutation-verified 2026-07-03)

Email: lease-invariant chain; mark survives batch-ctx death (mutation:
mark on batch ctx → fails); permanent-error immediate DLQ; panic row
doesn't strand the batch; two concurrent claimers disjoint over 10 rows;
strict-STARTTLS refusal against a live no-TLS fake + prod-none
rejection. Webhook: event+deliveries one tx born-leased (retry worker
can't steal inside the birth lease); UpdateDelivery CAS drops a stale
failure-mark after a sibling's success (mutation: guard removed →
fails); handler-owns-mark (outbox row dispatched atomically, replayed
tick re-claims nothing, exactly one event). Pre-existing claim-lease +
NULL-branch regression suites stay green.

**Amendment 2026-08-30 — the webhook-outbox drainer's claim is now pinned
too.** The 2026-07-03 locks above cover the email outbox and the webhook
retry worker; `webhook.OutboxStore.ProcessBatch` (the `webhook_outbox`
drainer) had no concurrent-claimer test — its `FOR UPDATE SKIP LOCKED` +
lease claim was held by convention. Two real-Postgres tests in
`internal/webhook/outbox_integration_test.go` close that:
`TestOutbox_ProcessBatch_SkipsRowsLockedBySibling` (a sibling transaction
holds row locks on half the due set; the claim must neither block nor
take those rows, and must take exactly them after release) and
`TestOutbox_ProcessBatch_ConcurrentClaimersDisjoint` (two dispatchers race
the same 10 rows; every row attempted exactly once). Mutation matrix
against the real store: deleting `FOR UPDATE SKIP LOCKED`, or keeping
`FOR UPDATE` without `SKIP LOCKED`, turns the sibling test red (claim blocks
until the 5 s ctx expires) while the race test stays green; dropping the
lease stamp from the claim UPDATE turns the race test red (rows attempted
twice) while the sibling test stays green — each mechanism has exactly one
test that fails for it.

## Amendment 2026-08-30 — no 30-day retention exists

The residual above said dispatched `email_outbox` rows carry a "+30-day
retention". Nothing implements one: the only 30-day figure in the code is
`ListByCustomer`'s read window (`internal/email/outbox.go`), not a
deletion. Dispatched rows persist indefinitely — and must, because they
are the invoice-email delivery record (the invoice timeline's
`ListByInvoice` has no age bound; the Postmark bounce/delivery webhook
correlates on the row id). So the token-URL exposure is unbounded in time
and the fix is a post-dispatch scrub of secret-bearing payload fields, not
row deletion; the mitigations listed (single-use tokens, 1 h reset expiry)
stand. `webhook_outbox` dispatched rows are pure transport intent —
`webhook_events`/`webhook_deliveries` are the replayable record — and are
safe to prune at any age; Hardcount does not do so automatically (register:
outbox-pruner, trigger recorded in velox-ops).

## Amendment 2026-10-13 — sent and skipped rows drop their secrets

The scrub named above is in. The mark that sets a row `dispatched` or
`skipped` also removes six keys from `payload`, in the same UPDATE:
`setup_url`, `update_url`, `password_reset_url`, `invite_url`,
`public_token` and `pdf` (`scrubbedPayloadKeys` in
`internal/email/outbox_sender.go`). Every other key stays, so the invoice
timeline, the Sent-emails panel and the Postmark webhook read what they
read before (`to`, `invoice_number`, `attempt_number`, `trigger`).

What is still stored unencrypted, and for how long:

- A `pending` row holds all six keys until it is sent. That is seconds in
  normal running and up to about 72 hours while a send is retried.
- A `failed` row keeps its whole payload with no time limit, so the
  requeue in decision 5 still sends a complete email. Deleting or
  requeueing the row is the operator's call.
- Rows marked `dispatched` or `skipped` before this change were not
  rewritten. No backfill ships with it.

A sent row can no longer be sent again by flipping it back to `pending`:
its link and PDF are gone. Send a new email from the dashboard or the API
instead. The residual's other proposal, encrypting the payload fields of
unsent rows, is not done.

## Amendment 2026-11-10 — what each mark may change

Decision 3 guarded every mark with `status = 'pending'` alone. That is not
enough when two claims of one row overlap.

A claim outlives its lease when its process freezes, or when the database
clock steps forward. A second claim then takes the row while the first
handler is still at work. This needs two or more replicas. With
`status = 'pending'` alone, the first claim's late mark changed the row
under the second claim:

- A late retry mark moved `next_attempt_at` from the end of the new claim's
  lease to seconds away, so a third claim could take the row.
- A late give-up marked `failed` a row that the new claim then sent. The
  requeue of `failed` rows in decision 5 sent it again.
- A late email send, after the new claim gave up or skipped, was dropped.
  The row read `failed` or `skipped` for a delivered email.
- The webhook handler's sent mark was not checked when it matched no row.
  Both claims could commit an event for one row, each with its own id,
  which a receiver cannot drop as a repeat.

**The rule now:**

| Mark | Applies to | Why |
|---|---|---|
| Retry and give-up, both outboxes | A row still `pending` whose `attempts` equals the value this claim returned | The claim is the only place that adds to `attempts`, so each claim holds a value no earlier claim holds. |
| Email sent | Any row not already `dispatched`. It also clears `skip_reason`. | A send that happened is recorded, whichever claim made it. It outranks another claim's give-up or skip. Dropping it would leave the row to be sent again. |
| Email skipped | Any `pending` row, as before | A skip does not go stale: the invoice states the check reads only move toward settled, so a later claim would skip too. If a later claim did send, its sent mark replaces the skip. |
| Webhook sent, in the event transaction | A row still `pending`, and the row count is checked | On zero rows `CreateEventWithDeliveries` returns `ErrOutboxRowNotPending`, and the event and its deliveries roll back. The update locks the row, so the second of two transactions waits, then finds the row `dispatched`. One row makes one event. |

The webhook outbox's own success mark after the handler, the backstop in
decision 4, is unchanged.

**What stays possible:**

- Two overlapping claims can both send one email (decision 5, unchanged).
  SMTP has no idempotency key, and no database check stops a send that
  another process has already started.
- A live claim's give-up still counts in
  `hardcount_email_outbox_dead_lettered_total` and logs at ERROR, even when
  a stalled claim's send later marks the row sent.
- The requeue in decision 5 resets `attempts` to 0. A stalled claim's retry
  or give-up can then match a later claim that reaches the same value. The
  stall must last through a give-up, the requeue, and as many new claims
  again.
- Two event transactions for one row are rare. The required
  `idle_in_transaction_session_timeout`
  ([Postgres requirements](../ops/postgres-requirements.md)) ends a handler
  frozen inside its event transaction before the lease ends. Without the
  setting, or on a forward clock step, the row-count check still keeps the
  two to one event.

The overlaps are tested against real Postgres in
`internal/webhook/outbox_claim_fence_integration_test.go` and
`internal/email/outbox_claim_fence_integration_test.go`. Each test ends one
claim's lease from inside its handler and runs a second `ProcessBatch` on
the same row.

## Amendment 2026-11-10 — guards for the delivery worker

The delivery worker POSTs each webhook event to its endpoints and records
the result in `webhook_deliveries`. Decision 3 says every per-row mark runs
on a detached context, and both outboxes recover a panic per row. The
delivery worker did neither, and nothing ended a delivery that never
recorded a result.

**The rule now** (`internal/webhook/service.go`):

| Guard | Rule | Why |
|---|---|---|
| Detached mark | Every delivery mark goes through `markDelivery`: `context.WithoutCancel` plus 5 seconds. | A retry's context ends with its tick: at shutdown, on a lost leader lease, or when the row's 13-second budget runs out. A `2xx` left unrecorded was sent again when the lease ended. |
| A POST cut short by its tick | Not counted. The row keeps its claim lease. | The receiver did not fail. Counting it would use up one retry of every delivery in flight at each deploy. |
| Panic | Both goroutines that send recover: an event's first attempt, and each row of the retry worker. The panic is logged with its stack and counted as one failed attempt. | No caller could recover for them, so a panic ended the process. Uncounted, the row was claimed when its lease ended and panicked again. |
| Panic in a dial hook | The client's two dial hooks, which check an address before it connects, recover for themselves (`recoverDialHook`). A panic is logged with its stack and returned as an error: the connection is refused, and the POST counts as one failed attempt. | `net/http` runs the transport's `DialContext` on a goroutine of its own, and `net.Dialer` runs `Control` on a further one for a host with both IPv4 and IPv6 addresses. The guard above does not cover them. |
| Age bound | A delivery still `pending` 50 hours 36 minutes after `created_at` is marked `failed` at its next claim, with no POST. `attempt_count` is unchanged. | Only a mark moves `attempt_count`. A delivery whose endpoint or event read kept failing, or whose mark did, was claimed at the end of every lease, for good. |

The age is the retry ladder plus a margin. The ladder is 26 hours 36
minutes, summed from `retryBackoffs`, so a change to the schedule moves the
bound. The margin is 24 hours. It keeps the bound off a delivery that is
still on its ladder. Such a delivery makes its last attempt one ladder
after its first, plus three delays: jitter, the wait for a tick, and any
time the worker was not running. In normal running they add up to minutes.
A wide margin costs little. A row that never records a result takes one
claim slot each claim lease until the bound.

`attempt_count` is not moved to the claim, as the outboxes' `attempts` is.
The API returns it as the number of attempts made, and a claim that sends
nothing is not an attempt.

**What stays possible:**

- A delivery delayed by more than a day in total is given up without its
  remaining attempts. That needs the worker stopped or far behind for that
  long. The operator replays it.
- A delivery past the age stays `pending` until its next claim. For one
  that counted a failure just before the age, that claim is the retry the
  ladder set, up to 24 hours later.
- A mark that panics or fails every time leaves the row `pending`. Each
  try is logged. `hardcount-doctor` flags a delivery still pending 25
  hours past the age (`webhook_delivery_pending_past_give_up`). The 25
  hours cover the ladder's longest wait, so the check does not flag a
  delivery that is waiting for a retry.
- The two dial hooks are the only code of this package that Go's `net`
  and `net/http` run on goroutines of their own today. A new hook there,
  such as a request body that this package reads, needs a recover of its
  own.
- A POST cut short by its tick may have reached the receiver. It is sent
  again after the lease, which decision 5 already allows.

Tested against real Postgres in
`internal/webhook/delivery_guards_integration_test.go`. Removing a
recover, putting the mark back on the tick's context, or removing the
bound each turns its test red. One recover has no such test: the `defer`
in `dialControl`. Its body has no seam through which a test can make it
panic. `recoverDialHook` itself is tested in a `Control` hook on a real
dialer (`internal/webhook/delivery_guards_test.go`).

## Amendment 2026-11-10 — a handler panic is one failed attempt

An email whose handler panicked was marked `failed` on its first attempt.
The recover wrapped the panic in `ErrPayloadDecode`, which is permanent. The
webhook outbox counted the same panic as one failed attempt and retried it.
Neither logged the stack.

**The rule now** (`attemptRow` in `internal/email/outbox.go` and
`internal/webhook/outbox.go`):

- A handler panic is one failed attempt, like any other error. The row waits
  out the retry schedule (the backoff ramp) and is `failed` after its 15th
  attempt. The email dead-letter metric counts it with
  `cause="max_attempts"`.
- The recover logs the panic at ERROR with its stack. `last_error` holds the
  panic value.
- A payload that cannot be decoded is still `failed` on its first attempt.
  The email handler itself refuses it with `ErrPayloadDecode`.

A panic is a fault in the code, not in the row. A deploy that fixes it can
land before the schedule ends, and the email then goes out with nothing for
an operator to do. The runbook's requeue of failed emails selects rows with
15 attempts, so it skipped an email that a panic had failed at once.

**What stays possible:**

- A panic after the relay accepted the email sends it again at the next
  attempt, which decision 5 already allows. After the relay accepts, the
  send path only closes the connection and writes one log line.
- A row that panics on every attempt is retried for about 52 hours, and each
  attempt logs a stack.

Tested against real Postgres in
`internal/email/handler_panic_integration_test.go` and by
`TestOutbox_ProcessBatch_PanicIsRetried` in
`internal/webhook/outbox_integration_test.go`. Wrapping the panic in
`ErrPayloadDecode` again, or dropping the stack from either log line, turns
its test red.
