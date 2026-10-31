# LiteLLM spend gap after an outage

LiteLLM is an AI proxy that can send each model call's token usage to
Hardcount. If Hardcount refuses a batch during an outage and LiteLLM runs out of
retries, that usage is lost. Back to the [runbook index](index.md).

## Symptom

- The `…/litellm/spend` 503 counter rose during a Postgres failover or a
  rolling restart:
  `hardcount_http_requests_total{method="POST",path="/v1/integrations/litellm/spend",status="503"}`.
- LiteLLM's proxy log shows `Generic API Logger Error sending batch`.
  LiteLLM 1.83.9 never sends a batch again after a timeout or an error
  answer. LiteLLM 1.84.0 and later
  send it again after a timeout or a 5xx only while `max_retries` lasts,
  and never after a 4xx, such as a 400 for a body over 1 MB
  ([what LiteLLM sends again](../../integrations/litellm.md#what-litellm-sends-again-and-what-is-lost)).

## Why

Hardcount answered 503 to spend batches while its usage store was unreachable,
so LiteLLM could send them again. A batch LiteLLM did not send again, or
whose retries ran out, is lost: every LLM call in it. Hardcount's spend view is then short
against LiteLLM's spend page for the period.

## Check

Compare the window's calls in LiteLLM's spend log with what Hardcount
stored. A call Hardcount holds has usage events whose idempotency key starts
with its `request_id`
(`GET /v1/usage-events?customer_id=<id>&from=<start>&to=<end>`).

## Fix

Replay the window's calls to `POST /v1/integrations/litellm/spend`.

- **Rebuild each call as the record LiteLLM sends.** The endpoint takes
  LiteLLM's logging record, not a spend-log row. Hardcount ships no tool
  that converts one to the other yet, so build one JSON object per row of
  LiteLLM's spend log (`LiteLLM_SpendLogs`):
  - `id`: the row's `request_id`. It is LiteLLM's call ID, so a call
    Hardcount already holds gets the same keys and is deduplicated.
  - `call_type`, `model`, `custom_llm_provider`, and the customer's ID in
    `end_user`, as the live record carries them.
  - `usage`: `prompt_tokens`, `completion_tokens`, and the cache counts
    (`cache_read_input_tokens`, `cache_creation_input_tokens`, or
    `prompt_tokens_details`), as LiteLLM reported them for the call.

  Send them as `{"events": [ ... ]}`. What each field is for:
  [Send usage from LiteLLM](../../integrations/litellm.md).
- Every billed row carries an idempotency key
  (`<call id>:input|cache_read|cache_write_5m|cache_write_1h|output`), and a
  call's rows are saved together. Nothing else goes into the key, so the
  spend log's millisecond times don't matter, and replaying a window that
  was partly recorded only fills the gap. For a call type Hardcount billed
  before 2026-10-22 (completion, chat, embedding, text completion) the key
  uses the call ID as it is. For the others, an ID that would make a key
  longer than 255 characters goes in as its SHA-256, which Hardcount works
  out the same way on the replay. A call first saved before cache writes
  were split (2026-10-22) is saved the old way again, not billed twice.
- **Deadline:** the replay must reach Hardcount before the period's invoice is
  built. That is `INVOICE_GRACE_PERIOD` (default 1 hour) after the period
  ends, plus up to one billing tick. A replay after that is stored but not
  billed; bill it with a one-off invoice (see
  [Invoice timing and late usage](invoice-timing-late-usage.md)).
- A replayed row that missed its period's invoice is counted in the
  answer's `after_close` and in
  `hardcount_usage_unbilled_total{reason="after_close"}`, with a WARN line
  naming it. Rows on a canceled subscription count as
  `outside_subscription` instead, and rows before a spend-threshold
  invoice that is not paid yet are not counted (see
  [Invoice timing and late usage](invoice-timing-late-usage.md)). These
  counts are what Hardcount found when each row arrived; the invoice is the
  final answer. `hardcount_usage_late_event_total` counts only events more than
  24 hours old on arrival, so it does not show which rows missed an
  invoice.

## Verify

Rows Hardcount already held come back as `deduplicated`, never counted twice,
and Hardcount's spend for the window now matches LiteLLM's.
