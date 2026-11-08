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
outage to a few minutes after it. With no outage, use the period you
suspect, for example the last full day, or the current billing period so
far. LiteLLM stores its times in UTC, in columns with no time zone, so
write the window in UTC, ending in `Z`.
`LITELLM_DATABASE_URL` is the address of LiteLLM's database, the
`DATABASE_URL` of the proxy. `end_user` is the customer's external ID: the
`user` field of each request your app sends to LiteLLM.

1. **Sum LiteLLM's tokens per customer.** Save this as `compare.sql`:

   ```sql
   SELECT end_user, sum(prompt_tokens + completion_tokens) AS tokens
   FROM "LiteLLM_SpendLogs"
   WHERE "endTime" >= :'from' AND "endTime" < :'to'
     AND "startTime" >= (:'from')::timestamp - interval '1 hour'
     AND coalesce(status, '') <> 'failure'
     AND coalesce(cache_hit, '') <> 'True'
     AND NOT (call_type = 'pass_through_endpoint' AND request_id LIKE 'vertex-ai-live-%')
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
   - Vertex AI Live calls are left out too: Hardcount does not bill them
     ([which calls bill](../../integrations/litellm.md#which-litellm-calls-bill)).
     The spend log does not keep the route that marks them, so the SQL
     finds them by the call ID LiteLLM gives them: `vertex-ai-live-`, then
     a time.

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
   - Calls of a kind Hardcount does not bill, such as image calls, also
     make Hardcount's number lower. Sending them again changes nothing:
     the answer counts each such call in `skipped`.

## Fix

Send the window's calls again. Each usage event has an idempotency key, a
unique key Hardcount builds from the call and keeps forever. So a call
Hardcount already holds comes back `deduplicated`, and is never billed
twice. For the same reason, sending a call again cannot change it: do
step 1 before you send anything.

Send them before the period's invoice is built. That happens one grace
period (`INVOICE_GRACE_PERIOD`, 1 hour by default) after the period ends,
at the next billing run
([when invoices are made](../../api/timing-and-limits.md#when-invoices-are-made)).
Hardcount never bills, on its own, a call that arrives after that: step 4
does.

1. **Check your prices first.** A live call has a `team_id` label when your
   app put `team_id` in the request's `metadata`. The spend log does not
   keep that value: its `team_id` column is the team of the LiteLLM key,
   which can differ. So a replayed call has no `team_id` label. A price
   that names `team_id` does not take it, and the call bills at another
   price, or as usage with no price. For each plan of the customers the
   Check found short, list the prices that name `team_id`:

   ```bash
   PLAN=<plan id>
   curl -sS "$API/v1/plans/$PLAN/prices?limit=100" -H "Authorization: Bearer $KEY" \
     | jq -c '.data[] | select(.dimension_match.team_id != null) | {id, meter_id, dimension_match}'
   ```

   **You see:** nothing, when no price of the plan names `team_id`.
   Otherwise, leave that plan's customers out of the replay: add
   `AND end_user NOT IN ('<external id>', ...)` to the `WHERE` of
   `replay.sql` in step 2. Bill their lost calls by hand: price their rows
   in LiteLLM's spend log at the plan's prices. Bill the total on a
   one-off invoice, as in steps 4 and 5 of
   [Usage is missing from a built invoice](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice).
   Give it its own memo and idempotency key there:
   `MEMO="LiteLLM calls not replayed, $FROM to $TO"` and
   `IDEMPOTENCY_KEY="not-replayed-$CUSTOMER-$FROM"`.

2. **Build one record per spend-log row.** Save this as `replay.sql`:

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
     AND NOT (call_type = 'pass_through_endpoint' AND request_id LIKE 'vertex-ai-live-%')
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
     Hardcount lists it in the answer's `errors`. The spend log's `user`
     column (the key's owner, not your customer) is left out.
   - `metadata.usage_object` holds the token counts, with the cache reads
     and cache writes.
   - Vertex AI Live calls are left out, as in the Check. A replayed record
     does not carry the route that tells Hardcount to skip them, so they
     would be billed.

3. **Send the records.** Each is about 1 KB, and one request takes up to
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
   - `accepted` counts the new usage events, and `deduplicated` the ones
     Hardcount already held.
   - `after_close` counts the accepted events whose period was already
     invoiced: step 4.
   - `held` counts events whose customer does not exist in Hardcount yet.
     They become usage events, dated when each call ended, once you create
     the customer and release them
     ([held usage](../../integrations/litellm.md#usage-for-a-customer-hardcount-does-not-know-yet)).
   - `no_price` counts events that no price of the plan takes:
     [Usage with no price](usage-with-no-price.md).
   - A part failed when its answer has no `accepted` count, such as an
     `error` object or an empty answer. This sends each failed part again.
     Rows already stored come back `deduplicated`:

     ```bash
     for a in $(grep -L '"accepted"' answer-part-*); do
       f=${a#answer-}
       curl -sS -X POST "$API/v1/integrations/litellm/spend" \
         -H "Authorization: Bearer $KEY" -H "Content-Type: application/x-ndjson" \
         --data-binary @"$f" | tee "$a"
       echo
     done
     ```

4. **Bill what missed its invoice.** A call whose period was already
   invoiced when you sent it is stored, and no invoice bills it. For each
   customer the Check found short, apart from those step 1 left out,
   follow
   [Usage is missing from a built invoice](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice).
   Its step 1 says whether that period's invoice is built yet, and its
   step 3 works out the amount.

   The answers do not name these customers, and they do not count every
   such call:
   - The `after_close` count tells you whether any calls missed their
     invoice. The WARN lines, `usage event arrived after its period was
     invoiced`, tell you which customers did: each carries `customer_id`
     (Hardcount's ID) and `timestamp`. Search the log for them right after
     sending. A second send counts the same calls as `deduplicated`, with
     no WARN line. On Docker Compose:

     ```bash
     docker compose logs --no-log-prefix hardcount-api \
       | grep '"usage event arrived after its period was invoiced' | jq -c '{customer_id, timestamp}'
     ```

   - A call of a canceled subscription is counted in `outside_subscription`
     instead, with no WARN line.
   - A call stamped before a spend-threshold invoice that is not paid yet
     is not counted at all
     ([Spend thresholds](../../concepts/invoices.md#spend-thresholds-an-invoice-made-early)).

## Verify

Run the Check again: each customer's totals now match. The exceptions are
a customer for whom step 2 of the Check found more in Hardcount, one with
rows held or skipped, and one that step 1 of the Fix left out.

## If you replayed with an earlier version of this page

Its records had no `endTime`. Each call it sent that Hardcount did not hold
yet is dated at the moment of that replay. If the replay fell in a later
billing period than the call, the call is billed in that later period.
Sending it again answers `deduplicated` and cannot move it.

The Check finds these calls: run it over the window of the lost calls, and
those customers still show short. A call replayed in its own period needs
nothing. If both periods price the calls the same, the customer paid the
right amount one period late, and you can leave it. Otherwise, for each
such customer:

1. Bill the calls on a one-off invoice for their own period, priced as that
   period priced them, as in steps 4 and 5 of
   [Usage is missing from a built invoice](invoice-timing-late-usage.md#usage-is-missing-from-a-built-invoice).
   Give it its own memo and idempotency key there, with `N` counting only
   such invoices: `MEMO="Replayed calls that belong to $NUMBER ($N)"` and
   `IDEMPOTENCY_KEY="replayed-$INVOICE-$N"`.
2. Once the invoice of the replay's period is built, issue a credit note on
   it for what those calls added
   ([Credit notes](../../concepts/invoices.md#credit-notes)).
