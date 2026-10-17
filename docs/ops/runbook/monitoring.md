# Health checks, alerts and logs

What to probe, what should page someone, and which log lines to search
during an incident. Back to the [runbook index](index.md).

## Health endpoints

| Endpoint | Purpose | Use for |
|---|---|---|
| `GET /health` | Liveness: the process is running | Kubernetes liveness probe |
| `GET /health/ready` | Readiness: 503 when the database ping fails or the process is shutting down, else 200. The body also reports whether the scheduler ran recently | Kubernetes readiness probe and load balancer health check |
| `GET /metrics` | Prometheus scrape. Send `METRICS_TOKEN` as a bearer token. In production with no token set it answers 401 | Metrics-collection job |

The scheduler is the background loop that drives billing. If it has not
ticked in 2× the configured interval, the `scheduler` check in the
`/health/ready` body starts with `degraded`, but the status code stays 200. Taking the replica
out of the load balancer would not restart the scheduler, so the readiness
probe does not catch a stalled scheduler. Alert on the scheduler and leader
metrics below instead, and see
[A background job is stuck](background-job-stuck.md).

When the database ping fails, the body says `database: error` and nothing
more. The cause is in the application log, in the
`readiness: database ping failed` line.

## Metrics to alert on

All metrics are exported under `/metrics`. The tables below are the
alerting tier: what should page someone, and what is only informational.

### Page (critical)

