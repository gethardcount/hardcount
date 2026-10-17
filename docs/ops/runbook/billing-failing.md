# Subscriptions whose billing is failing

When Velox cannot bill a subscription at the end of its period, it saves
the failure on the subscription and tries again later, waiting longer each
time. This page is for finding why and billing it. Back to the
[runbook index](index.md).

## Symptom

- `velox_billing_failing_subscriptions{mode,state}` is above zero.
- Or a customer was not invoiced for a period that has ended.

## Why

The billing run could not bill the subscription. It saved the failure on
the subscription and now waits before each new attempt.

- **`state="retrying"`**: the billing run will try again on its own. The
  waits after each failed attempt are 15 minutes, 1 hour, 4 hours,
  12 hours, then 24 hours three times. On the hourly production tick, an
  attempt happens on the first tick at or after the wait ends.
- **`state="parked"`**: eight attempts failed. Nothing retries the
  subscription until someone calls retry-billing. The earliest a
  subscription can park is 3 days 17 hours after its first failure.

A parked subscription skips no period. When it is retried, it is billed for
every period it missed, one invoice per period. Waiting still has a cost:

- Prepaid credits that expire in the meantime do not apply to the late
  invoice.
- Usage thresholds do not fire for a period that has already ended.
- The subscription cannot be cancelled or changed until it is billed. An
  immediate cancel, a scheduled cancel or its removal, a collection pause
  or resume, and any item or plan change answer 409 `unbilled_period`.
  Each one would otherwise act on the period that ended instead of the one
  that is running. The one exception is a subscription with no items:
  adding an item is how that failure is fixed, so it is allowed.

Retry promptly once the cause is fixed.

A cancel scheduled for the end of a trial still fires on a failing or
parked trialing subscription. Any other change that leaves the same work
due does not restart billing; retry after it.

### Why the error rate falls

`velox_billing_cycle_errors_total` falls once a fault has lasted a few
ticks, because a subscription that failed is not attempted again until its
wait has passed. On the hourly production tick, the first three ticks each
retry everything (the waits are 15 minutes, then 1 hour). From the fourth
tick the waits are longer than the tick. So a low error rate is not an
all-clear: alert on `velox_billing_failing_subscriptions` too.

## Check

```bash
# Which subscriptions, and why
curl -sS "$API/v1/subscriptions?billing=failing" -H "Authorization: Bearer $KEY" \
  | jq '.data[] | {id, code, billing_failure}'
```

**You see:** one object per failing subscription. `billing_failure.last_error`
is either a sentence that names the cause, or
`An internal error occurred. Reference: <subscription id>`. For the second
kind, the full error is in the server log:

```bash
grep "bill subscription failed" log | jq 'select(.subscription_id == "<subscription id>")'
```

Many subscriptions failing with the same error means one shared cause.
Check the tax provider setting and the last deploy first.

A database outage does not normally use up attempts. A lost connection, a
server shutdown, a deadlock and a serialization abort are not counted. Two
kinds of outage are counted, because the error type cannot tell them from a
real failure:

- a statement or lock timeout;
- a server that answers but refuses writes: a standby reached during a
  failover (SQLSTATE 25006).

After such an outage, retry the affected subscriptions; do not wait out the
ladder.

## Fix

1. **Fix the cause.** Until the period is billed, every change to the
   subscription (items, plan, a scheduled cancel, a collection pause) is
   refused with 409 `unbilled_period`. The exception is a subscription with
   no items: there, add an item (`POST /v1/subscriptions/{id}/items`) and
   retry.
2. **Retry the subscription.** This clears the saved failure, writes an
   audit log entry, and bills at once:

   ```bash
   curl -sS -X POST "$API/v1/subscriptions/<subscription id>/retry-billing" \
     -H "Authorization: Bearer $KEY" | jq '{invoices_generated, still_due, failure: .subscription.billing_failure}'
   ```

3. **After a shared cause is fixed, retry every failing subscription of the
   tenant.** There is no bulk endpoint; loop over the list:

   ```bash
   curl -sS "$API/v1/subscriptions?billing=failing&limit=100" -H "Authorization: Bearer $KEY" \
     | jq -r '.data[].id' \
     | while read -r id; do
         curl -sS -X POST "$API/v1/subscriptions/$id/retry-billing" -H "Authorization: Bearer $KEY" \
           | jq -c '{id: .subscription.id, invoices_generated, still_due, still_failing: (.subscription.billing_failure != null)}'
       done
   ```

   Repeat until the list is empty; each page holds at most 100. An empty
   list does not prove that everything billed: a row printed with
   `still_due: true` left the list without being billed. The next billing
   run attempts it.

A cancel does not clear a park. An immediate cancel of a subscription whose
ended period is not invoiced answers 409 with code `unbilled_period`, and
the subscription stays active. Fix the cause, retry billing as above, then
cancel again. A cancel that arrives after the period end on a healthy
subscription bills the ended period itself and needs no retry.

`POST /v1/billing/run` is not a retry. It skips subscriptions that are
waiting or parked.

## Verify

Read two fields of the retry response:

| Response | Meaning |
|---|---|
| no `billing_failure`, `still_due: false` | Billed, or nothing was left to bill |
| `billing_failure` present | The attempt failed again. `attempts` is back to 1 and the billing run retries on its own. This includes an attempt that ended without an error but left the subscription due ("Billing didn't finish, and the subscription is still due") |
| no `billing_failure`, `still_due: true` | The attempt did not complete: a database connection was lost, or a deadlock. Nothing was saved and the subscription is unbilled. The next billing run attempts it, and the server log has the reason (`retry-billing: the attempt failed`). A second call answers 409, because the failure is already cleared |

`invoices_generated` is not the success test. It counts period-close
invoices only. It is 0 when the retry fired a scheduled cancel (the
subscription comes back `canceled`, with its final invoice), ended a trial,
or closed a period with nothing to bill.

A call that times out returns an error, but may already have cleared the
failure. A repeat call can then answer 409 `billing_not_failing` while the
next billing run bills the subscription.

`velox_billing_failing_subscriptions` falls on the next scrape.

**Not covered here:** a subscription on a test clock never appears in this
gauge or list. Its failure stops the clock; see
[Test-clock advance hung](test-clock-advance-hung.md).

Related: [ADR-119](../../adr/119-billing-failure-record-keyed-to-the-due-instant.md).
