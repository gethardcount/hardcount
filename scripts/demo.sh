#!/usr/bin/env bash
# Velox demo — the AI billing golden path, end to end in ~30 seconds:
#
#   recipe → customer on a test clock → LiteLLM-shaped token usage
#   → provider cost rates → simulated month → invoice with per-model
#   lines + PDF → per-customer margin.
#
# Usage:
#   make bootstrap        # prints the secret-test key this script needs
#   make dev              # API on :8080 (separate terminal)
#   ./scripts/demo.sh <vlx_secret_test_...>
#
# Every call is checked — the script fails loudly at the first API
# mismatch instead of printing a success banner over broken output.

set -euo pipefail

API_KEY="${1:-}"
BASE="${VELOX_BASE:-http://localhost:8080}"

if [ -z "$API_KEY" ]; then
  echo "Usage: ./scripts/demo.sh <vlx_secret_test_...>"
  echo ""
  echo "Run 'make bootstrap' first to get a key."
  exit 1
fi

command -v jq >/dev/null || { echo "demo.sh needs jq (brew install jq / apt install jq)"; exit 1; }

green() { printf "\033[32m%s\033[0m\n" "$1"; }
blue()  { printf "\033[34m%s\033[0m\n" "$1"; }
step()  { echo ""; blue "━━━ $1 ━━━"; }
# Cents to dollars with two decimals ("$1.20", not jq's "$1.2").
# usd_micros does the same for provider cost, which is in millionths of a
# dollar: it rounds half up to the cent first (1277500 is "$1.28").
# pct1 turns basis points into a percent with one decimal (6707 is "67.1%").
USD='def usd: (if . < 0 then "-" else "" end) + "$" + ((fabs / 100 | floor) | tostring) + "." + ((fabs % 100) | tostring | if length < 2 then "0" + . else . end); def usd_micros: (if . < 0 then -1 else 1 end) * ((fabs + 5000) / 10000 | floor) | usd; def pct1: (if . < 0 then "-" else "" end) + ((fabs + 5) / 10 | floor | ((. / 10 | floor | tostring) + "." + (. % 10 | tostring))) + "%"; '


# req METHOD PATH [JSON_BODY] — curl with auth, fail-fast on non-2xx,
# echo the response body.
req() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -X "$method" "$BASE$path" -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" -w '\n%{http_code}')
  [ -n "$body" ] && args+=(-d "$body")
  local out status
  out=$(curl "${args[@]}")
  status=$(echo "$out" | tail -1)
  out=$(echo "$out" | sed '$d')
  if [ "${status:0:1}" != "2" ]; then
    echo "" >&2
    echo "✗ $method $path → HTTP $status" >&2
    echo "$out" | jq . >&2 2>/dev/null || echo "$out" >&2
    exit 1
  fi
  echo "$out"
}

step "1. Health check"
curl -sf "$BASE/health" >/dev/null || { echo "✗ API not reachable at $BASE — run 'make dev' first"; exit 1; }
green "✓ API is up at ${VELOX_PUBLIC_URL:-$BASE}"

