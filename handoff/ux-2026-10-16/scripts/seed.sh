#!/bin/bash
# Seed varied customer shapes for the UI walk. Test mode key.
set -u
K=$(cat /tmp/claude-0/run/test.key); B=http://localhost:8080
req(){ local m=$1 p=$2 d=${3:-}; local out; if [ -n "$d" ]; then out=$(curl -sS -X $m "$B$p" -H "Authorization: Bearer $K" -H "Content-Type: application/json" -H "Idempotency-Key: seed-$RANDOM$RANDOM" -d "$d" -w '\n%{http_code}'); else out=$(curl -sS -X $m "$B$p" -H "Authorization: Bearer $K" -w '\n%{http_code}'); fi; local code=$(echo "$out"|tail -1); local body=$(echo "$out"|sed '$d'); if [ "${code:0:1}" != "2" ]; then echo "  ✗ $m $p → $code $(echo "$body"|head -c 300)" >&2; fi; echo "$body"; }
ev(){ req POST /v1/usage-events "{\"external_customer_id\":\"$1\",\"event_name\":\"$2\",\"quantity\":$3,\"idempotency_key\":\"$1-$2-$RANDOM$RANDOM\"$( [ -n "${4:-}" ] && echo ",\"timestamp\":\"$4\"")}" >/dev/null; }
iso(){ date -u -d "$1" +%Y-%m-%dT%H:%M:%SZ; }

echo "== pricing: API calls meter + Pro/Starter plans"
RR=$(req POST /v1/rating-rules '{"rule_key":"api_calls_std","name":"API calls","mode":"flat","currency":"USD","flat_amount_cents":"0.5"}' | jq -r .id)
M=$(req POST /v1/meters "{\"key\":\"api_calls\",\"name\":\"API calls\",\"unit\":\"calls\",\"aggregation\":\"sum\",\"rating_rule_version_id\":\"$RR\"}" | jq -r .id)
RR2=$(req POST /v1/rating-rules '{"rule_key":"storage_gb","name":"Storage","mode":"graduated","currency":"USD","graduated_tiers":[{"up_to":100,"unit_amount_cents":"0"},{"up_to":0,"unit_amount_cents":"10"}]}' | jq -r .id)
M2=$(req POST /v1/meters "{\"key\":\"storage_gb\",\"name\":\"Storage\",\"unit\":\"GB\",\"aggregation\":\"max\",\"rating_rule_version_id\":\"$RR2\"}" | jq -r .id)
PRO=$(req POST /v1/plans "{\"code\":\"pro\",\"name\":\"Pro\",\"currency\":\"USD\",\"billing_interval\":\"monthly\",\"base_amount_cents\":4900,\"meter_ids\":[\"$M\",\"$M2\"]}" | jq -r .id)
STARTER=$(req POST /v1/plans "{\"code\":\"starter\",\"name\":\"Starter\",\"currency\":\"USD\",\"billing_interval\":\"monthly\",\"base_amount_cents\":1900,\"meter_ids\":[\"$M\"]}" | jq -r .id)
YEARLY=$(req POST /v1/plans "{\"code\":\"enterprise-annual\",\"name\":\"Enterprise (annual)\",\"currency\":\"USD\",\"billing_interval\":\"yearly\",\"base_amount_cents\":1200000,\"meter_ids\":[]}" | jq -r .id)
echo "  plans: pro=$PRO starter=$STARTER yearly=$YEARLY"

mkcust(){ req POST /v1/customers "{\"external_id\":\"$1\",\"display_name\":\"$2\",\"email\":\"$3\"$( [ -n "${4:-}" ] && echo ",\"test_clock_id\":\"$4\"")}" | jq -r .id; }
mksub(){ req POST /v1/subscriptions "{\"code\":\"$1\",\"display_name\":\"$2\",\"customer_id\":\"$3\",\"items\":[{\"plan_id\":\"$4\",\"quantity\":1}],\"billing_time\":\"${5:-anniversary}\",\"start_now\":true$( [ -n "${6:-}" ] && echo ",\"trial_days\":$6")}" | jq -r .id; }

