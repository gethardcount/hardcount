# Usage events

A usage event is one record that says "this customer used this much of
this thing". Your app sends one each time a customer uses something you
charge for, such as AI tokens or API calls. This page covers what an event
holds, how Hardcount counts it exactly once, and what happens to usage that
arrives late.

## The event

You send events to `POST /v1/usage-events`, one at a time, or up to 1,000
in one call to `POST /v1/usage-events/batch` (a JSON array). A LiteLLM proxy
can send them for you: see [LiteLLM integration](../integrations/litellm.md).

```json
{
  "event_name": "tokens",
  "external_customer_id": "acme-1",
  "quantity": "400000",
  "idempotency_key": "acme-1-call-1:input",
  "timestamp": "2026-06-30T23:50:00Z",
  "dimensions": {"model": "claude-sonnet-4.5", "token_type": "input"}
}
```

| Field | Required | What it is |
|---|---|---|
| `event_name` | Yes | Which meter counts the event: the meter's `event_name`, such as `tokens`. |
| `external_customer_id` | Yes | Whose usage it is: the customer's `external_id`, your own ID for them. |
| `quantity` | Yes | How much. A JSON number (`5`) or a string holding one (`"5.5"`), with at most 12 decimal places. `0` is allowed. A negative value corrects earlier usage. |
| `idempotency_key` | Yes | A unique ID for this one event, up to 255 characters. See [Counted once](#counted-once). |
| `timestamp` | No | When the usage happened, in RFC 3339 (`2026-06-30T23:50:00Z`). Without it, Hardcount uses the time it arrives. |
| `dimensions` | No | Labels, such as the AI model and the kind of token. Prices pick events by them. Up to 16 keys; each value is text, a number, true/false or null. |

A customer or meter that does not exist is refused with `422`, and nothing
is stored. A LiteLLM proxy's usage for an unknown customer is held instead,
so you can release it later.

A `timestamp` may not be more than 5 minutes ahead of the customer's
current time. For a customer on a [test clock](test-clocks.md), the current
time is the clock's time, and an event without a `timestamp` gets the
clock's time. There is no limit into the past, but see
[Late usage](#late-usage).

More: every field rule and error is in the
[API reference](../api-surface.md#sending-usage-safely).

## Counted once

Networks fail, so your app will sometimes send an event again without
knowing if the first one arrived. The idempotency key makes that safe.

- **Every event needs one.** An event without a key is refused with `422`.
- **Choose the key once, and reuse it on every retry.** Build it from
  something stable, such as your request ID plus the token type. A new key
  on each attempt makes each attempt a new event.
- **A key counts once, forever.** Keys are unique for each account and mode
  (test or live). When a key arrives again, Hardcount keeps the first event
  and drops the new one without comparing them. A single event answers
  `200` with the first event and the header `Idempotent-Replayed: true`. A
  batch counts it in `deduplicated`.
- **Never reuse a key for different usage.** That usage is lost. So do not
  build keys from a batch position or a timestamp that can repeat.

A batch is stored whole or not at all. So when a batch fails, send the
same batch again: events already stored come back as `deduplicated`.

The usage routes ignore the `Idempotency-Key` header that other write
routes take. The key in each event is their only one.

## Which period an event counts in

An event counts toward the customer's billing period that its `timestamp`
falls in, not the period in which it arrives. A period includes its start
and not its end: June's period runs from June 1 00:00 up to July 1 00:00
([billing periods](subscriptions.md#billing-periods)).

The answer that stored the event can carry a `billing_status`: what
Hardcount found when the event arrived.

| `billing_status` | Meaning |
|---|---|
| `after_close` | That period is already invoiced. The event is stored but on no invoice. |
| `not_on_plan` | The customer's plan has no price for this meter. |
| `no_price` | The plan prices the meter, but no price takes the event's dimensions. Usually a model with no price. |
| `outside_subscription` | No subscription of the customer bills that time, for example before it started or in a free trial. |

No `billing_status` means Hardcount found no problem then. It is not a
promise: the invoice is the final answer.

## Late usage

Usage often reaches Hardcount a little after it happened. So Hardcount
waits before it makes a period's invoice. The wait is the invoice grace
period, `INVOICE_GRACE_PERIOD` on your server: 1 hour by default, and
anything from 0 to 72 hours.

When the grace period has passed, the next billing run makes the invoice.
The billing run is hourly (every 5 minutes when `APP_ENV` is `local` or
not set). So by default the invoice is made 1 to 2 hours after the period
ends.

Once the invoice is made, it does not change. An event for that period
that arrives later is late usage: it is stored, answered with
`billing_status: "after_close"`, and billed on no invoice, now or later.
To charge for it, make a one-off invoice with the amount.

A few actions close an ended period at once, without the grace period:

- an immediate cancel;
- a plan, quantity or item change;
- a pause or resume of collection;
- a scheduled cancel, or its removal;
- `retry-billing`;
- moving a test clock.

More: [Invoice timing and late usage](../ops/runbook/invoice-timing-late-usage.md).

## Worked example: four events, one invoice line

A customer's June period runs from June 1 00:00 to July 1 00:00, UTC. The
plan prices Claude Sonnet 4.5 input tokens at $3.00 per million, which is
`0.0003` cents per token. The grace period is 1 hour, and the billing run
is hourly. Your app sends four events:

| Event | `timestamp` | Arrives | Key | Quantity |
|---|---|---|---|---|
| 1 | June 30 22:10 | June 30 22:10 | `call-7:input` | 400,000 |
| 2 | June 30 23:50 | July 1 00:40 | `call-8:input` | 350,000 |
| 3 | June 30 23:50 | July 1 00:41 | `call-8:input` | 350,000 |
| 4 | June 30 23:55 | July 1 02:30 | `call-9:input` | 250,000 |

1. Event 1 counts in June.
2. Event 2 is 40 minutes late, inside the grace period. It counts in June.
3. Event 3 is a retry of event 2, with the same key. It answers `200` with
   `Idempotent-Replayed: true`, and adds nothing.
4. The June invoice is made at the first billing run after 01:00, so
   between 01:00 and 02:00.
5. Event 4 arrives after that. It is stored with
   `billing_status: "after_close"` and is on no invoice.

The June invoice has one line for these events:

- quantity: 400,000 + 350,000 = 750,000 tokens;
- amount: 750,000 × 0.0003 cents = 225 cents, so $2.25.

Event 4 would have added 250,000 × 0.0003 = 75 cents. To charge it, add a
$0.75 line to a one-off invoice.

**Next:** how a meter adds events up and a price turns the total into
money: [Meters and prices](meters-and-prices.md).
