# Webhook outbox backed up

Velox queues every webhook event in the `webhook_outbox` table. A
background job turns each row into an event and one delivery per matching endpoint,
before anything is sent. An endpoint that keeps failing does not grow it: see
[A webhook endpoint keeps failing](webhook-endpoint-failing.md). Back to the
[runbook index](index.md).

## Symptom

- `velox_webhook_outbox_oldest_pending_age_seconds{mode="live"}` is above
  1800. A row queued 30 minutes ago has no event yet.
- `velox_webhook_outbox_dead_lettered_total` rises. Rows were given up on
  after 15 tries over about 72 hours. Their events were never written, so
  no endpoint gets them.
- `velox_webhook_outbox_pending` is above 1000.

## Why

- **The `webhook_outbox` job is stopped.** See
  [A background job is stuck](background-job-stuck.md).
- **Writing the event and its deliveries fails.** The database refuses the
  write, for example on lost connections or a full disk. The error is in
  `last_error`, and the row is retried on a backoff ramp.

## Check

```sql
-- The queue, oldest first, with the last error
SELECT livemode, event_type, status, attempts, created_at, last_error
FROM webhook_outbox
WHERE status IN ('pending', 'failed')
ORDER BY created_at
LIMIT 20;
```

## Fix

1. If the job is stopped, follow
   [A background job is stuck](background-job-stuck.md).
2. Fix the cause in `last_error`. Pending rows then go out on their
   `next_attempt_at` schedule.
3. Send the failed rows again, as below.

### Send failed rows again

Narrow the `created_at` window to the incident:

```sql
-- status = 'failed' keeps this off rows that are being dispatched or were
-- dispatched. A dispatched row already has its event: dispatching it again
-- sends a second event with a new id, which receivers cannot skip.
UPDATE webhook_outbox
SET status = 'pending', attempts = 0, next_attempt_at = now()
WHERE status = 'failed'
  AND created_at > now() - interval '7 days';
```

Each row then becomes one event, sent to the endpoints that match it at
that moment.

## Verify

`velox_webhook_outbox_oldest_pending_age_seconds` falls back near 0, and
`velox_webhook_outbox_dead_lettered_total` stops rising.
