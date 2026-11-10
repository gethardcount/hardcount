# Timing and limits

Every fixed time and size an API caller can run into, in one place. Each
value is the code's default; a row that names a setting can be changed by
the person who runs your server ([Settings](../settings.md)).

## When invoices are made

| What | Value | Setting |
|---|---|---|
| Wait after a billing period ends before its invoice is built (the grace period) | 1 hour | `INVOICE_GRACE_PERIOD`, from `0s` to `72h` |
| How often the scheduled billing run starts | Every hour. Every 5 minutes when `APP_ENV` is `local` or not set. | `APP_ENV` |

So an invoice appears 1 to 2 hours after its period ends, with the default
settings in production. Usage stamped inside the period is on the invoice if
it arrives before the invoice is built. Usage that arrives after that is
not; see [Late usage](../api-surface.md#late-usage).

The next billing run starts one interval after the previous run ended, so
the start time drifts by the length of each run.

A test clock bills with no grace period: moving the clock forward builds the
invoice at once. So does an immediate cancel, or `retry-billing`.

## When billing or a payment fails

A parked subscription is one Hardcount sets aside after eight failed billing
attempts, until someone retries it. Dunning is Hardcount retrying a failed
payment on a schedule, and emailing the customer.

| What | Value |
|---|---|
| Waits after a failed billing attempt for a subscription | 15 minutes, 1 hour, 4 hours, 12 hours, then 24 hours |
| Failed billing attempts before the subscription is parked | 8. `POST /v1/subscriptions/{id}/retry-billing` brings it back. |
| Payment retries of a new account's default dunning policy | On day 3, 7 and 14 after the failed payment. A policy takes 1 to 15 retries, each 1 to 30 days after the one before. |
| A card charge whose answer from Stripe was unclear | Hardcount checks Stripe's list of the customer's payments on each billing run. If no payment shows up 24 hours after the attempt, the invoice is charged again. |

Hardcount acts on these waits only during a billing run. So each wait ends
at the first billing run after its time is up.

## Requests

| What | Value |
|---|---|
| Request body | 1 MB (1,048,576 bytes). The LiteLLM callback, `POST /v1/integrations/litellm/spend`, takes 32 MB (33,554,432 bytes): a full LiteLLM batch is about 4 MB. |
| Stripe webhook body | 64 KB (65,536 bytes) |
| All request headers together | 8 KB. The server refuses them with `431` from about 12 KB, its 8 KB limit plus Go's 4 KB margin. |
| Events in one `POST /v1/usage-events/batch` | 1,000 |
| Rows in one list page | 100; 50 when `limit` is not sent ([pagination](pagination.md)) |
| Time limit of a request | 30 seconds, then `504` `timeout` ([errors](errors.md#504-gateway-timeout)) |
| Time the server has to send an answer | Until 35 seconds after the request arrives. A CSV export (`/v1/exports/…`) has a 5-minute time limit, and 5 minutes and 5 seconds to write. |
| Requests a minute or a second | See [Rate limits](rate-limits.md) |

## Keys and fields

| What | Value |
|---|---|
| A usage event's `idempotency_key` | At most 255 characters. Kept forever. |
| The `Idempotency-Key` header | Kept 24 hours from the first request ([idempotency](idempotency.md)) |
| A customer's `external_id` and `display_name` | At most 255 bytes each |
| A meter's `event_name` | At most 100 bytes: letters, digits, `_` and `-` |
| A usage event's `dimensions` | At most 16 labels, each value a string, number, boolean or `null` |
| A price's `dimension_match` | At most 16 labels; each key and value 1 to 100 bytes |
| A usage event's `quantity` | At most 12 decimal places, below 10^26, at most 100 characters as written |
| A usage event's `timestamp` | At most 5 minutes after the customer's current time. A customer on a test clock uses the clock's time. |
| One credit grant | At most 100,000,000 in the currency's smallest unit ($1,000,000) |
| Keeping an old API key working after a rotation | 0 to 604,800 seconds (7 days); 0 by default ([keys](authentication.md#manage-keys-with-the-api)) |

## Webhooks Hardcount sends

| What | Value |
|---|---|
| Time your endpoint has to answer | 10 seconds. Only a `2xx` counts as delivered. |
| Waits between delivery attempts | 1 minute, 5 minutes, 30 minutes, 2 hours, 24 hours, each plus up to 30 seconds |
| Delivery attempts | 6, over about 26 hours 36 minutes. Then the delivery is marked failed. |
| A delivery still waiting, whatever its attempts | Marked failed 50 hours 36 minutes after it was created, without another attempt |
| Tries to turn a queued event into deliveries | 15, over about 52 hours |
| Both secrets sign each message after a secret rotation | 72 hours |

More: [Retries](../webhooks.md#retries).

## Links, sessions and emails

| What | Value |
|---|---|
| Hosted invoice link | Never expires. `POST /v1/invoices/{id}/rotate-public-token` replaces it. |
| Stripe Checkout page opened from the hosted invoice's **Pay** | 1 hour |
| Payment-update link | 24 hours, and it works once |
| Dashboard session | 7 days from sign-in |
| Password-reset link | 1 hour |
| Team invite link | 7 days |
| Email delivery attempts | 15, over about 52 hours. Then the email is marked failed. |

Too many failed sign-ins make the dashboard wait before the next try. How
long: [Sign-in throttled](../ops/runbook/sign-in-throttled.md).

## Test clocks

| What | Value |
|---|---|
| One advance | Later than the clock's time, and at most 1 year ahead |
| The advance call | Answers at once with `status: "advancing"`. Poll `GET /v1/test-clocks/{id}` until `status` is `ready`. |
| Time the clock may take to bill what falls due | 10 minutes, then `status` is `internal_failure`. `POST /v1/test-clocks/{id}/retry-advance` tries again. |
