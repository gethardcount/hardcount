# ADR-024: Activity timeline — keep multi-source assembly; promote canonical primitives only on trigger

**Status:** Accepted
**Date:** 2026-05-05

## Summary

The invoice activity timeline is still assembled at read time from several sources (lifecycle columns, charge attempts, dunning, emails, credit notes), with no dedicated `invoice_events` table. Most billing systems work this way, and an events table would be a multi-week refactor with no current need behind it. Cursor fetch, ETag responses, a raw-payload endpoint, a generalized coalescer and an events table each wait for the trigger named below. Shipped since: a `source` field on every row (now `lifecycle`, `payment`, `dunning`, `email`, `credit_note`) and a server-set `is_simulated` flag. Status: active; [ADR-103](103-single-payment-source.md) and [ADR-104](104-one-activity-lane-entity-calendar.md) later reshaped the payment rows and the timeline layout.

## Context

Operator review of the invoice detail activity card surfaced visual heft
on dunning-heavy invoices and a fair design question: should the
timeline be re-architected to a dedicated `invoice_events` table,
modeled after event-sourcing patterns?

A survey of comparable billing systems found the answer is **no, that's
not the canonical shape**:

| System | Activity timeline backing |
|---|---|
| Stripe | Internal; their `/v1/events` API powers webhook delivery, not specifically dashboard activity rendering. |
| Lago (OSS — verified) | Multi-table assembly. `invoice_events` exists but for *business events* (usage logged, plan changed), NOT activity. Activity is joined from invoice + webhook + audit. |
| Recurly | Multi-source assembly across `transactions`, `events`, `invoice_notes`. |
| Chargebee | Generic `events` table for webhooks. Activity reads from events + entity tables. |
| QuickBooks | Activity log is a multi-table query. No dedicated events table. |
| Velox (today) | Multi-source assembly across `invoices` columns + `stripe_webhook_events` + `email_outbox` + `dunning_events`. Same pattern as Lago/Recurly/QuickBooks. |

The pattern Velox already has is industry-canonical. A dedicated
`invoice_events` table would be more opinionated than canonical, and
is a multi-week refactor touching invoice / payment / email / dunning
packages without addressing any operator pain that exists today
(1 user, 0 design partners, ~5-event typical timeline).

## Decision

**Keep the current multi-source-assembly architecture.** Do not refactor
to a dedicated events table.

Document the canonical primitives the timeline will eventually need —
each as a separate, contained improvement to the existing endpoint —
along with **trigger conditions** that justify promoting them. Until a
trigger fires, ship nothing speculative.

## Canonical primitives (deferred until triggered)

### 1. Cursor-based incremental fetch
- **What**: `GET /v1/invoices/{id}/payment-timeline?since=<event_id>` returns only events after the cursor. Client appends.
- **Why**: every billing platform above does this. Saves repolling the same N rows.
- **Trigger**: timeline payloads exceed ~5KB on real customer invoices, OR multiple operators concurrently polling the same surface raise visible API load.
- **Cost**: ~50 lines; depends on adding a stable per-event ID to the response shape.

### 2. ETag / 304 on the timeline endpoint
- **What**: server returns `Last-Modified` / `ETag`; client sends `If-None-Match` on poll; server returns 304 when nothing has changed.
- **Why**: standard HTTP. Pairs with cursor pagination — saves payload entirely on no-op polls.
- **Trigger**: same as cursor.
- **Cost**: ~20 lines, but cleaner if shipped together with cursor.

### 3. `source` field on each event row — **SHIPPED**
- **Status**: shipped. Every timeline row carries `source` (`internal/invoice/handler.go` `timelineEvent`). The shipped enum is `lifecycle | stripe | dunning | email | credit_note` (not the `tax` originally sketched here).
- **What**: every wire-row carries a `source` field.
- **Why**: every comparable system tags events with origin. Lets the frontend render source-distinct icons / filter chips without parsing `event_type` strings.
- **Trigger**: an operator-driven need to filter/scan by source, OR adding refund / credit / one-off-charge events to the timeline (more sources = the source-field gap becomes annoying to ignore).
- **Cost**: ~30 lines in the existing handler switch; no migration.