echo "== Northwind: live (wall clock), Pro, usage today"
NW=$(mkcust northwind "Northwind Traders" ap@northwind.test); NWS=$(mksub northwind-pro "Northwind — Pro" $NW $PRO)
for h in 1 2 3 4 5; do ev northwind api_calls $((h*1200)); done; ev northwind storage_gb 140

echo "== Globex: 3 months of history on a test clock, 2 subscriptions (Pro anniversary + Starter calendar)"
CLK=$(req POST /v1/test-clocks "{\"name\":\"globex-history\",\"frozen_time\":\"$(iso '-95 days')\"}" | jq -r .id)
GX=$(mkcust globex "Globex Corporation" finance@globex.test $CLK)
GXS=$(mksub globex-pro "Globex — Pro" $GX $PRO anniversary)
adv(){ req POST /v1/test-clocks/$CLK/advance "{\"frozen_time\":\"$1\"}" >/dev/null; for i in $(seq 1 40); do s=$(req GET /v1/test-clocks/$CLK | jq -r .status); [ "$s" = ready ] && break; sleep 1; done; }
ev globex api_calls 42000; ev globex storage_gb 80
adv "$(iso '-80 days')"; GXS2=$(mksub globex-starter "Globex — Starter (calendar)" $GX $STARTER calendar); ev globex api_calls 61000; ev globex storage_gb 120
adv "$(iso '-50 days')"; ev globex api_calls 75000; ev globex storage_gb 210
adv "$(iso '-20 days')"; ev globex api_calls 30000
adv "$(iso '-3 days')"; ev globex api_calls 9000

echo "== Initech: 14-day trial (wall clock)"
IN=$(mkcust initech "Initech" billing@initech.test); INS=$(mksub initech-trial "Initech — Pro trial" $IN $PRO anniversary 14); ev initech api_calls 3000

echo "== Umbrella: canceled subscription"
UM=$(mkcust umbrella "Umbrella Co" ar@umbrella.test); UMS=$(mksub umbrella-starter "Umbrella — Starter" $UM $STARTER); ev umbrella api_calls 5000; req POST /v1/subscriptions/$UMS/cancel '{}' >/dev/null

echo "== Hooli: no subscription, no usage"
HO=$(mkcust hooli "Hooli" pay@hooli.test)

echo "== Stark: annual plan + credits + manual invoice + credit note"
ST=$(mkcust stark "Stark Industries" accounts@stark.test); STS=$(mksub stark-annual "Stark — Enterprise annual" $ST $YEARLY)
req POST /v1/credits/grant "{\"customer_id\":\"$ST\",\"amount_cents\":50000,\"description\":\"Onboarding credit\",\"expires_at\":\"$(iso '+60 days')\"}" >/dev/null
INV=$(req POST /v1/invoices "{\"customer_id\":\"$ST\",\"currency\":\"USD\",\"net_payment_term_days\":15,\"memo\":\"Implementation services\",\"line_items\":[{\"description\":\"Onboarding workshop\",\"line_type\":\"add_on\",\"quantity\":1,\"unit_amount_cents\":250000},{\"description\":\"Custom integration (hours)\",\"line_type\":\"add_on\",\"quantity\":12,\"unit_amount_cents\":18000}]}" | jq -r .id)
req POST /v1/invoices/$INV/finalize '{}' >/dev/null
DR=$(req POST /v1/invoices "{\"customer_id\":\"$NW\",\"currency\":\"USD\",\"memo\":\"Draft add-on\",\"line_items\":[{\"description\":\"Priority support (month)\",\"line_type\":\"add_on\",\"quantity\":1,\"unit_amount_cents\":9900}]}" | jq -r .id)

echo "== Wayne: EUR customer (different currency plan)"
EURP=$(req POST /v1/plans "{\"code\":\"pro-eur\",\"name\":\"Pro (EUR)\",\"currency\":\"EUR\",\"billing_interval\":\"monthly\",\"base_amount_cents\":4500,\"meter_ids\":[]}" | jq -r .id)
WY=$(mkcust wayne "Wayne Enterprises GmbH" rechnung@wayne.test); WYS=$(mksub wayne-eur "Wayne — Pro EUR" $WY $EURP)

echo "== webhook endpoint"
req POST /v1/webhook-endpoints/endpoints '{"url":"https://hooks.example.test/velox","description":"Ops alerts"}' >/dev/null
echo "done"
