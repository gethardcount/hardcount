# ADR-098: Postmark delivery/bounce/complaint webhook ingestion

Date: 2026-07-19
Status: Accepted
Amends: ADR-082 (whose "Still cut" list deferred email provider events)

## Summary

Velox ingests Postmark's delivery, bounce and spam-complaint webhooks at `POST /v1/webhooks/postmark`, and ignores opens and clicks. Postmark answers SMTP 250 before it knows whether a message will deliver and reports bounces only later, so without this the dashboard could not show delivery and bounced addresses were never suppressed. The endpoint uses HTTP Basic Auth (`POSTMARK_WEBHOOK_USER`, `POSTMARK_WEBHOOK_PASS`), refuses every post with 401 when they are unset, and finds the tenant only through the outbox row id each email carries. A verdict sets `email_outbox.delivery_state`, which only moves to a more severe state; a hard bounce or spam complaint also flags the customer's email, and a soft bounce changes nothing. Status: active; it partly reverses [ADR-082](082-email-recipient-semantics.md)'s cut of provider events, and with other SMTP providers the state stays `unknown`.

## Context

Velox marks an outbox row `dispatched` on SMTP 250. For an
async-validating provider — Postmark, our recommended default — that 250
is only "accepted for handoff": Postmark decides *afterwards* whether the
message delivers, bounces, or draws a spam complaint, and reports each
verdict via webhook. Two live findings from the 2026-07-19 FLOW E
walkthrough forced this ADR:

1. **No `delivered` state existed.** The operator UI said "dispatched
   everywhere" — a genuinely delivered invoice email and a silently
   400-rejected one (unverified sender signature) looked identical.
2. **The sync bounce path is dead with Postmark.** Our RCPT-time 5xx
   classifier (`isPermanentSMTPBounce`) never fires because Postmark
   never rejects at RCPT — every bounce arrives asynchronously, so
   recipient suppression silently stopped working the day we moved off
   direct-SMTP providers.

Parity (live-doc verified): SendGrid/Resend/Mailgun/SES all model
`delivered` as a state distinct from `sent`; all suppress on hard bounce
+ spam complaint and **never** on soft bounce; webhook ingestion is the
standard mechanism. Billing peers (Stripe Invoicing, Lago, Metronome)
expose *no* recipient-level delivery state — ingesting it puts Velox
AHEAD of peers at the cost of one webhook endpoint.

## Decision

One platform-level endpoint — `POST /v1/webhooks/postmark` — ingesting
`Delivery`, `Bounce`, and `SpamComplaint`. Everything else
(`Open`/`Click`/unknown) is acked and ignored.

### Correlation: stamped metadata, nothing else

The dispatcher puts the outbox row id on the send ctx
(`email.WithCorrelation`); the MIME builders emit it as
`X-PM-Metadata-vlx-outbox-id`. Postmark strips the header before
delivery and echoes it in every webhook's `Metadata`. Inbound, that id
is the **only** tenant resolution: resolve the row (bypass-RLS read, the
blind-index pattern), take `tenant_id` + `livemode` from the row, never
from the blind POST. The key is hyphenated because Postmark's metadata
docs only demonstrate hyphenated keys; the value is our own
96-random-bit id, which doubles as the per-event capability. Events
without resolvable metadata are acked + WARNed — never a guessed tenant
(mis-attributing a bounce across tenants sharing an email would corrupt
another tenant's suppression state). We deliberately do NOT stamp
tenant_id/livemode as extra metadata (the row already carries them
authoritatively; a second copy could disagree) and do NOT store
Postmark's MessageID (no consumer; not unique across a message's
lifecycle events anyway).

### Trust model: Basic Auth, not HMAC — and why that's OK