| Metric | Threshold | What it means |
|---|---|---|
| `velox_http_request_duration_seconds` p99 | > 5s for 5m | Server is unhealthy |
| `velox_billing_cycle_errors_total` | rate > 0.1/s for 5m | Billing cycles failing systematically. A low rate is not an all-clear: use the next row. See [Why the error rate falls](billing-failing.md#why-the-error-rate-falls) |
| `sum(velox_billing_failing_subscriptions{mode="live"})` | above a threshold you choose (suggested 20), or rising for 30m | One cause is failing many subscriptions: a bad deploy, or a tax provider that is set but not configured. Nothing parks before 3 days 17 hours after its first failure, so there is time to fix the cause. See [Billing failing](billing-failing.md) |
| `up{job="velox"}` | == 0 | Process down |
| Postgres connection errors (count) | > 10/min | Database connectivity broken |
| `time() - velox_scheduler_last_run_timestamp_seconds` | > 2× tick interval | Scheduler stalled on this replica. `/health/ready` shows it in its body but stays 200, so this alert is how you find out |
| `velox_leader_last_tick_age_seconds{role}` | > 3× the role's interval | No replica has finished a tick of this role. **On a busy billing day this trips too**: if `velox_billing_due_subscriptions` is falling, the tick is draining; do not restart it. See [A background job is stuck](background-job-stuck.md) |
| `increase(velox_leader_lease_lost_total[1h])` | > 0 | A tick lost its lease. Split by `reason`, and exclude `paused` (an operator action). What each reason means: [Lease-lost reasons](background-job-stuck.md#lease-lost-reasons) |

### Warn (Slack or email, not page)

| Metric | Threshold | What it means |
|---|---|---|
| `velox_payment_charges_total{result="failed"}` | rate spikes 5× baseline | Stripe issue or systematic decline |
| `velox_auto_charge_queued_invoices{mode}` | > 5000 sustained | The charge sweep visits every queued invoice each tick, so a large queue makes it slow. See the note below the table |
| `velox_billing_oldest_due_age_seconds{mode}` | > 2× billing interval | A subscription has been due that long, and its billing is not saved as failing. See [A subscription is due but not billed](background-job-stuck.md#a-subscription-is-due-but-not-billed) |
| `velox_billing_failing_subscriptions{state="parked",mode="live"}` | > 0 | A subscription has not been invoiced and nothing will retry it. Eight billing attempts failed. See [Billing failing](billing-failing.md) |
| `velox_dunning_runs_processed_total{outcome="failed"}` | rate > 0.5/s | Dunning machinery struggling |
| `velox_webhook_deliveries_total{status="failed"}` | sustained failure | Customer's webhook endpoint down or signature wrong |
| `velox_stripe_breaker_state` | == 2 (open; 0=closed, 1=half_open) | Stripe API circuit breaker tripped |
| `velox_email_outbox_pending` | > 1000 | Email dispatcher stuck or SMTP provider issue (−1 = the metric query itself failed) |
| `velox_webhook_outbox_pending` | > 1000 | Webhook dispatcher stuck (−1 = metric query failed) |
| `velox_creditnote_pending_tax_reversals` | any row older than 24h (alert on age, not presence) | An issued credit note whose tax reversal at the provider is still owed. See the note below the table |
| `velox_invoice_pending_tax_reversals` | any row older than 24h | A voided `stripe_tax` invoice whose committed tax transaction has no confirmed reversal. See the note below the table |
| `velox_creditnote_pending_issue_drafts` | sustained growth over days | Clawback drafts not issuing. Alert on growth or age, not presence. See [Clawback drafts](clawback-drafts-not-issuing.md) |
| `velox_parked_invoices{mode}` | > 0, sustained | An invoice whose charge attempt could not be identified with the provider. **It will not resolve on its own and needs a human.** See [Parked invoices](parked-invoices.md) |
| `velox_http_requests_total{method="POST",path="/v1/integrations/litellm/spend",status="503"}` | any increase | LiteLLM spend batches were refused for retry (usage store unreachable). See [LiteLLM spend gap](litellm-spend-gap.md) |
| `velox_auto_charge_retries_total{result="failed"}` | growing rapidly | Many invoices stuck in retry |
| `velox_audit_write_errors_total{outcome="row_lost"}` | rate > 0/s | **Audit evidence permanently lost.** Treat as a compliance incident. See [Audit write errors](audit-write-errors.md) |
| `velox_audit_write_errors_total{outcome="mutation_refused"}` | rate > 0/s | **Nothing is missing.** An availability problem, not an evidence problem. See [Audit write errors](audit-write-errors.md) |
| `velox_audit_uncovered_mutation_total{route}` | any increase | A route changed data and wrote no audit row. Should be **flat zero**. See [Mutation without an audit row](mutation-without-audit-row.md) |
| `increase(velox_usage_unpriced_quantity_total{mode="live"}[1d])` by `meter_id` | > 0 | An invoice was built without usage that matched no price. That usage is not billed unless you bill it. See [Usage with no price](usage-with-no-price.md) |

Notes on three rows:

- **Queued auto-charges.** Invoices with no card stay queued on purpose:
  adding a card charges them on the next visit. So a steady baseline is
  normal. Above 5,000 is the point at which Velox should stop re-visiting
  card-less invoices every tick (an amendment to ADR-116).
- **Pending tax reversals (credit notes).** The sweep re-drives each one
  every tick and keeps it until the provider accepts it.
  A row for a tick or two is normal. A day means the provider keeps
  rejecting it: handle as SEV-2 (see [Escalation](escalation.md)).
- **Pending tax reversals (invoices).** The invoice tax-reversal sweep
  re-drives it. As for credit notes: a day old is SEV-2.

### Info (dashboards, no alert)

- `velox_billing_cycles_total`: cycle throughput
- `velox_invoices_generated_total`: invoice volume
- `velox_usage_events_ingested_total`: usage ingest rate
- `velox_billing_cycle_duration_seconds`: cycle latency
- `velox_credit_operations_total`: credit ledger activity
- `velox_tax_outcome_total{outcome, reason}`: tax outcomes that were not a
  plain success (deferrals), by reason
- `velox_scheduled_cleanup_rows_total`: periodic cleanup activity

## Logs to search when paged

Velox writes structured JSON logs with `slog` (Go's standard structured
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

# Auto-charge failures
grep "auto-charge failed" log

# Webhook delivery failures
grep "webhook delivery failed" log

# Tax provider failures
grep "tax outcome" log | jq 'select(.outcome == "failed")'

# Scheduler last run
grep "billing cycle started" log | tail -5
```

Request IDs (the `Velox-Request-Id` header) appear in the logs and in error
responses. Paste a request ID into your log search to see the whole request.
