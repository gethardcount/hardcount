# A webhook endpoint keeps failing

Velox sends each webhook event to every matching endpoint your customers
registered. Each attempt to deliver it to one endpoint is a row in
`webhook_deliveries`. Velox makes six attempts over about 26.5 hours, then
marks the delivery failed ([Retries](../../webhooks.md#retries)). This page
is for when one endpoint keeps failing. Back to the
[runbook index](index.md).

## Symptom

- `velox_webhook_deliveries_total{status="failed"}` rises. It counts
  deliveries marked failed after their last retry.
- The dashboard shows failed deliveries and a falling success rate for one
  endpoint.

`webhook_outbox` does not grow when an endpoint fails: its rows are marked
dispatched before any request is sent.

## Why

- The endpoint is down, or does not answer with a `2xx` within 10 seconds.
- The endpoint answers with a redirect (`3xx`). Velox does not follow
  redirects, so a redirect is a failed delivery.
- The HMAC signature does not match: the shared-secret signature on each
  delivery no longer verifies. The customer checks against an old secret.

## Check

```sql
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
