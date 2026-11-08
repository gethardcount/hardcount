# Subscriptions whose billing is failing

When Hardcount cannot bill a subscription at the end of its period, it saves
the failure on the subscription and tries again later, waiting longer each
time. Back to the [runbook index](index.md).

## Symptom

- `hardcount_billing_failing_subscriptions{mode,state}` is above zero.
- Or a customer was not invoiced for a period that has ended.

## Why

The billing run could not bill the subscription. It saved the failure on
the subscription and now waits before each new attempt.

- **`state="retrying"`**: the billing run will try again on its own. The
  waits after each failed attempt are 15 minutes, 1 hour, 4 hours,
  12 hours, then 24 hours three times. On the hourly production tick, an
  attempt happens on the first tick at or after the wait ends.
- **`state="parked"`**: eight attempts failed. Nothing retries the
  subscription until someone calls retry-billing
  (`POST /v1/subscriptions/{id}/retry-billing`, see [Fix](#fix)). The
  earliest a subscription can park is 3 days 17 hours 15 minutes after its
  first failure.

A parked subscription skips no period. When it is retried, it is billed for
every period it missed, one invoice per period. Waiting still has a cost:

- Credit that expires while a period waits still pays that period's
  invoice, and stays in the ledger balance until then
  ([Credits](../../concepts/credits.md#expiry)).
- Spend thresholds do not fire for a period that has already ended.
- The subscription cannot be canceled or changed until it is billed
  ([below](#changes-while-billing-is-failing)).

Retry promptly once the cause is fixed.

A database outage does not normally use up attempts. A lost connection, a
server shutdown, a deadlock and a serialization abort are not counted.
Three kinds of outage are counted, because the error type cannot tell them
from a real failure:

- a statement or lock timeout;
- a server out of resources, such as too many connections (SQLSTATE
  class 53);
- a server that answers but refuses writes: a standby reached during a
  failover (SQLSTATE 25006).

After such an outage, retry the affected subscriptions
([Fix](#fix), step 3); do not wait out the ladder.

### Changes while billing is failing

- An immediate cancel, a scheduled cancel or its removal, a collection
  pause or resume, and any item or plan change answer 409
  `unbilled_period`. Each would otherwise act on the period that ended
  instead of the one that is running.
- The one exception is a subscription with no items: adding an item is how
  that failure is fixed, so it is allowed.
- A cancel scheduled for the end of a trial still fires on a trialing
  subscription that is failing or parked.
- A change the 409 rule allows, such as adding an item to a subscription
  with none, clears the failure only if it changes when the subscription
  is due. If `billing_failure` is still set afterwards, run retry-billing
  ([Fix](#fix), step 2).

### Why the error rate falls

`hardcount_billing_cycle_errors_total` falls once a fault has lasted a few
ticks, because a subscription that failed is not attempted again until its
wait has passed. On the hourly production tick, the first three ticks each
retry everything (the waits are 15 minutes, then 1 hour). From the fourth
tick the waits are longer than the tick. So a low error rate is not an
all-clear: alert on `hardcount_billing_failing_subscriptions` too.

## Check

```bash
# Which subscriptions, and why
curl -sS "$API/v1/subscriptions?billing=failing" -H "Authorization: Bearer $KEY" \
  | jq '.data[] | {id, code, billing_failure}'
```

**You see:** one object per failing subscription. `billing_failure.last_error`
is either a sentence that names the cause, or
`An internal error occurred. Reference: <subscription id>`. For the second
kind, the full error is in the server's log. On Docker Compose:

```bash
docker compose logs --no-log-prefix hardcount-api | grep '"bill subscription failed"' \
  | jq 'select(.subscription_id == "<subscription id>")'
```

Many subscriptions failing with the same error means one shared cause.
Check the tax provider setting and the last deploy first.

## Fix

While you work, the billing run still attempts each `retrying`
subscription when its wait ends. It and retry-billing make the same
attempt, so each period is billed once, whichever runs first.

1. **Fix the cause.** For a subscription with no items, add one
   (`POST /v1/subscriptions/{id}/items`).
2. **Retry the subscription.** This clears the saved failure, writes an
   audit log entry, and bills at once:

   ```bash
   curl -sS -X POST "$API/v1/subscriptions/<subscription id>/retry-billing" \
     -H "Authorization: Bearer $KEY" | jq '{invoices_generated, still_due, failure: .subscription.billing_failure}'
   ```

3. **After a shared cause is fixed, retry every failing subscription of the
   account.** There is no bulk endpoint. The list is read 100 at a time,
   and a subscription that leaves it while it is read makes the next page
   skip one. So this lists every failing subscription before it retries
   any. The billing run can still bill one while the list is read, so it
   then reads the list again, and retries each subscription it has not
   retried yet. It stops when the list holds no such subscription:

   ```bash
   : > retried.txt
   while :; do
     : > failing.txt; offset=0; listed=1
     while :; do
       resp=$(curl -sSf "$API/v1/subscriptions?billing=failing&limit=100&offset=$offset" \
         -H "Authorization: Bearer $KEY") || { listed=0; break; }
       jq -r '.data[].id' <<<"$resp" >> failing.txt
       [ "$(jq '.data | length' <<<"$resp")" -lt 100 ] && break
       offset=$((offset + 100))
     done
     [ "$listed" = 1 ] || { echo "The list failed: run this again."; break; }
     grep -vxFf retried.txt failing.txt > todo.txt
     [ -s todo.txt ] || { echo "Every failing subscription on the list was retried."; break; }
     while read -r id; do
       curl -sS -X POST "$API/v1/subscriptions/$id/retry-billing" -H "Authorization: Bearer $KEY" \
         | jq -c --arg id "$id" '{id: $id, invoices_generated, still_due,
             still_failing: (.subscription.billing_failure != null), error: .error.code}'
       echo "$id" >> retried.txt
     done < todo.txt
   done
   ```

   **You see:** one line per subscription, then
   `Every failing subscription on the list was retried.` Read each line
   with the table in [Verify](#verify).
   - A subscription that is still failing after its retry stays on the
     list. The loop does not retry it again.
   - A line with `error: "billing_not_failing"` has no saved failure any
     more: the billing run or another retry got to it first.
   - If the list fails, the loop stops before it retries anything from
     that list. Run the whole block again. It can retry a subscription
     that is still failing once more, which only makes one more attempt.

A cancel that arrives after the period end on a healthy subscription bills
the ended period itself and needs no retry.

`POST /v1/billing/run` is not a retry. It skips subscriptions that are
waiting or parked.

## Verify

Read two fields of the retry response:

| Response | Meaning |
|---|---|
| no `billing_failure`, `still_due: false` | Billed, or nothing was left to bill |
| `billing_failure` present | The attempt failed again. `attempts` is back to 1 and the billing run retries on its own. This includes an attempt that ended without an error but left the subscription due ("Billing didn't finish, and the subscription is still due") |
| no `billing_failure`, `still_due: true` | The attempt did not complete: a database connection was lost, or a deadlock. Nothing was saved and the subscription is unbilled. The next billing run attempts it, and the server log has the reason (`retry-billing: the attempt failed`). A second call answers 409, because the failure is already cleared |

`invoices_generated` is not the success test. It counts only the invoices
built when a period closes, at the end of a billing period. It is 0 when
the retry fired a scheduled cancel (the subscription comes back `canceled`,
with its final invoice), ended a trial, or closed a period with nothing to
bill.

A call that times out returns an error, but may already have cleared the
failure. A repeat call can then answer 409 `billing_not_failing` while the
next billing run bills the subscription.

`hardcount_billing_failing_subscriptions` falls on the next scrape.

**Not covered here:** a subscription on a test clock (a pretend clock for
test customers) never appears in this gauge or list. Its failure stops the
clock; see [Test-clock advance hung](test-clock-advance-hung.md).

Related: [ADR-119](../../adr/119-billing-failure-record-keyed-to-the-due-instant.md).
