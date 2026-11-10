# Health checks, alerts and logs

What to probe, what should page someone, and which log lines to search
during an incident. Back to the [runbook index](index.md).

## Health endpoints

| Endpoint | Purpose | Use for |
|---|---|---|
| `GET /health` | Liveness: the process is running | Kubernetes liveness probe |
| `GET /health/ready` | Readiness: 503 when the database ping fails or the process is shutting down, else 200. The body also says how long ago this replica's billing loop last polled: whether the loop is alive here, not whether billing ran | Kubernetes readiness probe and load balancer health check |
| `GET /metrics` | Prometheus scrape. Send `METRICS_TOKEN` as a bearer token. In production with no token set it answers 401 | Metrics-collection job |

A loop that stopped polling shows `degraded` in the body but keeps the 200,
so alert on the scheduler metrics below.

When the database ping fails, the body says `database: error` and nothing
more. The cause is in the application log, in the
`readiness: database ping failed` line.

## Metrics to alert on

All metrics are exported under `/metrics`. The tables below are the
alerting tier: what should page someone, and what is only informational.

### Page (critical)

| Metric | Threshold | What it means |
|---|---|---|
| `hardcount_http_request_duration_seconds` p99 | > 5s for 5m | Server is unhealthy |
| `hardcount_billing_cycle_errors_total` | rate > 0.1/s for 5m | Billing cycles failing systematically. A low rate is not an all-clear: use the next row. See [Why the error rate falls](billing-failing.md#why-the-error-rate-falls) |
| `sum(hardcount_billing_failing_subscriptions{mode="live"})` | above a threshold you choose (suggested 20), or rising for 30m | One cause is failing many subscriptions: a bad deploy, or a tax provider that is set but not configured. No subscription parks (Hardcount stops retrying it) before 3 days 17 hours after its first failure, so there is time to fix the cause. See [Billing failing](billing-failing.md) |
| `up{job="hardcount"}` | == 0 | Process down |
| `/health/ready` answers 503 (your load balancer's or orchestrator's probe) | for 1m | The database ping failed on that replica, or it is shutting down. The cause is in the `readiness: database ping failed` log line |
| `time() - hardcount_scheduler_last_run_timestamp_seconds` | > 2× the billing interval (1 hour, or 5 minutes with `APP_ENV=local`) | This replica's billing loop stopped polling. Every replica's loop polls, the leader's and the others', so this says the loop is alive here, not that billing ran: the next row says that. `/health/ready` shows it in its body but stays 200, so this alert is how you find out |
| `hardcount_leader_last_tick_age_seconds{role}` | > max(3× the role's interval, 5 min) | No replica has finished a tick of this role (a role is a background job that one replica runs at a time). The 5-minute floor is for the roles that run every few seconds: a restart or a lease handover stops them for more than 3 intervals, and that is not a stall. **On a busy billing day this trips too**: if `hardcount_billing_due_subscriptions` is falling, the tick is draining; do not restart it. See [A background job is stuck](background-job-stuck.md) |
| `increase(hardcount_leader_lease_lost_total[1h])` | > 0 | A tick lost its lease. Split by `reason`, and exclude `paused` (an operator action). What each reason means: [Lease-lost reasons](background-job-stuck.md#lease-lost-reasons) |
| `hardcount_email_outbox_failing_emails{mode="live"}` | > 0 | See [Emails not going out](email-outbox-backed-up.md) |
| `hardcount_email_outbox_oldest_due_age_seconds{mode="live"}` | > 1800 | See [Emails not going out](email-outbox-backed-up.md) |
| `hardcount_webhook_outbox_oldest_pending_age_seconds{mode="live"}` | > 1800 | A webhook event queued 30 minutes ago has no deliveries yet: the webhook outbox job is stopped, or writing the event fails. A failing endpoint does not show here. See [Webhook outbox backed up](webhook-outbox-backed-up.md) |

### Warn (Slack or email, not page)

| Metric | Threshold | What it means |
|---|---|---|
| `rate(go_sql_wait_count_total{db_name="app"}[5m])` | > 0 sustained | Requests and background jobs on that replica wait for a free database connection: the pool is at `DB_MAX_OPEN_CONNS` and none is free. A short burst only delays those callers. When the waits go on, requests are slow and can time out. See [Connection pool settings](../postgres-requirements.md#connection-pool-settings) |
| `hardcount_payment_charges_total{result="failed"}` | rate spikes 5× baseline | Stripe issue or systematic decline |
| `hardcount_auto_charge_queued_invoices{mode}` | > 5000 sustained | The charge sweep visits every queued invoice each tick, so a large queue makes it slow. See the note below the table |
| `hardcount_billing_oldest_due_age_seconds{mode}` | > 2× billing interval | A subscription has been due that long, and it is not counted in `hardcount_billing_failing_subscriptions`. See [A subscription is due but not billed](background-job-stuck.md#a-subscription-is-due-but-not-billed) |
| `hardcount_billing_failing_subscriptions{state="parked",mode="live"}` | > 0 | A subscription has not been invoiced and nothing will retry it. Eight billing attempts failed. See [Billing failing](billing-failing.md) |
| `hardcount_dunning_runs_processed_total{outcome="failed"}` | rate > 0.5/s | Dunning (the automatic retries of failed payments) is struggling |
| `hardcount_webhook_deliveries_total{status="failed"}` | sustained failure | Customer's webhook endpoint down, redirecting, or signature wrong. Or Hardcount gave up a delivery it could not finish. See [A webhook endpoint keeps failing](webhook-endpoint-failing.md) |
| `hardcount_stripe_breaker_state` | == 2 (open; 0=closed, 1=half_open) | Stripe API circuit breaker tripped |
| `hardcount_email_outbox_pending` | > 1000 | Email dispatcher stuck or SMTP provider issue (−1 = the metric query itself failed) |
| `hardcount_webhook_outbox_pending` | > 1000 | Webhook dispatcher stuck (−1 = metric query failed) |
| `increase(hardcount_email_outbox_dead_lettered_total{mode="live"}[1h])` by `cause` | > 0 | See [Emails not going out](email-outbox-backed-up.md) |
| `increase(hardcount_webhook_outbox_dead_lettered_total{mode="live"}[1h])` | > 0 | Webhook events given up on before any delivery was written, so no endpoint gets them. See [Webhook outbox backed up](webhook-outbox-backed-up.md) |
| `min_over_time(hardcount_creditnote_pending_tax_reversals[24h])` | > 0 | The count of issued credit notes whose tax reversal at the provider is still owed has not been zero for a day. The gauge is a count, not an age. See the note below the table |
| `min_over_time(hardcount_invoice_pending_tax_reversals[24h])` | > 0 | The count of voided invoices taxed by Stripe Tax (`stripe_tax`) whose committed tax transaction has no confirmed reversal has not been zero for a day. See the note below the table |
| `hardcount_creditnote_pending_issue_drafts` | sustained growth over days | Clawback drafts are not issuing. A clawback is a credit note Hardcount issues when a paid, in-advance period is cut short. Alert on growth or age, not presence. See [Clawback drafts waiting](unclear-charge.md#clawback-drafts-waiting) |
| `hardcount_parked_invoices{disposition="open"}` | > 0 | A parked invoice that Hardcount has not finished on schedule. A parked invoice is one whose charge got an unclear answer from Stripe that named no payment. The gauge leaves out a parked invoice until 26 hours past its charge hold (the 5-minute claim a charge puts on the invoice), because Hardcount normally finishes one by about 25 hours. See [Parked invoices](unclear-charge.md#parked-no-paymentintent) |
| `hardcount_stripe_key_unusable_total{reason}` | increase > 0 | Charges are paused (not failed) until the key is fixed in Settings → Payments. `rejected`: Stripe refused the account's key (rolled, or a restricted key without a permission; adding the permission in Stripe also fixes it, and the next charge that goes through clears the Settings notice). `not_configured`: no key for that mode. The ERROR line "Stripe refused this tenant's API key" names the account and mode. Each tick tries again |
| `hardcount_stale_charge_hold_resolutions_total{result="cannot_ask"}` | increase > 0 | A lost result (a charge Hardcount sent but whose answer it never saved) that Hardcount cannot confirm with Stripe (disconnected, key rejected, customer gone). The invoice cannot be charged until it is fixed or written off. See [Lost results Hardcount cannot ask Stripe about](unclear-charge.md#interrupted-hardcount-cannot-ask-stripe) |
| `hardcount_http_requests_total{method="POST",path="/v1/integrations/litellm/spend",status="503"}` | any increase | LiteLLM spend batches were refused for retry (usage store unreachable). See [LiteLLM spend gap](litellm-spend-gap.md) |
| `hardcount_litellm_skipped_with_tokens_total{call_type}` | any increase | LiteLLM sent a call Hardcount does not bill (images, audio, batch results, ...) that carries tokens. The WARN "skipped a record that carries tokens" names the call type. Expected for `gpt-image-*` traffic; a new call type means LiteLLM sends usage Hardcount does not bill. See [Which LiteLLM calls bill](../../integrations/litellm.md#which-litellm-calls-bill) |
| `hardcount_litellm_cache_write_unsplit_total{reason}` | any increase | Claude cache writes billed as `input`, below the provider's cache-write price. `no_split` is expected for streamed calls; `no_count` means a `/v1/responses` call whose record has no cache-write count. See [LiteLLM: what lands in Hardcount](../../integrations/litellm.md#4-what-lands-in-hardcount) |
| `hardcount_auto_charge_retries_total{result="failed"}` | growing rapidly | Many invoices stuck in retry |
| `hardcount_audit_write_errors_total{outcome="row_lost"}` | rate > 0/s | **Audit evidence permanently lost.** Treat as a compliance incident. See [Audit log gaps](audit-log-gaps.md#audit-write-errors) |
| `hardcount_audit_write_errors_total{outcome="mutation_refused"}` | rate > 0/s | **Nothing is missing.** An availability problem, not an evidence problem. See [Audit log gaps](audit-log-gaps.md#audit-write-errors) |
| `hardcount_audit_uncovered_mutation_total{route}` | any increase | A route changed data and wrote no audit row. Should be **flat zero**. See [Audit log gaps](audit-log-gaps.md#a-change-with-no-audit-row) |
| `hardcount_auth_login_total{outcome="throttled"}` | rate > 0 for 15m | Someone keeps guessing dashboard passwords, or `TRUST_PROXY` is wrong. See [Sign-in throttled](sign-in-throttled.md) |
| `hardcount_auth_login_total{outcome="error"}` | any increase | Sign-in failed on the server side, usually the database. Everyone is refused while it lasts |
| `increase(hardcount_usage_unpriced_quantity_total{mode="live"}[1d])` by `meter_id` | > 0 | Some usage matched no price, and an invoice was built without it. That usage is not billed unless you bill it. See [Usage with no price](usage-with-no-price.md) |
| `increase(hardcount_usage_unbilled_total{reason="after_close"}[1d])` by `origin` | > 0 | Usage arrived after its period's invoice was built: stored, billed on no invoice unless you bill it. A WARN line names each event. See [Invoice timing and late usage](invoice-timing-late-usage.md) |
| `increase(hardcount_usage_unbilled_total{reason=~"no_price\|not_on_plan\|outside_subscription"}[1d])` by `reason` | > 0 | Usage was stored, and when it arrived Hardcount found a reason it is not billed: no price takes it (`no_price`), its meter is not on the plan (`not_on_plan`), or no subscription covered it (`outside_subscription`). The answer that wrote each event says which. See [Usage with no price](usage-with-no-price.md) |

Notes on three rows:

- **Queued auto-charges.** Invoices with no card stay queued on purpose:
  adding a card charges them on the next visit. So a steady baseline is
  normal.
- **Pending tax reversals (credit notes).** The sweep tries each one again
  every tick, until the provider accepts it.
  A row for a tick or two is normal. A count that stays above zero for a
  day can be one reversal the provider keeps rejecting, or a steady stream
  of new ones. The oldest tells which (in `psql`, see
  [Before you start](index.md#before-you-start)):
  `SET app.bypass_rls = 'on'; SELECT cn.tenant_id, cn.id, cn.issued_at FROM credit_notes cn WHERE cn.tax_reversal_pending AND cn.status = 'issued' AND cn.is_simulated = false AND EXISTS (SELECT 1 FROM invoices i WHERE i.id = cn.invoice_id AND i.tenant_id = cn.tenant_id AND COALESCE(i.tax_transaction_id, '') <> '') ORDER BY cn.issued_at LIMIT 5;`.
  One more than a day old: handle as SEV-2 (see [Escalation](escalation.md)).
- **Pending tax reversals (invoices).** The invoice tax-reversal sweep
  tries each one again. The oldest:
  `SET app.bypass_rls = 'on'; SELECT tenant_id, id, voided_at FROM invoices WHERE status = 'voided' AND tax_provider = 'stripe_tax' AND COALESCE(tax_transaction_id, '') <> '' AND tax_reversed_at IS NULL ORDER BY voided_at LIMIT 5;`.
  As for credit notes: one more than a day old is SEV-2. A common cause
  after a switch away from Stripe Tax: Stripe was disconnected while
  invoices it taxed were still open. Their reversals go to Stripe and fail
  until it is connected again.

### Info (dashboards, no alert)

- `hardcount_billing_cycles_total`: cycle throughput
- `hardcount_invoices_generated_total`: invoice volume
- `hardcount_usage_events_ingested_total`: usage ingest rate
- `hardcount_billing_cycle_duration_seconds`: cycle latency
- `hardcount_credit_operations_total`: credit ledger activity
- `hardcount_tax_outcome_total{outcome, reason}`: tax outcomes that were not a
  plain success (deferrals), by reason
- `hardcount_scheduled_cleanup_rows_total`: periodic cleanup activity

## Logs to search when paged

Hardcount writes structured JSON logs with `slog` (Go's standard structured
logger). Useful searches:

```bash
# Billing cycle errors
grep "billing cycle complete" log | jq 'select(.errors > 0)'

# One subscription's failed billing attempts (class: counted, transient, interrupted).
# recorded=false with class=counted carries not_recorded_reason: test_clock (a
# test-clock subscription never saves a failure), moved_on (another run finished
# the work or counted the attempt), write_error, or no_due_instant.
grep "bill subscription failed" log | jq '{subscription_id, class, recorded, not_recorded_reason, attempt, next_attempt_at, error}'

# Subscriptions that parked
grep "subscription billing parked" log

# Auto-charge sweep errors
grep "auto-charge retry error" log

# Webhook deliveries that gave up, and ones that will retry
grep "webhook delivery permanently failed" log
grep "webhook delivery scheduled for retry" log

# Webhook deliveries that failed inside Hardcount: an attempt that panicked
# (with its stack), and one given up while still pending after 50h36m
grep "webhook delivery attempt panicked" log
grep "webhook delivery given up" log

# Tax provider failures (the invoice waits for its tax retry)
grep "stripe tax failed, deferring invoice for retry" log

# The billing role's last ticks, on the replica that led them
grep "scheduler tick led" log | jq 'select(.worker == "billing")' | tail -5
```

Request IDs (the `Hardcount-Request-Id` header) appear in the logs and in error
responses. Paste a request ID into your log search to see the whole request.
