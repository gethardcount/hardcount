# A replica will not start: "REDIS_URL is required" or "Redis is invalid or unreachable"

In production, Velox uses Redis for rate limiting and refuses to start
without it. Back to the [runbook index](index.md).

## Symptom

A production replica refuses to boot and logs one of these messages:

- `REDIS_URL is required in production`
- `refusing to start: REDIS_URL is set but Redis is invalid or unreachable`

## Why

The general and hosted-invoice rate limiters fail closed without Redis. A
replica without Redis would boot green and answer 429 to its share of
traffic, including hosted-invoice pay pages, while `/health/ready` stays
ok. Refusing to start is the safer outcome.

## Check

- Is `REDIS_URL` set, and does it point at the right host?
- Is the network path to port 6379 open (security group, firewall)?
- Is the managed Redis up?

## Fix

Correct `REDIS_URL`, the network path, or the Redis service, then start the
replica again.

## Verify

The replica starts on the next attempt.

## Redis going down after boot

This is a different situation:

- General and hosted-invoice requests answer 429 (they fail closed).
- Usage ingest keeps working with no limit (it fails open).
- Sign-in, `/v1/auth`, keeps working and stays limited. Each replica
  counts sign-in requests on its own. The limit is 100 a minute per address
  by default, so with three replicas one address can make up to 300 a
  minute. The log shows `rate_limiter: redis error, counting in this
  process`.
- Restore Redis. No restart is needed.