Postmark signs nothing; its webhook auth is HTTP Basic Auth on the
configured URL. So the Stripe shape (per-tenant endpoint id → per-tenant
HMAC secret) does not transfer — Postmark is ONE platform account
(env `SMTP_*`) serving all tenants. The gate is constant-time Basic Auth
(`POSTMARK_WEBHOOK_USER`/`POSTMARK_WEBHOOK_PASS`, same config class as
the SMTP creds) checked before the body is read; unconfigured creds
reject everything (401 + boot WARN) rather than standing open. An
attacker with the credential still can't cross tenants: effects require
a live outbox id, and each id only ever resolves to its own tenant's
row. Response codes are tuned to Postmark's retry contract (only 403
stops retries; Delivery retries just ~3×/21min): bad auth → 401
(retryable, visible in Postmark's activity feed), deterministic garbage
→ 200 ack + WARN (retry can't fix it), transient DB failure → 5xx
(redelivers into idempotent writes).

### State model: two orthogonal columns, both monotonic

- `email_outbox.status` (send lifecycle) is **unchanged** — single
  writer stays the dispatcher. Overloading it with delivered/bounced
  would put two writers on one column and lose the
  delivery-report-before-dispatched-mark race.
- `email_outbox.delivery_state` (migration 0156):
  `unknown/delivered/bounced/complained`, written only by this webhook,
  **severity-monotonic** — a write lands only if it outranks the current
  state, so at-least-once redelivery and Delivery-vs-Bounce reordering
  converge to most-severe without a dedup table. Reflects the PRIMARY
  recipient only (a CC-address event never flips the row — ADR-082
  attribution).
- `customers.email_status`: hard bounce reuses `MarkEmailBounced`
  verbatim (blind-index → all in-tenant matches); SpamComplaint gets the
  writer 0050 anticipated but never had — `MarkEmailComplained`, firing
  the new per-cause `customer.email_complained` event. `complained`
  outranks `bounced`: `MarkEmailBounced` now no-ops (not errors) against
  a complained row. **Soft/transient bounce writes nothing** — every
  major ESP retries-then-surfaces and none suppresses on it; permanence
  is delegated to the provider's own verdict (`Inactive` ∨
  `HardBounce`), the same conservative bias as the SMTP classifier
  (sender-side codes like DMARCPolicy must never mark a recipient dead).
  `Delivery` never writes `email_status='ok'` — delivered ≠ inbox, and a
  late delivery of an earlier-queued message must not un-suppress a
  bounced address.

### Idempotency without a dedup table

Deliberate collapse: our effects are intrinsically idempotent (monotonic
`delivery_state`, guarded `email_status` lattice) and the timeline is
DERIVED from row state, not per-event append rows — a Stripe-style
events table would buy zero correctness. **Named fallback trigger**: the
day the timeline emits independent per-event append rows for
delivery/bounce (playbook class J), those appends are not idempotent and
a `UNIQUE(tenant_id, livemode, provider_event_id, record_type)` dedup
table becomes load-bearing — build it then.

### Surfacing

The invoice timeline layers the verdict over the send row ("Invoice
emailed to customer — delivered / — bounced / — recipient marked it as
spam"); the same pass fixed the pre-existing `skipped` fall-through that
rendered a deliberately-not-sent email as "succeeded". The customer
page's Sent-emails badge shows the provider verdict when one exists.

## Non-Postmark SMTP (Mailpit, Zoho, SES…)

`delivery_state` simply stays `unknown` and the UI shows what it always
showed. The header is stamped regardless (any `X-PM-*`-ignoring relay
passes it through harmlessly, and our own id leaks nothing). Honest
limitation, documented rather than papered over: delivery confirmation
exists only where the provider reports it.

## Cut (named triggers)

- Per-provider webhook abstraction (`/v1/webhooks/email/{provider}`) —
  one live ESP; generalize on the second.
- `provider_message_id` column — no consumer; add with a Bounce-API
  reconciler or Open/Click ingestion.
- `delivered_at`/`bounced_at` timestamp columns — add when an operator
  asks to see delivery times.
- Soft-bounce `deferred` state — add when operators ask for
  retry-in-progress visibility (Resend's `delivery_delayed` shape).
- Postmark egress-IP allowlisting — layered defense, needs the CIDR
  list; Basic-Auth-over-HTTPS is the gate today.
- Suppression reactivation UI — operator-only decision, deferred with
  the rest of the suppression-management surface.
