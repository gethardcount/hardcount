# LiteLLM spend gap after an outage

LiteLLM is an AI proxy that sends each model call's token usage to
Hardcount. LiteLLM also keeps its own spend log: one row per call, in the
`LiteLLM_SpendLogs` table of its database. If Hardcount refuses a batch of
calls and LiteLLM does not send it again, that usage is lost. This page
finds the lost calls and sends them again from the spend log. Its Hardcount
commands use `API` and `KEY` from [Before you start](index.md#before-you-start).

## Symptom

- Hardcount's 503 counter for the LiteLLM route,
  `POST /v1/integrations/litellm/spend`, rose while Hardcount could not
  reach its own database, such as during a Postgres failover:
  `hardcount_http_requests_total{method="POST",path="/v1/integrations/litellm/spend",status="503"}`.
- LiteLLM's proxy log shows `Generic API Logger Error sending batch`, its
  error for a batch Hardcount did not take. While Hardcount restarts, its
  own counter shows nothing: it refuses the connection.
- Hardcount's usage for a customer is short of LiteLLM's. This can happen
  with no outage at all: LiteLLM can drop calls when nothing failed
  ([why](../../integrations/litellm.md#calls-lost-when-nothing-failed)).

## Why

Hardcount answers 503 while it cannot reach its database, and refuses
connections while it restarts. LiteLLM sends such a batch again only in some
cases, and only for a short time
([what each version sends again](../../integrations/litellm.md#what-litellm-sends-again-and-what-is-lost)).
A batch it does not send again is lost, with every call in it.

## Check

Compare each customer's tokens over a window, from a few minutes before the
outage to a few minutes after it. LiteLLM stores its times in UTC, in
columns with no time zone, so write the window in UTC, ending in `Z`.
`LITELLM_DATABASE_URL` is the address of LiteLLM's database, the
`DATABASE_URL` of the proxy. `end_user` is the customer's external ID, the
`user` your app passes to LiteLLM.

1. **Sum LiteLLM's tokens per customer.** Save this as `compare.sql`:

   ```sql
   SELECT end_user, sum(prompt_tokens + completion_tokens) AS tokens
   FROM "LiteLLM_SpendLogs"
   WHERE "endTime" >= :'from' AND "endTime" < :'to'
     AND "startTime" >= (:'from')::timestamp - interval '1 hour'
     AND coalesce(status, '') <> 'failure'
     AND coalesce(cache_hit, '') <> 'True'
   GROUP BY end_user
   ORDER BY end_user;
   ```

   ```bash
   FROM=2026-11-08T13:15:00Z; TO=2026-11-08T13:22:00Z
   psql "$LITELLM_DATABASE_URL" -v from="$FROM" -v to="$TO" -f compare.sql
   ```

   - It picks calls by the time they ended. Hardcount dates each call with
     that time too.
   - The `startTime` line lets Postgres use LiteLLM's index. It leaves out
     a call that ran for more than an hour: if yours can, make the hour
     longer.
   - Failed calls and LiteLLM cache hits are left out: Hardcount bills
     neither.

2. **Sum Hardcount's tokens for each `end_user` that step 1 lists.**
   LiteLLM usage goes to Hardcount's `tokens` meter, as up to five usage
   events per call: input, cache reads, cache writes (two kinds) and
   output. Their sum is LiteLLM's prompt tokens plus completion tokens. The
   window works the same way: `from` inclusive, `to` exclusive, by the time
   each event is dated.

   ```bash
   EXTERNAL_ID=<end_user>
   TOKENS=$(curl -sS "$API/v1/meters" -H "Authorization: Bearer $KEY" \
     | jq -r '.data[] | select(.event_name == "tokens") | .id')
   CUSTOMER=$(curl -sS -G "$API/v1/customers" -H "Authorization: Bearer $KEY" \
     --data-urlencode "external_id=$EXTERNAL_ID" | jq -r '.data[0].id')
   curl -sS -G "$API/v1/usage-events/aggregate" -H "Authorization: Bearer $KEY" \
     --data-urlencode "customer_id=$CUSTOMER" --data-urlencode "meter_id=$TOKENS" \
     --data-urlencode "from=$FROM" --data-urlencode "to=$TO" | jq .total_units
   ```

   **You see:** the same number as step 1 when nothing is lost. A lower
   one means lost calls. A higher one means something else sends `tokens`
   events for that customer, such as your own app.

## Fix

Send the window's calls again. Each usage event has an idempotency key, a
unique key Hardcount builds from the call and keeps forever. So a call
Hardcount already holds comes back `deduplicated`, and is never billed
twice.

Send them before the period's invoice is built: `INVOICE_GRACE_PERIOD`,
a setting of the Hardcount server (1 hour by default), after the billing
period ends, plus up to one billing run
([when invoices are made](../../api/timing-and-limits.md#when-invoices-are-made)).
Hardcount never bills, on its own, a call that arrives after that: step 3
does.

1. **Build one record per spend-log row.** Save this as `replay.sql`:

   ```sql
   SELECT json_build_object(
     'id', request_id,
     'call_type', call_type,
     'model', model,
     'custom_llm_provider', custom_llm_provider,
     'end_user', end_user,
     'startTime', extract(epoch FROM "startTime"),
     'endTime', extract(epoch FROM "endTime"),
     'prompt_tokens', prompt_tokens,
     'completion_tokens', completion_tokens,
     'metadata', json_build_object('usage_object', metadata->'usage_object'))
   FROM "LiteLLM_SpendLogs"
   WHERE "endTime" >= :'from' AND "endTime" < :'to'
     AND "startTime" >= (:'from')::timestamp - interval '1 hour'
     AND coalesce(status, '') <> 'failure'
     AND coalesce(cache_hit, '') <> 'True'
   ORDER BY "endTime";
   ```

   ```bash
   psql "$LITELLM_DATABASE_URL" -At -v from="$FROM" -v to="$TO" -f replay.sql > replay.ndjson
   ```

   The fields that matter:
   - `id` is LiteLLM's call ID, the same as in the record LiteLLM first
     sent. Hardcount builds the keys from it and the kind of token, so the
     keys match the first send's. `call_type` comes from the same record:
     it decides whether an ID over 240 characters is shortened to its
     SHA-256, as on the first send.
   - `endTime`, in unix seconds, dates the call, so it is billed in the
     period it ended in. Without it, Hardcount dates the call at the moment
     of the replay and bills it in the current period.
   - `end_user` is the customer. A row without one cannot be billed:
     Hardcount lists it in the answer's `errors`. LiteLLM's `user` column
     is the owner of the LiteLLM key, so it is left out.
   - `metadata.usage_object` holds the token counts, with the cache reads
     and cache writes.

2. **Send the records.** Each is about 1 KB, and one request takes up to
   32 MB, so send them in parts of 20,000. Each answer is saved next to its
   part:

   ```bash
   split -l 20000 replay.ndjson part-
   for f in part-*; do
     curl -sS -X POST "$API/v1/integrations/litellm/spend" \
       -H "Authorization: Bearer $KEY" -H "Content-Type: application/x-ndjson" \
       --data-binary @"$f" | tee "answer-$f"
     echo
   done
   ```

   **You see:** one answer per part, such as
   `{"accepted":8,"deduplicated":8,"held":0,"skipped":0,"cache_hits":0,"after_close":0,"no_price":3,"not_on_plan":0,"outside_subscription":0}`.
   - The counts are of usage events. `accepted` counts the new ones, and
     `deduplicated` the ones Hardcount already held.
   - `after_close` counts the accepted events whose period was already
     invoiced: step 3.
   - `held` counts events whose customer does not exist in Hardcount yet.
     They become usage events, dated when each call ended, once you create
     the customer and release them
     ([held usage](../../integrations/litellm.md#usage-for-a-customer-hardcount-does-not-know-yet)).
   - `no_price` counts events that no price of the plan takes:
     [Usage with no price](usage-with-no-price.md).
   - A part that failed can be sent again: rows already stored come back
     `deduplicated`.

3. **Bill what missed its invoice.** For each event counted in
   `after_close`, Hardcount's log has a WARN line,
   `usage event arrived after its period was invoiced`, with its customer
   and time. A second send counts them as `deduplicated`, so read the
   first answers or the WARN lines. For each such customer, follow
   [Usage is missing from a built invoice](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice):
   it works out the amount.

A replayed call has no `team_id` or `request_tags` label. A price that names
one of them does not match it, so the call bills at another price, or shows
as usage with no price.

## Verify

Run the Check again: each customer's totals now match, unless step 2 of the
Check found more in Hardcount, or some rows are held.
