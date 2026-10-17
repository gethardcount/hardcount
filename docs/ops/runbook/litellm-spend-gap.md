# LiteLLM spend gap after an outage

LiteLLM is an AI proxy that can send each model call's token usage to
Velox. If Velox refuses a batch during an outage and LiteLLM runs out of
retries, that usage is lost. This page is for filling the gap. Back to the
[runbook index](index.md).

## Symptom

- The `…/litellm/spend` 503 counter rose during a Postgres failover or a
  rolling restart:
  `velox_http_requests_total{method="POST",path="/v1/integrations/litellm/spend",status="503"}`.
- LiteLLM's proxy log shows `Generic API Logger Error sending batch` (its
  `max_retries` were used up).
- Or LiteLLM was never configured with retries
  ([LiteLLM setup](../../integrations/litellm.md)).

## Why

Velox answered 503 to spend batches while its usage store was unreachable,
so LiteLLM would retry them. A batch whose retries ran out is lost: every
LLM call LiteLLM sent in that window. Velox's spend view is then short
against LiteLLM's spend page for the period.

## Check

Compare Velox's usage for the window with LiteLLM's spend logs for the same
window.

## Fix

Replay LiteLLM's spend logs for the window to
`POST /v1/integrations/litellm/spend`.

- Every row carries an idempotency key
  (`<litellm_call_id>:input|output|cache_read`). So replaying a window that
  was partly recorded only fills the gap.
- **Deadline:** the replay must reach Velox before the period's invoice is
  built. That is `INVOICE_GRACE_PERIOD` (default 1 hour) after the period
  ends, plus up to one billing tick. A replay after that is stored but not
  billed; bill it with a one-off invoice (see
  [Invoice timing and late usage](invoice-timing-late-usage.md)).
- `velox_usage_late_event_total` counts only events more than 24 hours old
  on arrival, so it does not show which rows missed an invoice.

## Verify

Rows Velox already held come back as `deduplicated`, never counted twice,
and Velox's spend for the window now matches LiteLLM's.
