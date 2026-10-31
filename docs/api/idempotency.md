# Idempotency: retry without doing it twice

A request is idempotent when sending it twice has the same effect as
sending it once. Networks fail, so your code will sometimes not know whether
a request reached Hardcount. Hardcount gives you two keys that make a retry
safe:

| Requests | The key | Kept for |
|---|---|---|
| Usage events | The `idempotency_key` field in each event | Forever |
| Every other `POST`, `PUT` and `PATCH` under `/v1` | The `Idempotency-Key` header | 24 hours |

The rule for both is the same: **make the key once for each operation, and
send the same key on every retry of it.**

## Usage events: the `idempotency_key` field

Every usage event carries its own key. It names that one event, such as
`"acme-1-call-8812:input"`.

```json
{
  "event_name": "tokens",
  "external_customer_id": "acme-1",
  "quantity": 1000000,
  "idempotency_key": "acme-1-call-8812:input",
  "dimensions": {"model": "claude-sonnet-4.5", "token_type": "input"}
}
```

- **It is required.** An event without one, or with only spaces, is refused
  with `422` and `param: idempotency_key`.
- **It is at most 255 characters.** A longer one is refused with `422`.
- **It is unique for each account and mode (test or live), forever.** Only
  deleting a test clock frees keys: it deletes its customers' events.
- **A key that was used before keeps the first event.** Hardcount drops the
  new one without comparing the two. A single event gets `200`, the first
  event, and the header `Idempotent-Replayed: true`. A batch counts it in
  `deduplicated`, and its `results[i].status` is `deduplicated`.
- **A batch is stored all or nothing.** If one event is refused, none is
  stored, so you can resend the whole batch.

Never reuse a key for a different event: that usage is dropped. Do not build
keys from a batch position or a timestamp that can repeat. Build them from
something your app already counts once, such as a request ID.

The usage routes (`/v1/usage-events…` and `/v1/integrations/litellm…`)
ignore the `Idempotency-Key` header. The event's own key is their only
check.

## Other writes: the `Idempotency-Key` header

Send the header on every `POST`, `PUT` and `PATCH` you may retry, such as
creating a customer, granting credit or issuing a credit note:

```bash
IDEM_KEY=$(uuidgen)   # make it once; keep it with the job

curl -s -X POST "$HARDCOUNT/v1/customers" \
  -H "Authorization: Bearer $HARDCOUNT_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $IDEM_KEY" \
  -d '{"external_id": "cust_123", "display_name": "Acme Inc"}'
```

Send the same command again, with the same `$IDEM_KEY`. **You see:** the
first answer again, with `Idempotent-Replayed: true`. No second customer is
made.

Use a UUID, or another unique string of at most 255 characters. A key
belongs to one account and one mode. Hardcount matches a retry on its
method, its path and its body. The query string is not part of the match.

### What a retry with the same key gets

| The first request | A retry with the same key gets |
|---|---|
| Finished with any status except `409` or `422`, including `5xx` | The first status and body, plus `Idempotent-Replayed: true`. The request does not run again. |
| Finished with `409` or `422` | A new run. These answers are not stored, so the key is free again. |
| Is still running | Hardcount waits up to 5 seconds for it, then answers `409` `conflict_idempotency`. Retry later with the same key. |
| Finished, but the retry's method, path or body differs | `422` `idempotency_error`. Your code reused a key for a different request. |
| Started more than 5 minutes ago and never recorded an answer | `409` `conflict_idempotency_unresolved`. See below. |

A replay sends the first response's status and body only. The body's
`request_id` is the first request's. The `Hardcount-Request-Id` header is the
new request's. No other header of the first response is sent again.

**A stored `5xx` comes back on every retry with that key.** Hardcount stores
it on purpose: the first request may have charged a card or written data
before it failed, and running it again could do that twice. After a `5xx`:

1. Read the resource to see whether the change happened, for example with
   `GET /v1/customers?external_id=cust_123`.
2. If it did not happen, send the request again with a new key.

**Some answers come before the key is checked and are never stored.** A
retry with the same key runs normally after them:

- `401` (no key or a bad key) and `429` (rate limit).
- `403` for a browser request from another site.
- `503` `authentication_unavailable`.
- `503` `idempotency_unavailable`: Hardcount could not record the key, so it
  ran nothing. Retry with the same key; `Retry-After` is `1`.

### A key Hardcount could not finish

If the server stops, or its database fails, between running the request and
storing the answer, the key stays reserved. A retry then gets `409`
`conflict_idempotency`. Once 5 minutes have passed since the first request,
it gets `409` `conflict_idempotency_unresolved`. The request never runs
twice. Whether the first one made its change is unknown:

1. Read the resource to see whether the change happened.
2. If it did not, send the request again with a new key.

### How long a key lasts

Hardcount keeps a key for 24 hours after the first request. Do not reuse a
key for a new request, even after that. The expired key is deleted by a
cleanup that runs with each scheduled billing run, and until then a request
with it gets `409` `conflict_idempotency`.

### Routes that ignore the header

The header works on `POST`, `PUT` and `PATCH` requests to `/v1` routes that
take an API key. These routes ignore it, and run every time:

- The usage routes: `/v1/usage-events…` and `/v1/integrations/litellm…`. Use
  each event's `idempotency_key`.
- Sign-in, password reset and invites (`/v1/auth…`) and `/v1/bootstrap`.
- The public pages (`/v1/public/…`).
- The routes Stripe and Postmark call (`/v1/webhooks…`).

`GET` and `DELETE` requests do not use it.

## A retry loop

1. Make the key before the first attempt, and keep it with the job.
2. On a network error or a timeout, send the same request with the same key.
3. On `429`, wait the seconds in `Retry-After`, then send it with the same
   key. See [Rate limits](rate-limits.md).
4. On `409` `conflict_idempotency`, wait a few seconds and send it with the
   same key.
5. On a `5xx`, or `409` `conflict_idempotency_unresolved`, read the resource
   first. Send the request with a new key only if the change is not there.
6. On another `4xx`, do not retry: fix the request. Every code is in
   [Errors](errors.md).

Usage events are simpler. An event sent again with the same key is never
counted twice, so after any error, send the same events with the same keys.
The [two calls page](two-calls.md) has a working retry loop in curl, Node,
Python and Go.
