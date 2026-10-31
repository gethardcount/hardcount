# Rate limits

A rate limit caps how many requests Hardcount accepts in a period. A request
over the limit gets `429` and runs nothing. This page gives each limit, the
headers that report it, and how to retry.

## The limits

These are the defaults. The person who runs your Hardcount server can change
the first three (see [Change a limit](#change-a-limit)).

| Requests | Limit | Counted for each |
|---|---|---|
| The API and the dashboard: every `/v1` route not listed below | 1,000 a minute | API key. Dashboard users share one count per account and mode (test or live). |
| Usage: `/v1/usage-events…` and `/v1/integrations/litellm…` | 1,000 a second | API key, or account and mode for the dashboard |
| Sign-in, password reset and invites (`/v1/auth…`), and `/v1/bootstrap` | 100 a minute | Client IP address |
| The hosted invoice page, the payment-update page and the public cost dashboard (`/v1/public/…`) | 60 a minute | Client IP address |
| `POST /v1/customers/{customer_id}/payment-methods/send-setup-email` | 1 a minute, only on a server with Redis | Customer |

- Each API key has its own counts. A usage-only key on a gateway and a
  secret key on your server never use up each other's limit.
- One key has two separate counts: one for usage routes, one for every other
  route.
- `/health`, `/health/ready`, `/metrics` and the routes Stripe and Postmark
  call are not counted.

**The count refills steadily.** It is not reset at the start of each
minute. A full allowance lets a burst of up to the limit through at once.
After that, one request's allowance comes back every period ÷ limit: every
60 ms at 1,000 a minute, every second at 60 a minute.

**A request with a bad key is refused before it is counted.** It gets `401`
(see [Authentication](authentication.md#when-a-key-is-refused)).

## The headers

Every counted response carries three headers:

| Header | Value |
|---|---|
| `X-RateLimit-Limit` | The limit, such as `1000` |
| `X-RateLimit-Remaining` | Requests you can send now |
| `X-RateLimit-Reset` | When your full allowance is back, in Unix seconds |

A `429` also carries `Retry-After`: the number of seconds until your next
request can pass, at least 1.

## The 429 answer

```json
{
  "error": {
    "type": "rate_limit_error",
    "code": "rate_limited",
    "message": "too many requests — please retry after the rate limit resets",
    "request_id": "req_…"
  }
}
```

The setup-link email answers `cooldown_active` instead, with the message `a
setup link was sent to this customer recently — wait before sending again`.

Sign-in has a second, separate guard: after failed sign-ins it answers `429`
with code `login_throttled`. See [Errors](errors.md#429-too-many-requests).

## Retry after a 429

1. Wait the number of seconds in `Retry-After`.
2. Send the same request again, with the same `Idempotency-Key` header or the
   same usage `idempotency_key`.

A `429` ran nothing and stored nothing, so the same key is safe to send
again. See [Idempotency](idempotency.md).

To stay under the limit:

- Give each integration its own API key.
- Send usage in batches: `POST /v1/usage-events/batch` takes up to 1,000
  events in one request.
- Cache answers you poll, such as the live credit balance.

The [two calls page](two-calls.md) shows a client that waits for
`Retry-After`, in four languages.

## When Redis is down

Hardcount counts requests in Redis. A production server
(`APP_ENV=production`) refuses to start without a Redis it can reach. If Redis
goes down after start-up, each limit acts differently:

| Requests | In production | Elsewhere |
|---|---|---|
| The API and the dashboard, `/v1/bootstrap`, and the public pages | Every request gets `429`, with `Retry-After: 60`, until Redis is back | Not limited |
| Usage, and the setup-link email | Not limited | Not limited |
| Sign-in, password reset and invites | Counted by each server process on its own | Counted by each server process on its own |

Without Redis, a server with several copies (replicas) lets one address sign
in up to the limit on each copy. More:
[Redis going down after boot](../ops/runbook/replica-will-not-start.md#redis-going-down-after-boot).

## Change a limit

The person who runs the server sets these. Each must be a whole number of 1
or more; any other value stops start-up.

| Setting | Default | Limit |
|---|---|---|
| `HARDCOUNT_RATE_LIMIT_GENERAL_PER_MIN` | `1000` | The API and the dashboard, a minute |
| `HARDCOUNT_RATE_LIMIT_INGEST_PER_SEC` | `1000` | Usage, a second |
| `HARDCOUNT_RATE_LIMIT_AUTH_PER_MIN` | `100` | Sign-in, password reset, invites and bootstrap, a minute |

The public-page limit and the setup-link limit cannot be changed. Behind a
load balancer, set `TRUST_PROXY`, or every client shares the load balancer's
address and its per-address limits. More: [Settings](../settings.md).