step "2. Install Anthropic-style pricing (one call)"
# One recipe = the tokens meter + a per-{model, token_type} price matrix
# (input / output / cache_read, sub-cent decimal rates) + a monthly plan.
# Rerun-friendly: apply is idempotent (ADR-085) — a re-run returns the
# already-installed instance with 2xx, never a 409.
INST_RAW=$(curl -sS -X POST "$BASE/v1/recipes/anthropic_style/instantiate" \
  -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" -d '{}' -w '\n%{http_code}')
INST_STATUS=$(echo "$INST_RAW" | tail -1)
INST=$(echo "$INST_RAW" | sed '$d')
if [ "${INST_STATUS:0:1}" = "2" ]; then
  PLAN_ID=$(echo "$INST" | jq -r '.created_objects.plan_ids[0]')
else
  echo "✗ recipe instantiate → HTTP $INST_STATUS"; echo "$INST" | jq . 2>/dev/null || echo "$INST"; exit 1
fi
[ -n "$PLAN_ID" ] && [ "$PLAN_ID" != "null" ] || { echo "✗ no plan found for the recipe"; exit 1; }
green "✓ Recipe live: tokens meter + per-model rates + plan $PLAN_ID"

step "3. Test clock + customer"
# The clock lets us simulate a whole billing month in seconds.
NOW=$(date -u +%Y-%m-%dT%H:%M:%SZ)
# RUN makes this run's ids unique: customer external_id, subscription code
# and LiteLLM call ids are unique per tenant, and a rerun must not collide.
# Full UTC date and time plus a random suffix: a time of day alone repeats
# the next day, and the PID alone repeats in a fresh container.
RUN="$(date -u +%Y%m%d%H%M%S)-$RANDOM"
CLOCK_ID=$(req POST "/v1/test-clocks" "{\"name\": \"demo\", \"frozen_time\": \"$NOW\"}" | jq -r '.id')
CUST=$(req POST "/v1/customers" "{
  \"external_id\": \"acme_$RUN\",
  \"display_name\": \"ACME Corp\",
  \"email\": \"billing@acme.com\",
  \"test_clock_id\": \"$CLOCK_ID\"
}")
CUST_ID=$(echo "$CUST" | jq -r '.id')
[ "$CUST_ID" != "null" ] || { echo "✗ customer create failed"; echo "$CUST" | jq .; exit 1; }
green "✓ Customer $CUST_ID pinned to test clock $CLOCK_ID"

step "4. Subscribe ACME to the plan"
SUB=$(req POST "/v1/subscriptions" "{
  \"code\": \"acme-tokens-$RUN\",
  \"display_name\": \"ACME — usage\",
  \"customer_id\": \"$CUST_ID\",
  \"items\": [{\"plan_id\": \"$PLAN_ID\", \"quantity\": 1}],
  \"billing_time\": \"anniversary\",
  \"start_now\": true
}")
SUB_ID=$(echo "$SUB" | jq -r '.id')
[ "$SUB_ID" != "null" ] || { echo "✗ subscription create failed"; echo "$SUB" | jq .; exit 1; }
green "✓ Subscription $SUB_ID, status $(echo "$SUB" | jq -r '.status')"

step "5. Tell Velox what YOU pay Anthropic (provider cost table)"
for rate in \
  '{"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "input",      "cost_per_token": "0.0000012"}' \
  '{"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "output",     "cost_per_token": "0.0000045"}' \
  '{"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "cache_read", "cost_per_token": "0.00000005"}' ; do
  req PUT "/v1/provider-costs" "$rate" >/dev/null
done
green "✓ 3 provider rates saved — every new event gets its COGS stamped at ingest"

step "6. Usage arrives straight from a LiteLLM proxy (no SDK)"
# One LiteLLM StandardLoggingPayload = one API call by ACME's end user.
# Velox splits it into input / cache_read / output token events with
# {model, token_type} dimensions and dedupes replays by call id.
for i in 1 2 3 4 5; do
  req POST "/v1/integrations/litellm/spend" "{
    \"id\": \"chatcmpl-$RUN-$i\",
    \"call_type\": \"completion\",
    \"model\": \"claude-sonnet-4-5-20250929\",
    \"custom_llm_provider\": \"anthropic\",
    \"user\": \"acme_$RUN\",
    \"usage\": {
      \"prompt_tokens\": 120000,
      \"completion_tokens\": 35000,
      \"cache_read_input_tokens\": 40000
    }
  }" >/dev/null
done
EVENTS=$(req GET "/v1/usage-events?customer_id=$CUST_ID&limit=100" | jq '.data | length')
[ "$EVENTS" -ge 15 ] || { echo "✗ expected ≥15 token events (5 calls × 3 roles), got $EVENTS"; exit 1; }
green "✓ 5 proxy calls → $EVENTS dimensioned token events (input / cache_read / output per call)"

step "7. Simulate a month: advance the test clock past period end"
ADV_TO=$(date -u -v+32d +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "+32 days" +%Y-%m-%dT%H:%M:%SZ)
req POST "/v1/test-clocks/$CLOCK_ID/advance" "{\"frozen_time\": \"$ADV_TO\"}" >/dev/null
# The advance runs billing catchup; poll the clock back to ready.
STATUS=""
for i in $(seq 1 30); do
  STATUS=$(req GET "/v1/test-clocks/$CLOCK_ID" | jq -r '.status')
  [ "$STATUS" = "ready" ] && break
  sleep 1
done
[ "$STATUS" = "ready" ] || { echo "✗ clock stuck in status '$STATUS'"; exit 1; }
green "✓ Clock at $ADV_TO — billing ran during the advance"

