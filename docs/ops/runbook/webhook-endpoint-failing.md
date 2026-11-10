# A webhook endpoint keeps failing

Hardcount retries on the [schedule in Webhooks](../../webhooks.md#retries),
then marks the delivery failed. `webhook_deliveries` holds one row per
event and endpoint; each retry updates that row (`attempt_count`, the last
error, the next retry time). Back to the [runbook index](index.md).

## Symptom

- `hardcount_webhook_deliveries_total{status="failed"}` rises. It counts
  deliveries marked failed after their last retry, about 26 hours 36
  minutes after the first attempt. It also counts deliveries given up
  while still pending 50 hours 36 minutes after they were created. It
  covers all endpoints together (it has no endpoint label).
- Earlier, from the first failed attempt: the WARN line
  `webhook delivery scheduled for retry`, which names the `endpoint_id`.
  The last failure logs `webhook delivery permanently failed` at ERROR.
- The dashboard shows failed deliveries and a falling success rate for one
  endpoint.
- The ERROR line `webhook delivery attempt panicked` or
  `webhook delivery given up`. Both mean the failure is Hardcount's own
  ([below](#the-failure-is-hardcounts-own)).

## Why

- The endpoint is down, or does not answer with a `2xx` within 10 seconds.
- The endpoint answers with a redirect (`3xx`). Hardcount does not follow
  redirects, so a redirect is a failed delivery.
- The HMAC signature does not match: the shared-secret signature on each
  delivery no longer verifies. The customer checks against an old secret.
- Hardcount itself failed. The delivery's `error_message` says so
  ([below](#the-failure-is-hardcounts-own)).

## Check

```sql
SET app.bypass_rls = 'on';
-- Failures by endpoint
SELECT webhook_endpoint_id, error_message, count(*)
FROM webhook_deliveries
WHERE status = 'failed' AND error_message IS NOT NULL
GROUP BY webhook_endpoint_id, error_message
ORDER BY count(*) DESC
LIMIT 20;
```

## Fix

1. Contact the customer and confirm the endpoint is up. If it answers with
   a redirect, register the URL it redirects to.
2. If the signature does not match:
   1. Rotate the signing secret in the dashboard
      (`Webhooks → Endpoint → Rotate secret`).
   2. The customer updates their side.
3. Replay the failed events, from the dashboard or with
   `POST /v1/webhook_events/{id}/replay`. A replay keeps the event's `id`,
   so a receiver that already handled the event skips it.
4. Only in a SEV-1, pause deliveries by deactivating the endpoint (see
   step 1 of [Escalation](escalation.md)). Events sent while it is inactive
   are not delivered to it.

## Verify

The query shows no new failures for the endpoint, and its success rate in
the dashboard recovers.

## The failure is Hardcount's own

Two values of `error_message` mean the endpoint is not at fault.

**`internal error during the delivery attempt`.** An attempt panicked: a
bug in Hardcount. The attempt counts as one failed attempt, and the
delivery is retried on the schedule.

1. Find the ERROR line `webhook delivery attempt panicked` with the
   delivery's `delivery_id`. Its `stack` field shows where.
   - If no line has that `delivery_id`, search for
     `webhook delivery attempt panicked in a dial hook`. The panic was in
     the check Hardcount makes on an address before it connects, and that
     line has no `delivery_id`. Its `address` field is the host being
     connected, by name or by IP address, with the port. Match the line
     to the delivery by that and by the time.
2. Report the stack as a bug. A delivery that ends failed can be replayed
   once the bug is fixed.

**`gave up: still pending past the whole retry schedule, with no final
outcome recorded`.** The delivery was still pending 50 hours 36 minutes
after it was created: the whole retry schedule plus a day. The retry
worker marked it failed without sending it. When an earlier attempt had
failed, the text ends with `(last error: …)`.

1. Find the ERROR line `webhook delivery given up` with the delivery's
   `delivery_id`. Its `attempts` field counts the attempts made.
2. Find why it waited that long:
   - The `webhook_delivery` job was stopped, paused or far behind for more
     than a day: see [A background job is stuck](background-job-stuck.md).
   - Hardcount could not read the delivery's endpoint or event. Search the
     log for `get endpoint for retry` or `get event for retry` with the
     same `delivery_id`. The line's `error` field says why.
   - Hardcount could not save the result. Search for `mark not applied`
     with the same `delivery_id`.
3. Fix the cause, then replay the event as in step 3 of [Fix](#fix).

`hardcount-doctor` lists every delivery still pending 25 hours past that
age, so 75 hours 36 minutes after it was created (check
`webhook_delivery_pending_past_give_up`). The 25 hours cover a delivery
that is waiting for a retry. Hardcount gives a delivery up the next time
it picks the delivery up, and a retry set just before the age comes due
up to 24 hours later. A row there means the `webhook_delivery` job is
stopped, paused or far behind, or it cannot save the delivery.
