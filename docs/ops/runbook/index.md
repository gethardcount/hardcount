# Operations runbook

What to do when Velox is in trouble. This runbook is for whoever runs a
Velox deployment, often during an incident. Find what you see in the table
below and open its page. Each page follows the same order: **Symptom**,
**Why**, **Check** (commands), **Fix**, and **Verify**.

It covers failures specific to Velox. General Postgres, Kubernetes and
Stripe troubleshooting is out of scope.

## Before you start

Most **Check** steps call the Velox API or the database. Set these once:

```bash
export API=https://billing.example.com   # your Velox address
export KEY=vlx_secret_live_...           # a secret API key for the tenant
```

SQL steps run in `psql` as the database owner. Under the app's
row-level-security role you see only one tenant's rows, or none.

## Words on these pages

- **Tenant**: one account's data. Many tenants share one database.
- **Replica**: one running copy of the Velox server.
- **Tick**: one run of a background job, such as one billing pass.
- **Lease**: a short booking, in the database, that lets one replica run
  one tick of a job. Like a meeting-room booking, it expires unless it is
  renewed.
- **Fence token**: a number on the lease that goes up each time a replica
  takes it. Work checks the number, so a replica whose lease ran out
  cannot claim anything.
- **Outbox**: a table of emails or webhooks waiting to be sent.
- **Parked**: set aside until something changes. An invoice is parked
  when Stripe's answer to its charge was unclear and named no payment; Velox
  finds out from Stripe within a day. A subscription is parked when billing
  it failed every time it was tried, and waits for a person.
- **Wait ladder**: the growing waits between retries of a failed billing
  attempt: 15 minutes, 1 hour, 4 hours, 12 hours, then 24 hours three
  times.
- **PI**: a Stripe PaymentIntent, the Stripe object for one card charge.

## What do you see?

| If you see | Open |
|---|---|
| The `scheduler` check in `/health/ready` starts with `degraded`, no invoices are being generated, or a background job looks stuck | [A background job is stuck](background-job-stuck.md) |
| `velox_email_outbox_failing_emails` or `velox_email_outbox_oldest_due_age_seconds` firing, emails dead-lettered, or customers report missing invoice emails | [Emails not going out](email-outbox-backed-up.md) |
| `velox_webhook_outbox_oldest_pending_age_seconds` firing, or `webhook_outbox` rows dead-lettered | [Webhook outbox backed up](webhook-outbox-backed-up.md) |
| One customer's webhook endpoint keeps failing (`velox_webhook_deliveries_total{status="failed"}`) | [A webhook endpoint keeps failing](webhook-endpoint-failing.md) |
| `velox_stripe_breaker_state == 2`, or dunning retries skipped | [Stripe circuit breaker open](stripe-circuit-breaker-open.md) |
| Invoices stuck with an unconfirmed payment for hours | [Payments stuck as unknown](payment-status-unknown.md) |
| A test clock stuck on the Advancing badge | [Test-clock advance hung](test-clock-advance-hung.md) |
| A tenant sees another tenant's data | [Data leak between tenants suspected](rls-leak-suspected.md) |
| LiteLLM spend 503s, or Velox spend short of LiteLLM's | [LiteLLM spend gap after an outage](litellm-spend-gap.md) |
| A replica will not boot and names Redis | [A replica will not start](replica-will-not-start.md) |
| Someone cannot sign in: "Too many sign-in attempts", or `velox_auth_login_total{outcome="throttled"}` rising | [Sign-in says "Too many sign-in attempts"](sign-in-throttled.md) |
| `velox migrate` or the start-up stops with `migration … failed`, `schema_migrations`, `goose_db_version` or `another Velox process is migrating` | [A database upgrade stopped](migration-failed-midway.md) |
| An upgrade stops with `waited 5s for a table lock` | [A database upgrade waited for a table lock](migration-lock-timeout.md) |
| `velox_stripe_key_unusable_total` rising, or "Stripe refused this tenant's API key" in the logs: charges paused until the key is fixed in Settings → Payments | [Health checks, alerts and logs](monitoring.md) |
| `velox_parked_invoices{disposition="open"}` above zero | [Parked invoices](parked-invoices.md) |
| "stale charge check: can't confirm an earlier charge with Stripe" in the logs, or `velox_stale_charge_hold_resolutions_total{result="cannot_ask"}` rising | [Interrupted charges](interrupted-charges.md) |
| An invoice says "after a lost dispute", a `payment.dispute_opened` or `payment.dispute_closed` webhook, or Stripe emails about a dispute | [Card disputes](card-disputes.md) |
| `velox_creditnote_pending_issue_drafts` growing over days | [Clawback drafts not issuing](clawback-drafts-not-issuing.md) |
| `velox_audit_write_errors_total` rising | [Audit write errors](audit-write-errors.md) |
| `velox_audit_uncovered_mutation_total` rising | [Mutation without an audit row](mutation-without-audit-row.md) |
| `velox_billing_failing_subscriptions` above zero, or a subscription is not being invoiced | [Subscriptions whose billing is failing](billing-failing.md) |
| A period ended but its invoice is not built yet, or usage is missing from an invoice | [Invoice timing and late usage](invoice-timing-late-usage.md) |
| `velox_usage_unpriced_quantity_total` rising, or a WARN with "no rating rule" and "not billed" | [Usage with no price](usage-with-no-price.md) |
| "Usage below zero billed as zero" on a subscription's timeline, or a WARN with "usage below zero" | [Usage below zero](usage-below-zero.md) |
| Write I/O high for minutes after a bulk load | [Write I/O after a large backfill](write-io-after-backfill.md) |
| p99 jumps, p50 flat, after a quiet spell (RDS) | [Commits stall after a quiet spell on RDS](rds-wal-pool.md) |
| p50 lifts for everyone for seconds on a large table | [Autovacuum stalls every commit](autovacuum-stall.md) |

## Reference

- [Health checks, alerts and logs](monitoring.md): the health endpoints,
  which metrics should page someone, and the log lines to search.
- [Manual fixes](manual-interventions.md): resolving a dunning run, marking
  an invoice paid, and a customer with no usable card.
- [Escalation](escalation.md): what to do for SEV-1, SEV-2 and SEV-3
  incidents.