step "8. The invoice"
INVOICES=$(req GET "/v1/invoices?customer_id=$CUST_ID")
INV_ID=$(echo "$INVOICES" | jq -r '.data[0].id // empty')
[ -n "$INV_ID" ] || { echo "✗ no invoice generated"; echo "$INVOICES" | jq .; exit 1; }
INV=$(req GET "/v1/invoices/$INV_ID")
echo "$INV" | jq -r "$USD"'.invoice | "  \(.invoice_number)  total \(.total_amount_cents | usd) \(.currency)  status \(.status)"'
echo "$INV" | jq -r "$USD"'.line_items[] | "    \(.description)  → \(.amount_cents | usd)"' | head -8
# The numbers are checked, not just printed: a draft invoice, a missing line or
# a subtotal that disagrees with its own lines must fail the demo.
echo "$INV" | jq -e '.invoice.status == "finalized" or .invoice.status == "paid"' >/dev/null \
  || { echo "✗ invoice is not finalized (status $(echo "$INV" | jq -r '.invoice.status'))"; exit 1; }
echo "$INV" | jq -e '(.line_items | length) >= 3' >/dev/null \
  || { echo "✗ expected at least 3 invoice lines, got $(echo "$INV" | jq '.line_items | length')"; exit 1; }
echo "$INV" | jq -e '.invoice.subtotal_cents > 0 and .invoice.subtotal_cents == ([.line_items[].amount_cents] | add)' >/dev/null \
  || { echo "✗ subtotal $(echo "$INV" | jq '.invoice.subtotal_cents') does not equal the sum of its lines $(echo "$INV" | jq '[.line_items[].amount_cents] | add')"; exit 1; }
SUBTOTAL=$(echo "$INV" | jq '.invoice.subtotal_cents')
PDF_BYTES=$(curl -sf "$BASE/v1/invoices/$INV_ID/pdf" -H "Authorization: Bearer $API_KEY" | wc -c | tr -d ' ')
[ "$PDF_BYTES" -gt 5000 ] || { echo "✗ PDF looks empty ($PDF_BYTES bytes)"; exit 1; }
green "✓ Invoice with per-(model, token_type) lines — PDF renders ($PDF_BYTES bytes)"

step "9. Margin: what you billed against what the provider cost you"
MARGIN=$(req GET "/v1/customers/$CUST_ID/margin?from=$NOW&to=$ADV_TO")
echo "$MARGIN" | jq -r "$USD"'"  billed  \(.revenue_cents | usd)\n  cost    \(.cost_micros | usd_micros)\n  margin  \((.margin_bps // 0) | pct1)"'
echo "$MARGIN" | jq -r "$USD"'.by_model[] | "    \(.model): cost \(.cost_micros | usd_micros)" + (if .attributed then "  billed \((.revenue_cents // 0) | usd)" else "" end)'
# The cost follows from this script's own inputs (the token counts in step 6
# and the rates in step 5), and the billed amount must match the invoice.
echo "$MARGIN" | jq -e '.cost_micros == 1277500' >/dev/null \
  || { echo "✗ provider cost is $(echo "$MARGIN" | jq '.cost_micros') micros, expected 1277500"; exit 1; }
echo "$MARGIN" | jq -e --argjson s "$SUBTOTAL" '.revenue_cents == $s' >/dev/null \
  || { echo "✗ margin says billed $(echo "$MARGIN" | jq '.revenue_cents') cents but the invoice subtotal is $SUBTOTAL"; exit 1; }
green "✓ Per-customer margin from stamped COGS vs rated revenue"

echo ""
blue "━━━ Demo complete — everything above actually ran ━━━"
echo ""
echo "  ✓ One-call Anthropic-style price matrix (recipe)"
echo "  ✓ LiteLLM proxy → dimensioned token events, no SDK"
echo "  ✓ Provider cost table → COGS stamped per event at ingest"
echo "  ✓ Simulated month via test clock → real invoice + PDF"
echo "  ✓ Per-customer margin (billed vs provider cost, by model)"
echo ""
DASH="${VELOX_DASHBOARD_URL:-http://localhost:5173}"
echo "  See it in the dashboard (it opens in test mode, where the demo ran):"
echo "    Invoice:  $DASH/invoices/$INV_ID"
echo "    Customer: $DASH/customers/$CUST_ID"
echo ""
echo "  The home page totals leave out customers on a test clock, so they"
echo "  still read \$0 after the demo. Open the links above instead."