### 4. Raw-payload disclosure endpoint
- **What**: `GET /v1/events/{event_id}/payload` returns the raw provider data (Stripe webhook JSON, email outbox payload, dunning event detail). Frontend exposes via expand-row.
- **Why**: every billing platform surfaces this for operator debugging. The raw Stripe error / failure_code is currently invisible from the timeline.
- **Trigger**: an operator complains they can't debug a webhook-ordering issue or a dunning-decline reason without querying the DB directly.
- **Cost**: ~80 lines (endpoint + schema + frontend disclosure UI).

### 5. Server-derived `simulated` flag — **SHIPPED**
- **Status**: shipped as `is_simulated` on each timeline row (`internal/invoice/handler.go`); the SPA reads the flag directly, no client-side clock comparison.
- **What**: server stamps `is_simulated: true` on events produced during a test-clock-advanced run.
- **Why**: cleaner separation — client renders, server owns semantics. Removes a leaky check that breaks if wall-clock and server-clock drift.
- **Trigger**: any time we touch the timeline backend for another reason; bundle this in.
- **Cost**: ~10 lines.

### 6. Generalized coalescer
- **What**: replace the ADR-020 hard-coded "lifecycle.paid + payment_intent.succeeded → one row" special case with a write-time or read-time rule engine that handles arbitrary coalesce pairs (e.g., "payment_failed + payment-failed-email-sent → one row").
- **Why**: each new coalesce today is bespoke. Matches Lago's approach.
- **Trigger**: needing 3+ coalesce rules. Currently we have 1.
- **Cost**: ~150 lines; touches the timeline assembly handler.

### 7. Dedicated `invoice_events` table (full event-sourced read model)
- **What**: every state change writes one row to a unified events table; timeline reads project from it.
- **Why**: ALSO valid as architecture, but NOT the dominant pattern for billing activity (only Stripe/Chargebee do this, both for webhook-delivery primary and timeline-secondary). Lago/Recurly/QuickBooks do not.
- **Trigger**: ≥3 of (cursor needed, payload-size pressure, 5+ sources feeding timeline, operator debugging pain, multi-tab load matters). High bar — likely never until post-design-partner.
- **Cost**: multi-week refactor across 4 packages. Migration. Backfill. Not justified pre-launch.

## What ships now

Pure-CSS density fix on the timeline row: `pb-4` → `pb-2`. ~30% tighter,
matches Stripe's row density. No backend or contract change.

## Consequences

- **Velox stays on the canonical pattern** (multi-source assembly) which the majority of billing platforms use.
- **No architectural debt accrues** from speculative-built infrastructure.
- **Future improvements are pre-designed** with trigger conditions, so we don't reinvent the design later or build the wrong thing under deadline pressure.
- **The trigger discipline is real**: each item lists a specific signal that, if observed, justifies the cost. Without a trigger, building any of these is over-engineering for an audience (multi-operator dashboards, debugging-heavy ops teams, scale) Velox doesn't have yet.

## Alternatives considered

- **Build the dedicated `invoice_events` table now as the "right architecture."** Rejected: not unambiguously canonical (only 2/5 surveyed do it), multi-week cost, no current pain it solves.
- **Build all canonical primitives now as a "Stage 1 timeline polish PR."** Rejected: each one has its own trigger that hasn't fired. Bundling them = speculative work. Captured as deferred so they're not lost.
- **Pretend the timeline is fine and don't document.** Rejected: the canonical primitives ARE real long-term improvements; without a written record, future-me would either (a) reinvent under pressure, or (b) build a bespoke variant. The ADR cost is one file.
