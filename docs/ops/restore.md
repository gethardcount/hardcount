# Restore Hardcount

This page brings Hardcount's database back from a backup, after data loss or a lost server. It is for whoever runs Hardcount. Setting up the backups, and testing that they restore, is on [Back up Hardcount](backup-considerations.md).

A restore takes the database back to the moment of the backup. Everything written after it is gone from Hardcount: usage events, invoices, payments, refunds, and changes made in the dashboard or through the API. Stripe still holds the payments. Hardcount must not charge again until it knows about them.

So Hardcount's background jobs stay paused until the checks on this page pass. A background job is a loop that one copy of Hardcount runs at a time, such as billing. If Hardcount started straight after a restore, its billing job could charge a customer again for an invoice paid after the backup.

## Before you restore

1. **Stop every copy of Hardcount, and keep them stopped until [check 4](#4-match-hardcount-with-stripe).** A copy that runs on the restored database can start billing and charging within seconds. On one VM, run `docker compose stop hardcount-api` in `/opt/hardcount/deploy/compose`. Elsewhere, scale every copy to zero.
2. **Restore into an empty database when you can.** The script replaces everything the backup holds, including the migration record, the list of database changes already made. It leaves alone a table the backup does not hold, such as one that a newer release created. Such a table causes one of two problems:
   - It refers to a table in the backup (a foreign key). The restore cannot replace that table, so it stops and changes nothing.
   - It refers to no table in the backup. The table stays, but the migration record goes back to the backup's version. The next upgrade runs the migration that made the table again, and can stop on `already exists`.
3. **On a new Postgres server, create the limited user first.** Users belong to the server, not to the database, so a backup cannot carry them. Without the user, the restore cannot work, so the script stops before it starts ([how this was found](../dev/records/restore-drill-first-run-2026-08-11.md)). The second line sets the timeout that [Postgres requirements](postgres-requirements.md#timeouts-on-the-limited-user) calls required.

   ```sql
   CREATE ROLE hardcount_app LOGIN PASSWORD '<your-password>';
   ALTER ROLE hardcount_app SET idle_in_transaction_session_timeout = '30s';
   ```

4. **Use tools that match the server.**
   - `pg_restore` must not be newer than the target server. The script stops if it is. Run a matching one in a container: `PG_RESTORE="docker run --rm -i --network host postgres:16-alpine pg_restore"`.
   - `psql` must be at least as recent as `pg_restore`. An older one stops at `invalid command \restrict`, and nothing changes.
   - The script skips the version check when `HARDCOUNT_RESTORE_TOOL_MISMATCH_OK=yes` is set, or when it cannot read the server's version.

## Restore

`scripts/restore.sh` restores the backup in one transaction: either all of it is restored, or nothing changes. Each restored table gets the access rights it has in the backup, even when the target database gives new tables more, as a new Compose install does. It needs `pg_restore` and `psql` on the machine that runs it.

1. Run it against the target database.

   ```bash
   DATABASE_URL="postgres://hardcount:PASSWORD@db-host:5432/hardcount?sslmode=require" \
     ./scripts/restore.sh /var/backups/hardcount/latest.dump
   ```

   **You see:** a warning box naming the target and the backup, then `Type 'yes' to proceed:`. Type `yes`. To skip the question in a script, set `HARDCOUNT_RESTORE_CONFIRM=yes`.

2. Wait for it to finish.

   **You see:** `All critical tables present.`, a "Restore Summary" block, and `Restore complete. Verify application behavior before resuming traffic.`

   Exit codes: `0` success, `1` a setting or the file is wrong, `2` the restore failed and was rolled back, `3` a critical table is missing after the restore.

   With a managed Postgres, restore the snapshot or the point in time with your provider's tool instead, into the database Hardcount will use. Then go on to step 3.

3. **Pause the background jobs.** The database calls a job a *role*. Run this in `psql` as the database owner, before anything starts Hardcount:

   ```sql
   SELECT leader_pause('billing',          'your-name', 'restore');
   SELECT leader_pause('dunning',          'your-name', 'restore');
   SELECT leader_pause('email_outbox',     'your-name', 'restore');
   SELECT leader_pause('webhook_outbox',   'your-name', 'restore');
   SELECT leader_pause('webhook_delivery', 'your-name', 'restore');
   ```

   **You see:** `t` five times. A job that was already paused in the backup returns an empty value instead. Then check all five:

   ```sql
   SELECT role, paused_by, pause_reason FROM leader_status ORDER BY role;
   ```

   **You see:** five rows, each with `your-name` and `restore`. Do not start Hardcount until you do.

   The pause is a row in the database, so it holds on every copy and through restarts, until [check 6](#6-resume-the-jobs-and-watch-billing-catch-up) ends it. These five are all the background jobs that charge a card or send an email or a webhook:

   | Job | What it does |
   |---|---|
   | `billing` | Builds invoices and charges cards |
   | `dunning` | Retries failed payments, and queues the emails that go with them |
   | `email_outbox` | Sends queued emails |
   | `webhook_outbox` | Sends queued webhooks |
   | `webhook_delivery` | Retries webhooks that failed |

   One more piece of work resumes when Hardcount starts: a test clock that was moving forward at the backup time. It bills test customers only, in Stripe's test mode.

## Checks after a restore

Run them in order. Hardcount stays stopped for checks 1 to 3. Run the SQL in `psql` as the database owner. The blocks that read or change account data begin with `SET app.bypass_rls = 'on';`. Without it, on a managed database whose admin user is not a superuser, they see no rows and change none, with no error, so a check looks passed ([why](runbook/index.md#before-you-start)).

### 1. The schema version matches

Run the `hardcount migrate status` of the release you will start, against the restored database.

**You see:** `state:        up to date, nothing to apply`. `behind` lists what `hardcount migrate` will do. `no migration record` means the backup was restored without its migration table: restore it again, with the table, into an empty database. A backup made by Velox, Hardcount's name before v0.9.0, cannot be restored into Hardcount ([Coming from Velox](../upgrades.md#coming-from-velox)).

The record itself, if you want to look:

```sql
SELECT max(version_id) FROM public.goose_db_version;
```

### 2. Row-Level Security is on

```sql
SELECT schemaname, tablename, rowsecurity
FROM pg_tables WHERE schemaname = 'public' AND rowsecurity = true
ORDER BY tablename;
```

Row-Level Security (RLS) is the Postgres feature that keeps each account's rows apart. The list must include every account-scoped table, such as `customers` and `invoices`. A missing table means its data is not kept apart.

### 3. The money tables look right

```sql
SET app.bypass_rls = 'on';
SELECT tenant_id, count(*) FROM invoices GROUP BY tenant_id;
SELECT tenant_id, count(*) FROM credit_notes GROUP BY tenant_id;
SELECT tenant_id, sum(total_amount_cents) FROM invoices
  WHERE status = 'paid' AND created_at >= now() - interval '90 days'
  GROUP BY tenant_id;
```

Compare the numbers with what you saw before the incident, for example in your metrics.

### 4. Match Hardcount with Stripe

The restored database does not know about the payments Stripe took after the backup. Billing's own check with Stripe, the reconciler, does not find them: it asks only about charges whose result Hardcount has not saved. This check finds each payment and records it in Hardcount, while the jobs are still paused.

1. **Note the backup time.** A dump's name holds the time it started, in UTC: `hardcount_20261019_030000.dump` started at 03:00:00 UTC on 19 October 2026. After a point-in-time restore, it is the time you restored to.
2. **Start Hardcount.** On one VM, run `docker compose up -d hardcount-api`.

   **You see:** `/health/ready` answers 200, and the `leader_status` query from [Restore](#restore), step 3, still shows five paused jobs. The 200 does not mean the jobs run.

   Hardcount now answers requests, and Stripe can deliver events to it. Do not use **Charge now** on an invoice until check 6.
3. **Have your app send usage events again.** Send every event it sent after the backup time, with the same `idempotency_key`. Hardcount counts each key once, so an event the backup holds is not counted twice. An event that is not sent again is not billed.
4. **List the payments Stripe took after the backup time.** Do this in each Stripe account your Hardcount accounts use. In the Stripe Dashboard, open **Payments** and filter by date and by the status **Succeeded**. Or use the Stripe CLI with that account's live secret key. `1792378800` is the backup time above, in Unix seconds (`date -u -d '2026-10-19 03:00:00' +%s` on Linux):

   ```bash
   stripe payment_intents search --api-key sk_live_… --limit 100 \
     --query "status:'succeeded' AND created>=1792378800"
   ```

   If the answer says `"has_more": true`, run it again with `--page` set to its `next_page`. Each payment Hardcount made names its invoice in its metadata, as `hardcount_invoice_id`. A payment without it was not made by Hardcount: skip it.
5. **Look up each payment's invoice in the restored database.**

   ```sql
   SET app.bypass_rls = 'on';
   SELECT id, status, payment_status FROM invoices WHERE id = 'hc_inv_…';
   ```

   | You see | Do |
   |---|---|
   | `paid` | Nothing |
   | Any other status | Resend the payment's event (step 6) |
   | No row | Note the payment's ID, amount and `hardcount_customer_id` for check 6. The invoice was made after the backup. |

6. **Resend each payment's `payment_intent.succeeded` event to Hardcount.** In the Stripe Dashboard, open the event from the **Events** tab in Workbench and click **Resend**. Or run `stripe events resend evt_… --webhook-endpoint we_… --api-key sk_live_…`, where `we_…` is Hardcount's endpoint in the **Webhooks** tab. Hardcount skips only the events its database holds, and the restored one holds none from after the backup.

   **You see:** the step 5 query shows `paid`.

   Stripe resends an event only within 15 days of it. For an older one, record the payment on the invoice instead ([Mark an invoice paid](runbook/manual-interventions.md#mark-an-invoice-paid-offline-payment-received)), with the payment's ID in the note.

### 5. Queued emails and webhooks

Hardcount queues outgoing emails and webhooks in two tables, `email_outbox` and `webhook_outbox`, and sends them later. After a restore, every `pending` row is sent when the jobs resume. If the backup is older than some sends, customers get those emails or webhooks twice. The payments recorded in check 4 queued their receipts and webhooks too.

The safe default: leave the rows `pending` and accept a few duplicates. Webhook receivers should already handle the same event twice.

If you are sure you want to skip them, mark them sent, and write down that you did:

```sql
SET app.bypass_rls = 'on';
UPDATE email_outbox SET status = 'dispatched', dispatched_at = now(),
  payload = payload - '{setup_url,update_url,password_reset_url,invite_url,public_token,pdf}'::text[]
  WHERE status = 'pending';
UPDATE webhook_outbox SET status = 'dispatched', dispatched_at = now() WHERE status = 'pending';
```

The `payload - '{…}'` part removes the links and the PDF from the email rows, so they are not kept for emails that were never sent.

### 6. Resume the jobs, and watch billing catch up

1. **Resume the jobs.**

   ```sql
   SELECT leader_unpause('billing');
   SELECT leader_unpause('dunning');
   SELECT leader_unpause('email_outbox');
   SELECT leader_unpause('webhook_outbox');
   SELECT leader_unpause('webhook_delivery');
   ```

   **You see:** `t` five times.

2. **Check that each job runs.**

   ```sql
   SELECT role, paused_at IS NOT NULL AS paused, last_tick_age_s FROM leader_status ORDER BY role;
   ```

   **You see:** `paused` is `f` on every row. A job runs at once if its last run in the backup is older than its interval, and otherwise when the interval ends. Its `last_tick_age_s` then drops to a few seconds. Billing's interval is in [When invoices are made](../api/timing-and-limits.md#when-invoices-are-made).

   Billing now bills every subscription that was due but not billed, and charges the invoices waiting for a charge. Dunning runs the payment retries that are due.

3. **Watch billing catch up.** With `API` set to your Hardcount address:

   ```bash
   curl -sS "$API/metrics" -H "Authorization: Bearer $METRICS_TOKEN" \
     | grep -E '^hardcount_billing_(due_subscriptions|oldest_due_age_seconds)\{mode="live"\}'
   ```

   **You see:** once the first billing run ends, `hardcount_billing_oldest_due_age_seconds` is below one billing interval, and `hardcount_billing_due_subscriptions` is back to its usual level, often 0. If they do not fall, see [A background job is stuck](runbook/background-job-stuck.md). A subscription whose billing fails is counted apart: see [Subscriptions whose billing is failing](runbook/billing-failing.md).

4. **Handle the payments that had no invoice.** For each payment you noted in check 4, look for the customer's new invoices:

   ```sql
   SET app.bypass_rls = 'on';
   SELECT id, status, payment_status, total_amount_cents, billing_period_start, billing_period_end
   FROM invoices WHERE customer_id = 'hc_cus_…' ORDER BY created_at DESC LIMIT 5;
   ```

   When the restore lost a subscription's invoice, billing builds it again and charges the card again. No setting stops this for one subscription. So:
   - **A new invoice for the same period is paid:** the customer paid twice. Refund the earlier payment, the one from your list, in the Stripe Dashboard. The new invoice then matches the one payment left. No invoice holds the refunded payment, so for up to three days Hardcount logs a warning each time Stripe resends that refund's event. That is expected.
   - **Its charge failed:** record the earlier payment on the new invoice instead ([Mark an invoice paid](runbook/manual-interventions.md#mark-an-invoice-paid-offline-payment-received)), with the payment's ID in the note.
   - **No new invoice for that period:** no subscription builds it again, so nothing is charged twice. Keep the payment's ID for your accounts.

**Next:** [Health checks, alerts and logs](runbook/monitoring.md).
