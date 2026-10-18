# ADR-122: LiteLLM usage for an unknown customer is held and released by its key, never re-timed

**Date:** 2026-10-17
**Status:** Accepted

## Summary

When the LiteLLM callback gets a token row whose customer or meter does not
exist, Velox keeps the row in `usage_held_events`, counts it as `held` in the
200 response, and shows it on the Usage page. An operator releases the rows
once the customer exists. A release writes each row to `usage_events` under
its original idempotency key and time, so a row is recorded at most once,
whatever order a release, a LiteLLM resend and a second release arrive in.
A released row bills only if its time falls in a billing period whose invoice
is not built yet. Velox never moves it onto a later invoice (ADR-120). There
is no automatic release when a customer is created.

## Context

Issue #1044. The callback answers 200 for a row whose customer or meter is
unknown, because a non-2xx makes LiteLLM resend the whole batch, so one bad
call would block the rest. The row was then dropped: LiteLLM reads any 2xx as
delivered and does not resend. The only traces were `errors[]` in a response
nobody reads and a count in the server log. Usage sent before the customer
was created, or with a `user` value that does not match an external ID, was
lost. `usage_events.customer_id` must name a real customer, so the row cannot
be stored there.

What other usage-billing engines do with an event for an unknown customer
(read 2026-10-17):

