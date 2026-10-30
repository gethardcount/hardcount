# Operations runbook

What to do when Hardcount is in trouble. This runbook is for whoever runs a
Hardcount deployment, often during an incident. Find what you see in the table
below and open its page. Most pages follow the same order: **Symptom**,
**Why**, **Check** (commands), **Fix**, and **Verify**.

It covers failures specific to Hardcount. General Postgres, Kubernetes and
Stripe troubleshooting is out of scope.

## Before you start

Most **Check** steps call the Hardcount API or the database. Set these once:

```bash
export API=https://billing.example.com   # your Hardcount address
export KEY=hc_secret_live_...           # a secret API key for the tenant
```

SQL steps run in `psql` as the database owner. Start the session with this
line, which every SQL step on these pages also begins with:

```sql
SET app.bypass_rls = 'on';
```

Every table that holds account data enforces row-level security, for its
owner too. Without the line, a query sees no rows and an update changes
none, with no error. A superuser skips row-level security, so on the Docker
Compose setup, whose database owner is a superuser, the queries work either
way. On a managed database (Amazon RDS, Cloud SQL and the like) the admin
user usually is not one. To check yours:
`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user;`.
A step that inserts a row also sets `app.livemode`, and says so.

## Words on these pages

- **Tenant**: one account's data. Many tenants share one database.
- **Replica**: one running copy of the Hardcount server.
- **Tick**: one run of a background job, such as one billing pass.
- **Lease**: a short booking, in the database, that lets one replica run
  one tick of a job. Like a meeting-room booking, it expires unless it is
  renewed.
- **Fence token**: a number on the lease that goes up each time a replica
  takes it. Work checks the number, so a replica whose lease ran out
  cannot claim anything.
- **Outbox**: a table of emails or webhooks waiting to be sent.
- **Parked**: set aside until something changes
  ([invoices](unclear-charge.md#parked-no-paymentintent),
  [subscriptions](billing-failing.md)).
- **Wait ladder**: the growing waits between billing retries
  ([Billing failing](billing-failing.md#why)).
- **PI**: a Stripe PaymentIntent, the Stripe object for one card charge.

## What do you see?

| If you see | Open |
|---|---|
| The `scheduler` check in `/health/ready` starts with `degraded`, no invoices are being generated, or a background job looks stuck | [A background job is stuck](background-job-stuck.md) |
| `hardcount_email_outbox_failing_emails` or `hardcount_email_outbox_oldest_due_age_seconds` firing, emails dead-lettered, or customers report missing invoice emails | [Emails not going out](email-outbox-backed-up.md) |
| `hardcount_webhook_outbox_oldest_pending_age_seconds` firing, or `webhook_outbox` rows dead-lettered | [Webhook outbox backed up](webhook-outbox-backed-up.md) |
| One customer's webhook endpoint keeps failing (`hardcount_webhook_deliveries_total{status="failed"}`) | [A webhook endpoint keeps failing](webhook-endpoint-failing.md) |
| `hardcount_stripe_breaker_state == 2`, or dunning retries skipped | [Stripe circuit breaker open](stripe-circuit-breaker-open.md) |
| Invoices stuck with an unconfirmed payment for hours | [A charge's result is unclear](unclear-charge.md#unconfirmed-with-a-paymentintent) |
| A payment `processing` for days, or a dunning retry that "is processing" | [A charge's result is unclear: processing](unclear-charge.md#processing-for-days) |
| A test clock stuck on the Advancing badge | [Test-clock advance hung](test-clock-advance-hung.md) |
| A tenant sees another tenant's data | [Data leak between tenants suspected](rls-leak-suspected.md) |
| LiteLLM spend 503s, or Hardcount spend short of LiteLLM's | [LiteLLM spend gap after an outage](litellm-spend-gap.md) |
| A replica will not boot and names Redis | [A replica will not start](replica-will-not-start.md) |
| Someone cannot sign in: "Too many sign-in attempts", or `hardcount_auth_login_total{outcome="throttled"}` rising | [Sign-in says "Too many sign-in attempts"](sign-in-throttled.md) |
| `hardcount migrate` or the start-up stops with `migration … failed`, `goose_db_version`, `no migration record` or `another Hardcount process is migrating` | [A database upgrade stopped](migration-failed-midway.md) |
| `hardcount migrate` or the start-up stops with `this database was made by Velox` | [The database was made by Velox](migration-failed-midway.md#the-database-was-made-by-velox) |
| An upgrade stops with `waited 5s for a table lock` | [A database upgrade stopped: table lock](migration-failed-midway.md#it-waited-5s-for-a-table-lock) |
| `hardcount_stripe_key_unusable_total` rising, or "Stripe refused this tenant's API key" in the logs: charges paused until the key is fixed in Settings → Payments | [Health checks, alerts and logs](monitoring.md) |
| `hardcount_parked_invoices{disposition="open"}` above zero | [A charge's result is unclear: parked](unclear-charge.md#parked-no-paymentintent) |
| An invoice notice reads "Customer must confirm payment", or a payment retry was not attempted because the bank wants the customer to confirm | [The customer's bank wants them to confirm a payment](customer-must-confirm-payment.md) |
| An invoice notice reads "Below Stripe's minimum charge", or a customer's hosted page has no Pay button for a small amount | [An invoice is below Stripe's minimum charge](below-stripe-minimum.md) |
| "stale charge check: can't confirm an earlier charge with Stripe" in the logs, or `hardcount_stale_charge_hold_resolutions_total{result="cannot_ask"}` rising | [A charge's result is unclear: interrupted](unclear-charge.md#interrupted-hardcount-cannot-ask-stripe) |
| An invoice says "after a lost dispute", a `payment.dispute_opened` or `payment.dispute_closed` webhook, or Stripe emails about a dispute | [Card disputes](card-disputes.md) |
| An invoice says "A refund of … was made in Stripe and not recorded here", a `payment.refunded_in_stripe` webhook, or a refund made in the Stripe dashboard | [A refund made in Stripe](refund-made-in-stripe.md) |
| `hardcount_creditnote_pending_issue_drafts` growing over days | [A charge's result is unclear: clawback drafts](unclear-charge.md#clawback-drafts-waiting) |
| `hardcount_audit_write_errors_total` rising | [Audit log gaps](audit-log-gaps.md#audit-write-errors) |
| `hardcount_audit_uncovered_mutation_total` rising | [Audit log gaps](audit-log-gaps.md#a-change-with-no-audit-row) |
| `hardcount_billing_failing_subscriptions` above zero, or a subscription is not being invoiced | [Subscriptions whose billing is failing](billing-failing.md) |
| A period ended but its invoice is not built yet, or usage is missing from an invoice | [Invoice timing and late usage](invoice-timing-late-usage.md) |
| `hardcount_usage_unpriced_quantity_total` rising, or a WARN with "matched no price" and "not billed" | [Usage with no price](usage-with-no-price.md) |
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
- [Failure-mode entries](../../failure-modes/rfm-001-wal-pool-exhaustion.md):
  infrastructure failures diagnosed and reproduced on real hardware, each
  with its signature, fix and reproduction. One entry so far (RFM-001).
