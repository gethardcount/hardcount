# AI pricing in code

In five steps, with no Stripe Billing objects, you can:

- bill Anthropic-style multi-dimensional pricing with **one meter**,
- sell a prepaid commit (an up-front purchase of usage credit) against it,
- and read per-customer margin.

The API comes from `make dev` and the key from `make bootstrap` (see [Quick start](../README.md#quick-start)).

```bash
# 1. Create one meter for "tokens"
curl -X POST http://localhost:8080/v1/meters \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{"key": "tokens", "name": "LLM tokens", "unit": "token"}'

# 2. Ingest events that carry the dimensions inline
curl -X POST http://localhost:8080/v1/usage-events \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -H "Idempotency-Key: req_8f2c..." \
  -d '{
    "event_name": "tokens",
    "external_customer_id": "cust_acme",
    "quantity": "12450",
    "dimensions": {"model": "gpt-4", "token_type": "input"}
  }'

# 3. Define one pricing rule per (dimension subset, rate)
curl -X POST http://localhost:8080/v1/meters/$METER_ID/pricing-rules \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{
    "dimension_match": {"model": "gpt-4", "token_type": "input"},
    "rating_rule_version_id": "rrv_gpt4_input",
    "aggregation_mode": "sum",
    "priority": 100
  }'

# 4. Sell a $10k prepaid commit for $9k — the credit block funds when the
#    invoice finalizes, and usage draws it down
#    ($INVOICE_ID: a draft one-off invoice from POST /v1/invoices — elided for brevity)
curl -X POST http://localhost:8080/v1/invoices/$INVOICE_ID/line-items \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{"description": "Annual commit", "line_type": "add_on",
       "quantity": 1, "unit_amount_cents": 900000,
       "commit_granted_cents": 1000000}'
curl -X POST http://localhost:8080/v1/invoices/$INVOICE_ID/finalize \
  -H "Authorization: Bearer $VELOX_SECRET"

# 5. Know which customers lose you money — stamped provider COGS vs rated revenue
curl http://localhost:8080/v1/customers/$CUSTOMER_ID/margin \
  -H "Authorization: Bearer $VELOX_SECRET"
```

Already running a LiteLLM proxy? Skip step 2. Point its spend callback at `POST /v1/integrations/litellm/spend`, and every completion lands as dimensioned token events (`model`, `token_type`). Replays are deduped, and no SDK is needed. See [`docs/integrations/litellm.md`](integrations/litellm.md).

Each event is claimed by at most one pricing rule: the highest-priority match (the priority + claim resolver), so nothing is double-counted.

- Token roles are disjoint, so each `{model, token_type}` maps to exactly one rule at equal priority.
- You can also mix a coarse catch-all rule (`{"model": "gpt-4"}`) with finer per-role rules. Priority decides which one claims each event.

**Usage that no rule claims has no price.** A new model name with no rule is the common case. If the meter has a default price (`PATCH /v1/meters/{id}` with `rating_rule_version_id`), that price bills it. If not, Velox builds and sends the invoice without that usage, as Lago, Metronome and Orb do. You see it in four places:

- the "unmatched usage — not billed" row of the customer's usage (`GET /v1/customers/{id}/usage`, and the customer page);
- the invoice preview warning, which gives the quantity;
- the `velox_usage_unpriced_quantity_total{meter_id, mode}` counter (test-clock simulations are not counted);
- a server log line when the invoice is built.

To bill it, add a rule or a default price; invoices not built yet then include it. Bill periods already invoiced with a one-off invoice ([runbook §16](ops/runbook.md#16-usage-with-no-price)). Velox refuses (409) to remove a meter's last price while a plan with a live subscription uses the meter.

The full design lives in [`docs/design-multi-dim-meters.md`](design-multi-dim-meters.md): schema, aggregation semantics, decimal quantities, and all five aggregation modes.