- **Orb** stores it: "If the `external_customer_id` is specified, the
  customer in Orb does not need to exist." "Events will be attributed to any
  future customers with the `external_customer_id` on subscription creation."
  ([ingest events](https://docs.withorb.com/api-reference/event/ingest-events))
- **Metronome** stores it and attributes it later by ingest alias: "if you
  first send usage keyed on `IngestAlias1` and later add this ingest alias to
  an existing customer, Metronome retroactively associates that usage to the
  correct customer."
  ([provision a customer](https://docs.metronome.com/guides/customers-billing/manage-customers/provision-a-customer.md))
- **Lago** saves the event without checking the subscription
  (`app/services/events/create_service.rb`), then matches subscriptions by
  external id and time: `started_at <= event.timestamp`
  (`app/services/events/post_process_service.rb`). An event from before the
  subscription started matches nothing.
- **OpenMeter / Kong** store events by subject and match customers when usage
  is queried. Kong: "Metering & Billing only invoices and meters events that
  are sent _after_ the subscription is created."
  ([Kong customers](https://developer.konghq.com/metering-and-billing/customer/))
- **Stripe** meter events report `meter_event_customer_not_found` after
  acceptance and do not keep the event: "Correct and resend invalid events for
  re-processing."
  ([recording usage](https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage-api))
- **Flexprice** answers 202 and the event does not show up; its fix is
  "Create the customer first"
  ([troubleshooting](https://docs.flexprice.io/docs/event-ingestion/troubleshooting.md)).
  This was Velox's behaviour too.

None of them silently drops the event while telling the sender it was
delivered. None moves late usage onto a later invoice; Lago and Kong attribute
only inside the subscription's own time window.

One fact shapes the value of a release. In Velox a subscription cannot start
in the past (`subscription.CreateInput` has only `StartNow`), and an invoice
adds up only the usage inside its own period. So rows that arrived before the
customer existed usually fall before any billing period, and a release stores
them without billing them. The value is: never lose the row, show it, and
release it once with an honest statement of what it will bill.

## Decision

1. **Hold.** Migration 0179 adds `usage_held_events` (row-level security,
   livemode from the session, `UNIQUE (tenant_id, livemode,
   idempotency_key)`). The LiteLLM handler calls `usage.Service.Hold` (through
   a consumer-defined `Holder`) when the customer or meter lookup returns
   not-found. The insert is `ON CONFLICT DO NOTHING`; a resend of a held row
   is counted as `deduplicated`. A hold that cannot be written is a 503, so
   LiteLLM resends. A payload with no `id` (no key) or no `user` stays an
   error and is not held.
2. **Show.** `GET /v1/usage-events/held` groups the held rows by external id,
   meter key and reason, with whether a matching customer and meter exist
   now. It lists the 200 external ids held most recently, each with all its
   groups, and returns `total_customers` and `total_rows` over every held
   row so the notice can say when the list is cut. The Usage page shows
   them in a notice with Release and Discard.
3. **Release once.** `POST /v1/usage-events/held/release` runs one
   transaction:
   - claim the rows with `UPDATE … SET status='released' WHERE status='held'
     … RETURNING`, so concurrent releases get disjoint rows;
   - insert each through `ingestOneTx`, the one INSERT every ingest path
     uses, with its original key and time and origin `backfill`;
   - record the event id, write one audit row, and commit. If `usage_events` already has the key (LiteLLM resent
   after the customer existed), the held row is marked released and nothing
   is written. Rows whose meter is still missing, or whose time is after the
   customer's current time (a test clock in the past), stay held
   (`still_held`). One release claims at most 5,000 rows; the rest it could
   have taken are counted apart (`more_to_release`), and another release
   takes them.
4. **Never re-time.** The release response counts released rows inside an
   active subscription's current period on a meter its plan prices
   (`in_open_period`), inside that period on a meter no plan on it prices
   (`not_on_plan`, never invoiced), and outside it
   (`outside_open_period`). The plan check reads each item's current plan,
   so a row from before a mid-period plan change is judged by the new plan. The second kind is not billed by a subscription
   invoice unless that period's invoice is not built yet; the operator bills
   it with a one-off invoice, as for any late usage (ADR-120).
5. **Discard.** `POST /v1/usage-events/held/discard` marks rows discarded.
   They are kept as a record and never billed. Audited.
6. **A resend after the customer exists.** The LiteLLM door ingests through
   `IngestResolvingHeld`: in one transaction it locks the held row with the
   same key (`SELECT … FOR UPDATE`, the lock release and discard take).
   A `discarded` row means nothing is written and the resend counts as
   deduplicated, so discard stays final. A `held` row is written and marked
   released, so a later discard cannot mark usage that is already billed.

## Why this design

The idempotency key is the one fact that makes "release later" safe. The held
row carries the exact key the mapper builds (`<id>:input`, `<id>:cache_read`,
`<id>:output`), and the release goes through the same INSERT and the same
unique constraint as live ingest. So the row lands at most once, and no new
dedup mechanism is needed.

Holding is scoped to the LiteLLM door because it is the only door that must
answer 200 for a row it cannot ingest. `POST /v1/usage-events` answers 422
synchronously, so its caller already knows.

## Alternatives considered

- **A. Only report (a counter and a log line).** Stripe's shape. Rejected:
  Stripe's sender sees the error and resends; LiteLLM never sees `errors[]`
  and never resends, so a report alone still loses the usage.
- **B. Release automatically when the customer is created.** Rejected for
  now: it makes the customer domain call usage (a new cross-domain edge),
  needs a fallback for a crash between create and release, and mostly adds
  rows that do not bill (see Context). It can be added later through a
  consumer-defined interface on `customer.Service`.
- **C. Bill released rows on the next invoice.** Rejected: it re-times usage
  into a period it did not happen in. No engine checked does this, and
  ADR-120 already decided against it for late usage.
- **D. Hold rows with no `user`.** Deferred: they need an "assign to a
  customer" step. They stay errors.

## Consequences

- An operator sees held usage on the Usage page without reading logs, and the
  LiteLLM response gains `held`.
- `usage_held_events` grows only with misrouted traffic; there is no retention
  job. Released and discarded rows stay as a record.
- A row with no payload time is stamped with the time Velox received it.
  Released to a customer on a test clock frozen before that time, it stays
  held until the clock passes it.
- Metric `velox_usage_held_events_total{reason}` counts new held rows.
