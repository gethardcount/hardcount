# Webhook outbox backed up or one endpoint failing

Velox queues every webhook event in the `webhook_outbox` table, and
background jobs send each one to your customers' endpoints. This page is
for when that queue grows or one endpoint keeps failing. Back to the
[runbook index](index.md).

## Symptom

- `webhook_outbox` grows past 1000 rows in `status='pending'`
  (`velox_webhook_outbox_pending` > 1000).
- Or one customer's endpoint keeps failing
  (`velox_webhook_deliveries_total{status="failed"}`).

The outbox holds one row per event, with no endpoint column. Each attempt
to deliver to one endpoint is a row in `webhook_deliveries`.

## Why

- The customer's webhook endpoint is down or rejecting requests.
- The HMAC signature does not match: the shared-secret signature on each
  delivery no longer verifies. The customer rotated the secret without
  telling Velox.

## Check

```sql
-- The queue: one row per event
SELECT event_type, status, count(*), max(attempts) AS max_attempts
FROM webhook_outbox
WHERE status IN ('pending', 'failed')
GROUP BY event_type, status;

-- Failures by endpoint (the delivery log, not the outbox)
SELECT webhook_endpoint_id, error_message, count(*)
FROM webhook_deliveries
WHERE status = 'failed' AND error_message IS NOT NULL
GROUP BY webhook_endpoint_id, error_message
ORDER BY count(*) DESC
LIMIT 20;
```

## Fix

1. Contact the customer and confirm the endpoint is up.
2. If the signature does not match:
   1. Rotate the signing secret in the dashboard
      (`Webhooks → Endpoint → Rotate secret`).
   2. The customer updates their side.
   3. Replay the failed events.
3. Only in a SEV-1, pause deliveries by deactivating the endpoint (see
   step 1 of [Escalation](escalation.md)). Events sent while it is inactive
   are not delivered to it.

## Verify

`velox_webhook_outbox_pending` falls back below 1000, and the second query
shows no new failures for the endpoint.
