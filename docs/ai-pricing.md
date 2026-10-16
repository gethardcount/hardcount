# AI pricing in code

With no Stripe Billing objects, you can:

- bill Anthropic-style multi-dimensional pricing with **one meter**,
- sell a prepaid commit (an up-front purchase of usage credit) against it,
- and read one customer's usage margin.

The calls below show the main parts. You also need a customer (here with external ID `cust_acme`), a rating rule for each price, and a plan and subscription, because usage is billed on the subscription's cycle invoice.

Start Velox with the [Quick start](../README.md#quick-start). It prints a test API key; put it in `$VELOX_SECRET`.

```bash
# 1. Create one meter for "tokens"
curl -X POST http://localhost:8080/v1/meters \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{"key": "tokens", "name": "LLM tokens", "unit": "token"}'

# 2. Set what your model provider charges you per token. Velox stamps each
#    event's cost when it arrives, so set rates first: a rate change applies
#    only to later events.
curl -X PUT http://localhost:8080/v1/provider-costs \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{"provider": "openai", "model": "gpt-4", "token_type": "input",
       "cost_per_token": "0.00003"}'

# 3. Ingest events that carry the dimensions inline
#    (cost is stamped only when the event has provider, model and token_type).
#    idempotency_key is required: one unique key per event, sent again
#    unchanged when you retry that event. Never reuse it for another event.
curl -X POST http://localhost:8080/v1/usage-events \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{
    "event_name": "tokens",
    "external_customer_id": "cust_acme",
    "quantity": "12450",
    "idempotency_key": "req_8f2c:input",
    "dimensions": {"provider": "openai", "model": "gpt-4", "token_type": "input"}
  }'

# 4. Define one pricing rule per (dimension subset, rate)
#    (rating_rule_version_id: the id of a rating rule you created for this price)
curl -X POST http://localhost:8080/v1/meters/$METER_ID/pricing-rules \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{
    "dimension_match": {"model": "gpt-4", "token_type": "input"},
    "rating_rule_version_id": "vlx_rrv_...",
    "aggregation_mode": "sum",
    "priority": 100
  }'

# 5. Sell a $10k prepaid commit for $9k. The credit block is granted when the
#    invoice finalizes. Later invoices for this customer draw it down.
#    ($INVOICE_ID: a draft one-off invoice from POST /v1/invoices — elided for brevity)
curl -X POST http://localhost:8080/v1/invoices/$INVOICE_ID/line-items \
  -H "Authorization: Bearer $VELOX_SECRET" \
  -d '{"description": "Annual commit", "line_type": "add_on",
       "quantity": 1, "unit_amount_cents": 900000,
       "commit_granted_cents": 1000000}'
curl -X POST http://localhost:8080/v1/invoices/$INVOICE_ID/finalize \
  -H "Authorization: Bearer $VELOX_SECRET"

# 6. Read one customer's usage margin: rated usage revenue against stamped
#    provider cost (last 30 days unless you pass from/to; the dashboard card
#    opens on the current billing period instead, ADR-121)
curl http://localhost:8080/v1/customers/$CUSTOMER_ID/margin \
  -H "Authorization: Bearer $VELOX_SECRET"
```

Already running a LiteLLM proxy? Skip step 3. Point its callback at `POST /v1/integrations/litellm/spend`. Each chat, completion or embedding call becomes token events on the `tokens` meter, with `model`, `provider` and `token_type` dimensions. LiteLLM's `user` must match the customer's external ID. Replays are deduplicated, and no SDK is needed. Cache-write tokens are not billed yet. See [`docs/integrations/litellm.md`](integrations/litellm.md).

Each event is claimed by at most one pricing rule: the highest-priority match (the priority + claim resolver), so nothing is double-counted.

- Token roles do not overlap. If you create one rule per `{model, token_type}`, each event matches exactly one rule. An event no rule matches falls to the meter's default price, if it has one (see below).
- You can also mix a coarse catch-all rule (`{"model": "gpt-4"}`) with finer per-role rules. Priority decides which one claims each event.

**Usage that no rule claims has no price.** A new model name with no rule is the common case. If the meter has a default price (`PATCH /v1/meters/{id}` with `rating_rule_version_id`), that price bills it. If not, Velox builds and sends the invoice without that usage, as Lago, Metronome and Orb do. You see it in four places:

- the "unmatched usage — not billed" row of the customer's usage (`GET /v1/customers/{id}/usage`, and the customer page). The API call covers the current cycle unless you pass `from` and `to`;
- the invoice preview warning, which gives the quantity;
- the `velox_usage_unpriced_quantity_total{meter_id, mode}` counter (test-clock simulations are not counted);
- a server log line when the invoice is built.

To bill it, add a rule or a default price; invoices not built yet then include it. Bill periods already invoiced with a one-off invoice ([runbook §16](ops/runbook.md#16-usage-with-no-price)). Velox refuses (409) to remove a meter's last price while a plan with a live subscription uses the meter.

The original design is in [`docs/design-multi-dim-meters.md`](design-multi-dim-meters.md): schema, aggregation rules, decimal quantities and the five aggregation modes. ADR-044, ADR-045 and ADR-070 record what changed since.
