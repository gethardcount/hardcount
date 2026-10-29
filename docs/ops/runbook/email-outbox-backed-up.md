# Emails not going out

Hardcount queues every email in the `email_outbox` table, and a background job
sends them through your SMTP relay. Back to the [runbook index](index.md).

## Symptom

- `hardcount_email_outbox_failing_emails{mode="live"}` is above 0. These emails
  were tried 3 or more times and queued over 30 minutes ago, and they keep
  failing.
- `hardcount_email_outbox_oldest_due_age_seconds{mode="live"}` is above 1800.
  An email has been due for 30 minutes and no replica has sent it.
- `hardcount_email_outbox_dead_lettered_total` rises. Emails were dead-lettered:
  set aside as `failed` and never tried again on their own.
- `hardcount_email_outbox_pending` is above 1000.
- Customers report missing invoice emails.

## Why

- **The SMTP relay refuses the mail.** The emails keep failing. The relay's
  reply is in `last_error`. Common ones:
  - `554 Message rejected: Email address is not verified` (AWS SES): the
    sender is not verified with the relay. In the SES sandbox the
    recipient must be verified too.
  - `550 The from address does not match a verified Sender Identity`
    (SendGrid): the sender is not verified with the relay.
  - `535`: the relay rejected the SMTP username or password, often after a
    credential rotation.
  - `454`: the relay's sending quota or rate is used up, as in the SES
    sandbox.
  - `email: SMTP not configured (SMTP_HOST unset)`: Hardcount has no relay.

  Hardcount retries these for about 52 hours (15 tries), then dead-letters them
  with `cause="max_attempts"`.
- **The address can never get mail.** It bounced, it is suppressed after an
  earlier bounce, or it is not a valid address. These are dead-lettered at
  once, with `cause="permanent"`. A bounce is the relay's answer to the
  recipient address: 550 to 553 without a `5.7.x` status, or any 5xx with
  a `5.1.x` or `5.2.x` status. A refusal of the sender or of the message
  is retried, whatever its code.
- **The sending job is stopped or behind.** Due emails wait, and the oldest
  due age grows. One replica sends about 20 to 30 emails a minute, so a
  billing day that queues many invoice emails at once takes a while to
  drain. If the age grows and no emails are sent at all, see
  [A background job is stuck](background-job-stuck.md) for the
  `email_outbox` role.

## Check

```sql
SET app.bypass_rls = 'on';
-- Emails that keep failing, and the relay's reply
SELECT livemode, email_type, attempts, created_at, last_error
FROM email_outbox
WHERE status = 'pending' AND attempts >= 3
  AND created_at < now() - interval '30 minutes'
ORDER BY created_at
LIMIT 20;

-- The queue by type and status
SELECT email_type, status, count(*), max(attempts) AS max_attempts
FROM email_outbox
WHERE status IN ('pending', 'failed')
GROUP BY email_type, status
ORDER BY count(*) DESC;

-- Most common reasons for dead-lettered emails
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

A `last_error` that begins `email: recipient is not a single valid email
address` is not a provider problem. The row holds an address saved by
v0.8.2 or earlier, before Hardcount refused names, quotes and angle brackets,
so it failed at once with
no retry. Retrying the row sends the same address and fails again. Fix the
customer's email or CC list (or the member's email) and send again.

## Fix

1. Fix the cause in `last_error`. Verify the sender with the relay, correct
   `SMTP_USERNAME` and `SMTP_PASSWORD`, raise the relay's quota, or set
   `SMTP_HOST`. Hardcount reads the `SMTP_*` settings at start-up, so restart it
   after changing them.
2. Once the relay accepts mail, run step 3. Otherwise each pending email
   waits for its next try, up to 12 hours away, and
   `hardcount_email_outbox_failing_emails` keeps paging until then.
3. Set `next_attempt_at` to now for every pending row:

   ```sql
   SET app.bypass_rls = 'on';
   UPDATE email_outbox SET next_attempt_at = now()
   WHERE status = 'pending' AND attempts < 15;
   ```

4. Send dead-lettered emails again. Narrow the `created_at` window to the
   incident:

   ```sql
   SET app.bypass_rls = 'on';
   -- status = 'failed' keeps this off emails that are being sent or were sent.
   -- attempts >= 15 keeps it to emails that ran out of tries.
   UPDATE email_outbox
   SET status = 'pending', attempts = 0, next_attempt_at = now()
   WHERE status = 'failed' AND attempts >= 15
     AND email_type NOT IN ('password_reset', 'payment_setup_link', 'payment_setup_request')
     AND created_at > now() - interval '7 days';
   ```

   - A failed row keeps its links and PDF, so it can be sent again.
   - Emails that failed at once are left out: a bounce, or a suppressed or
     invalid address. A requeued email goes to the address saved in it, so
     it would fail again. Fix the customer's email, then send the email
     again from the dashboard.
   - Password resets and payment setup emails are left out: their links
     expired before the emails ran out of tries (a reset link after 1 hour,
     a setup link after 24 hours). The user asks for a new reset with
     **Forgot password?**; send a new setup link from the customer's or the
     invoice's page.
   - A payment reminder whose invoice was paid since is skipped, not sent.
   - To accept the loss instead, tell the affected customers.

## Verify

`hardcount_email_outbox_failing_emails` falls to 0 as the pending emails are
sent,
`hardcount_email_outbox_oldest_due_age_seconds` stays near 0, and
`hardcount_email_outbox_dead_lettered_total` stops rising.
