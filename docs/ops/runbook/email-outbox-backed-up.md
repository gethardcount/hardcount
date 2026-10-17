# Email outbox backed up

Velox queues every email in the `email_outbox` table, and a background job
sends them. This page is for when that queue grows. Back to the
[runbook index](index.md).

## Symptom

- The `email_outbox` table grows past 1000 rows in `status='pending'`
  (`velox_email_outbox_pending` > 1000).
- Customers report missing invoice emails.

## Why

- The SMTP provider is rate-limiting or down.
- The provider rejected the mail (authentication, sender domain, and so on).
- The dispatcher stopped (rare). See
  [A background job is stuck](background-job-stuck.md).

## Check

```sql
SELECT email_type, status, count(*), max(attempts) AS max_attempts
FROM email_outbox
WHERE status IN ('pending', 'failed')
GROUP BY email_type, status
ORDER BY count(*) DESC;

-- Most recent failure messages (the 10 most common)
SELECT email_type, last_error, count(*)
FROM email_outbox
WHERE status = 'failed' AND last_error IS NOT NULL
GROUP BY email_type, last_error
ORDER BY count(*) DESC
LIMIT 10;
```

A `last_error` that begins `email outbox: unknown email_type` means
different things by row status:

- On a **pending** row: a replica older than the code that wrote the row
  claimed it, during a rolling deploy. It retries on the backoff ramp, and
  a replica that knows the type delivers it.
- On a **failed** row: no replica knew the type for the whole ramp. The
  code that writes this email type shipped without its dispatcher case.

## Fix

1. Diagnose the SMTP provider from `last_error`.
2. Once the provider is healthy, the dispatcher drains the queue on its
   own. Pending rows are sent on their `next_attempt_at` schedule.
3. To recover faster, set `next_attempt_at` to now for every pending row:

   ```sql
   UPDATE email_outbox SET next_attempt_at = now()
   WHERE status = 'pending' AND attempts < 15;
   ```

4. Failed rows are dead-lettered: set aside after delivery gave up
   (15 attempts over about 72 hours). Find the root cause. Then either fix
   it and retry (`UPDATE ... SET status='pending', attempts=0`), or accept
   the loss and tell the affected customers.

## Verify

`velox_email_outbox_pending` falls back below 1000, and the first query
shows fewer pending rows on each run.
