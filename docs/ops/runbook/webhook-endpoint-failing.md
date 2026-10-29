# A webhook endpoint keeps failing

Hardcount retries on the [schedule in Webhooks](../../webhooks.md#retries),
then marks the delivery failed. `webhook_deliveries` holds one row per
event and endpoint; each retry updates that row (`attempt_count`, the last
error, the next retry time). Back to the [runbook index](index.md).

## Symptom

- `hardcount_webhook_deliveries_total{status="failed"}` rises. It counts
  deliveries marked failed after their last retry, about 26.5 hours after
  the first attempt, for all endpoints together (it has no endpoint label).
- Earlier, from the first failed attempt: the WARN line
  `webhook delivery scheduled for retry`, which names the `endpoint_id`.
  The last failure logs `webhook delivery permanently failed` at ERROR.
- The dashboard shows failed deliveries and a falling success rate for one
  endpoint.

## Why

- The endpoint is down, or does not answer with a `2xx` within 10 seconds.
- The endpoint answers with a redirect (`3xx`). Hardcount does not follow
  redirects, so a redirect is a failed delivery.
- The HMAC signature does not match: the shared-secret signature on each
  delivery no longer verifies. The customer checks against an old secret.

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
