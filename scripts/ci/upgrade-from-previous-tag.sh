#!/usr/bin/env bash
# The upgrade CI job (ADR-125, amendment 2026-10-30). Its invariant: an
# upgrade from the previous release leaves every invoice not on a test clock
# exactly as the API returned it, and the upgraded database still bills.
# Upgrading deletes test clocks and everything on one (00241), so an invoice
# on a clock must be gone.
#
#   1. Build the previous release from its own source: the newest v* tag
#      that HEAD contains, other than a tag on HEAD itself. No registry is
#      involved, so the job does not depend on GHCR.
#   2. Install it as Compose does, on an empty Postgres: the server starts
#      with RUN_MIGRATIONS_ON_BOOT=true, then its hardcount-bootstrap makes a
#      tenant, then its scripts/demo.sh bills, through the public API. It
#      also bills a customer on no clock through its API: a subscription,
#      one usage event, then a cancel, which issues the final invoice.
#   3. Snapshot every invoice not on a clock, and its lines, through the API,
#      in both modes, and note the invoices on a clock.
#   4. Stop it, start HEAD on the same database (its migrations run as it
#      starts), snapshot again and compare. Any difference fails, printing
#      the diff. A field HEAD adds passes only when
#      scripts/ci/upgrade-new-fields.txt lists it, and a field HEAD removes
#      when scripts/ci/upgrade-removed-fields.txt does. Each invoice that was
#      on a clock must answer 404.
#   5. On the upgraded database HEAD renders each old invoice's PDF, bills a
#      new month with its own scripts/demo.sh, leaves the old invoices
#      unchanged while doing it, and hardcount-doctor finds no broken money
#      invariant.
#
# Needs Docker, Go, git, curl and jq on the host. It starts its own Postgres
# container, so it runs the same on a laptop as in CI.
#   PREV_TAG      the release to upgrade from (default: as in step 1)
#   KEEP_OUT      a directory to keep the outputs in (CI uploads it on failure)
#   UPGRADE_PORT  the port the API listens on (default 18080, clear of make dev)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=pinned-images.sh
. "$ROOT/scripts/ci/pinned-images.sh"
WORK="$(mktemp -d)"
OUT="${KEEP_OUT:-$WORK/out}"
mkdir -p "$OUT"
PORT="${UPGRADE_PORT:-18080}"
BASE="http://127.0.0.1:$PORT"
NEW_FIELDS="$ROOT/scripts/ci/upgrade-new-fields.txt"
REMOVED_FIELDS="$ROOT/scripts/ci/upgrade-removed-fields.txt"
PG_NAME="hc-upgrade-$$"
PREV_SRC="$WORK/prev-src"
API_PID=""
WORKTREE_ADDED=""

cleanup() {
  if [ -n "$API_PID" ]; then kill -9 "$API_PID" >/dev/null 2>&1 || true; fi
  docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
  if [ -n "$WORKTREE_ADDED" ]; then git -C "$ROOT" worktree remove --force "$PREV_SRC" >/dev/null 2>&1 || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

for tool in docker go git curl jq; do
  command -v "$tool" >/dev/null || fail "this script needs $tool"
done

# fields FILE SCOPE: the JSON array of the SCOPE field names FILE lists, one
# "<scope> <field>" per line. A malformed line fails.
fields() {
  local out=""
  while read -r scope field rest || [ -n "$scope" ]; do
    case "$scope" in ''|'#'*) continue ;; esac
    [ -n "$field" ] && [ -z "$rest" ] || fail "$1: want '<scope> <field>' with the comment on the line above, got '$scope $field $rest'"
    case "$scope" in
      invoice|line_item) [ "$scope" = "$2" ] && out+="$field"$'\n' ;;
      *) fail "$1: unknown scope '$scope' (want invoice or line_item)" ;;
    esac
  done < "$1"
  printf '%s' "$out" | jq -R . | jq -s -c .
}

# The two lists, read before anything starts so a malformed line fails first.
INV_NEW=$(fields "$NEW_FIELDS" invoice)
LINE_NEW=$(fields "$NEW_FIELDS" line_item)
INV_REMOVED=$(fields "$REMOVED_FIELDS" invoice)
LINE_REMOVED=$(fields "$REMOVED_FIELDS" line_item)

# On a tag push the newest tag is the release being built, so tags on HEAD
# are skipped. --merged keeps the choice to releases this code grew from.
if [ -z "${PREV_TAG:-}" ]; then
  on_head=$(git -C "$ROOT" tag --points-at HEAD 'v*')
  PREV_TAG=$(git -C "$ROOT" tag --merged HEAD --sort=-v:refname 'v*' | grep -vxF -e "${on_head:-}" | head -1 || true)
fi
[ -n "$PREV_TAG" ] || fail "no earlier v* tag found; CI must check out with tags (fetch-depth: 0)"

say "build $PREV_TAG (the previous release) and HEAD"
git -C "$ROOT" worktree add --detach --quiet "$PREV_SRC" "$PREV_TAG"
WORKTREE_ADDED=1
mkdir -p "$WORK/prev" "$WORK/head"
(cd "$PREV_SRC" && go build -o "$WORK/prev/" ./cmd/hardcount ./cmd/hardcount-bootstrap)
(cd "$ROOT" && go build -o "$WORK/head/" ./cmd/hardcount ./cmd/hardcount-doctor)
echo "previous release: $PREV_TAG ($(git -C "$ROOT" rev-parse --short "$PREV_TAG^{commit}")); HEAD: $(git -C "$ROOT" rev-parse --short HEAD)"

# The previous release's own Compose init makes hardcount_app, as an install
# of that release would have. The server then runs under RLS, as it does in
# production, instead of falling back to the owner connection.
say "start Postgres with $PREV_TAG's Compose role setup"
docker pull -q "$POSTGRES16_IMAGE" >/dev/null
docker run -d --name "$PG_NAME" -p "127.0.0.1::5432" \
  -e POSTGRES_USER=hardcount -e POSTGRES_PASSWORD=hardcount -e POSTGRES_DB=hardcount \
  -e HARDCOUNT_APP_DB_PASSWORD=hardcount-app-ci \
  -v "$PREV_SRC/deploy/compose/postgres-init.sh:/docker-entrypoint-initdb.d/00-init.sh:ro" \
  "$POSTGRES16_IMAGE" >/dev/null
PG_PORT=$(docker port "$PG_NAME" 5432/tcp | head -1)
PG_PORT=${PG_PORT##*:}
[ -n "$PG_PORT" ] || fail "Postgres has no host port"
# -h 127.0.0.1: the image's init runs on a server without TCP, so this
# waits for the real server, after the init script.
for _ in $(seq 1 60); do
  docker exec "$PG_NAME" pg_isready -q -h 127.0.0.1 -U hardcount && break
  sleep 1
done
docker exec "$PG_NAME" pg_isready -q -h 127.0.0.1 -U hardcount || fail "Postgres did not start"
ADMIN_URL="postgres://hardcount:hardcount@127.0.0.1:$PG_PORT/hardcount?sslmode=disable"
APP_URL="postgres://hardcount_app:hardcount-app-ci@127.0.0.1:$PG_PORT/hardcount?sslmode=disable"

# start_api LABEL BIN: starts a server as Compose does and waits until it
# answers. It runs in $WORK so a developer's .env cannot reach it.
start_api() {
  local label=$1 bin=$2 log="$OUT/$1-server.log"
  (cd "$WORK" && DATABASE_URL="$ADMIN_URL" APP_DATABASE_URL="$APP_URL" PORT="$PORT" \
    RUN_MIGRATIONS_ON_BOOT=true exec "$bin" > "$log" 2>&1) &
  API_PID=$!
  for _ in $(seq 1 120); do
    curl -sf "$BASE/health" >/dev/null 2>&1 && break
    kill -0 "$API_PID" 2>/dev/null || { tail -50 "$log"; fail "$label server exited while starting"; }
    sleep 1
  done
  curl -sf "$BASE/health" >/dev/null || { tail -50 "$log"; fail "$label server never became healthy"; }
  grep -q 'using app database connection (RLS enforced)' "$log" \
    || { tail -50 "$log"; fail "$label server is not on the app connection: row-level security is off"; }
  echo "$label server is up (log: $log)"
}

# stop_api LABEL: SIGTERM, the way Compose stops a release before the next
# one starts. 120s covers the server's shutdown budget (90s plus the drain).
stop_api() {
  local label=$1 status=0
  kill -TERM "$API_PID"
  for _ in $(seq 1 120); do
    kill -0 "$API_PID" 2>/dev/null || break
    sleep 1
  done
  if kill -0 "$API_PID" 2>/dev/null; then
    tail -50 "$OUT/$label-server.log"
    fail "$label server did not stop within 120s of SIGTERM"
  fi
  wait "$API_PID" || status=$?
  API_PID=""
  echo "$label server stopped (exit $status)"
}

# post KEY PATH [BODY]: a POST with the key, and the JSON body if one is
# given; non-2xx fails with the body.
post() {
  local out status args=(-sS -X POST "$BASE$2" -H "Authorization: Bearer $1" -w '\n%{http_code}')
  [ -n "${3:-}" ] && args+=(-H "Content-Type: application/json" -d "$3")
  out=$(curl "${args[@]}")
  status=${out##*$'\n'}
  out=${out%$'\n'*}
  [ "${status:0:1}" = 2 ] || { echo "$out" >&2; fail "POST $2 answered HTTP $status"; }
  printf '%s\n' "$out"
}

# api KEY PATH: a GET with the key; non-2xx fails with the body.
api() {
  local out status
  out=$(curl -sS "$BASE$2" -H "Authorization: Bearer $1" -w '\n%{http_code}')
  status=${out##*$'\n'}
  out=${out%$'\n'*}
  [ "${status:0:1}" = 2 ] || { echo "$out" >&2; fail "GET $2 answered HTTP $status"; }
  printf '%s\n' "$out"
}

# snapshot LABEL: every invoice not on a test clock that the API lists, in
# each mode, as one JSON object keyed by "<mode> <invoice id>". The lines are
# keyed by their id, so the comparison does not depend on the order they
# come back in. The raw file keeps every invoice, those on a clock too.
#
# Two fields are left out of both sides, because the API works them out on
# each read from the moment and the server's settings, not from the invoice
# (domain.InvoiceWebhookData leaves them out of webhooks for the same
# reason):
#   attention         the dashboard notice: it reads whether Stripe and SMTP
#                     are set up, the clock and the dunning run, and its
#                     wording is the dashboard's, which releases change.
#   recovery_warning  the notice before recording a payment against a
#                     written-off invoice, read the same way.
# Every stored field stays in, timestamps included: an upgrade must not
# touch an invoice row at all, so updated_at must not move either.
snapshot() {
  local label=$1 mode key offset page n total ids="$WORK/$1-ids"
  : > "$OUT/$label-raw.jsonl"
  for mode in test live; do
    key=$TEST_KEY
    [ "$mode" = live ] && key=$LIVE_KEY
    : > "$ids"
    offset=0
    while :; do
      page=$(api "$key" "/v1/invoices?limit=100&offset=$offset")
      jq -r '.data[].id' <<< "$page" >> "$ids"
      n=$(jq '.data | length' <<< "$page")
      total=$(jq '.total' <<< "$page")
      [ "$n" -lt 100 ] && break
      offset=$((offset + 100))
    done
    [ "$(wc -l < "$ids" | tr -d ' ')" = "$total" ] || fail "$label: listed $(wc -l < "$ids") $mode invoices, but the list's total is $total"
    while read -r id; do
      api "$key" "/v1/invoices/$id" | jq -c --arg mode "$mode" '{mode: $mode} + .'
    done < "$ids" >> "$OUT/$label-raw.jsonl"
  done
  jq -S -s '
    map(select((.invoice.test_clock_id // "") == ""))
    | map({key: "\(.mode) \(.invoice.id)",
         value: {invoice: (.invoice | del(.attention, .recovery_warning)),
                 line_items: (.line_items | map({key: .id, value: .}) | from_entries)}})
    | from_entries' "$OUT/$label-raw.jsonl" > "$OUT/$label.json"
  echo "$label: $(jq 'length' "$OUT/$label.json") invoices, $(jq '[.[].line_items | length] | add // 0' "$OUT/$label.json") lines"
}

# compare LABEL ONLY_OLD: the snapshot LABEL against the one taken before
# the upgrade. A listed new field is dropped from HEAD's side only where the
# old invoice or line did not have it, so a field both sides return is still
# compared; a listed removed field is dropped from the before side.
# ONLY_OLD=true leaves out invoices made after the upgrade.
compare() {
  local label=$1 only_old=$2
  jq -S --argjson inv "$INV_REMOVED" --argjson line "$LINE_REMOVED" '
    map_values(.invoice |= (reduce $inv[] as $f (.; del(.[$f])))
      | .line_items |= map_values(reduce $line[] as $f (.; del(.[$f]))))
  ' "$OUT/before.json" > "$OUT/before-compared.json"
  jq -S --slurpfile old "$OUT/before.json" --argjson inv "$INV_NEW" \
      --argjson line "$LINE_NEW" --argjson only_old "$only_old" '
    def drop_new($was; $fields):
      reduce $fields[] as $f (.; if ($was | type) == "object" and ($was | has($f)) then . else del(.[$f]) end);
    $old[0] as $o
    | with_entries(select(($only_old | not) or (.key as $k | $o | has($k))))
    | with_entries(.key as $k | .value |= (
        .invoice |= drop_new($o[$k].invoice; $inv)
        | .line_items |= with_entries(.key as $l | .value |= drop_new($o[$k].line_items[$l]; $line))))
  ' "$OUT/$label.json" > "$OUT/$label-compared.json"
  if ! diff -u "$OUT/before-compared.json" "$OUT/$label-compared.json" > "$OUT/$label.diff"; then
    head -300 "$OUT/$label.diff"
    echo
    echo "An invoice that existed before the upgrade reads differently $label (- before, + after)."
    echo "A changed amount, line, status, number or tax is a money finding: fix the upgrade, do not list it."
    echo "A field HEAD adds on purpose goes in scripts/ci/upgrade-new-fields.txt, with why;"
    echo "one it removes on purpose goes in scripts/ci/upgrade-removed-fields.txt."
    fail "existing invoices changed $label"
  fi
  echo "$label: every invoice from before the upgrade is unchanged"
}

say "install $PREV_TAG and bill a month with its demo"
start_api prev "$WORK/prev/hardcount"
# Its output stays in $WORK, not $OUT: it holds the keys and the owner's
# password, and CI uploads $OUT.
(cd "$WORK" && DATABASE_URL="$ADMIN_URL" "$WORK/prev/hardcount-bootstrap" > "$WORK/bootstrap.out" 2>&1) \
  || { tail -20 "$WORK/bootstrap.out"; fail "$PREV_TAG's hardcount-bootstrap failed"; }
TEST_KEY=$(grep -oE 'hc_secret_test_[a-f0-9]+' "$WORK/bootstrap.out" | head -1 || true)
LIVE_KEY=$(grep -oE 'hc_secret_live_[a-f0-9]+' "$WORK/bootstrap.out" | head -1 || true)
[ -n "$TEST_KEY" ] && [ -n "$LIVE_KEY" ] || fail "$PREV_TAG's hardcount-bootstrap printed no test and live secret keys"
HARDCOUNT_BASE="$BASE" "$PREV_SRC/scripts/demo.sh" "$TEST_KEY" > "$OUT/prev-demo.txt" 2>&1 \
  || { cat "$OUT/prev-demo.txt"; fail "$PREV_TAG's scripts/demo.sh failed"; }
echo "$PREV_TAG's demo billed"
# A customer on no clock, billed by the previous release on the demo's plan:
# one usage event, then a cancel, which issues the final invoice at once.
PLAN_ID=$(api "$TEST_KEY" "/v1/plans" | jq -r '.data[0].id')
CUST_ID=$(post "$TEST_KEY" "/v1/customers" '{"external_id": "upgrade-real", "display_name": "Upgrade Real", "email": "billing@upgrade.test"}' | jq -r .id)
SUB_ID=$(post "$TEST_KEY" "/v1/subscriptions" "{\"code\": \"upgrade-real\", \"display_name\": \"Upgrade Real\", \"customer_id\": \"$CUST_ID\", \"items\": [{\"plan_id\": \"$PLAN_ID\", \"quantity\": 1}], \"billing_time\": \"anniversary\"}" | jq -r .id)
post "$TEST_KEY" "/v1/usage-events" '{"event_name": "tokens", "external_customer_id": "upgrade-real", "quantity": "1000000", "idempotency_key": "upgrade-real-1", "dimensions": {"provider": "anthropic", "model": "claude-sonnet-4.5", "token_type": "input"}}' > /dev/null
post "$TEST_KEY" "/v1/subscriptions/$SUB_ID/cancel" > /dev/null
echo "$PREV_TAG billed a customer on no clock"

say "snapshot before the upgrade"
snapshot before
[ "$(jq '[.[] | select(.invoice.status == "finalized" or .invoice.status == "paid")] | length' "$OUT/before.json")" -ge 1 ] \
  || fail "no finalized invoice before the upgrade: the comparison would prove nothing"
jq -r 'select((.invoice.test_clock_id // "") != "") | "\(.mode) \(.invoice.id)"' "$OUT/before-raw.jsonl" > "$OUT/clock-invoices.txt"
echo "before: $(wc -l < "$OUT/clock-invoices.txt" | tr -d ' ') invoices on a test clock"
stop_api prev

say "upgrade: HEAD starts on the same database"
DATABASE_URL="$ADMIN_URL" "$WORK/head/hardcount" migrate status | tee "$OUT/status-before-upgrade.txt"
start_api head "$WORK/head/hardcount"
grep 'migration' "$OUT/head-server.log" | head -20 || true
DATABASE_URL="$ADMIN_URL" "$WORK/head/hardcount" migrate status | tee "$OUT/status-after-upgrade.txt"
grep -q 'up to date, nothing to apply' "$OUT/status-after-upgrade.txt" || fail "HEAD started, but the database is not up to date"

say "snapshot after the upgrade"
snapshot after
compare after false
while read -r mode id; do
  k=$TEST_KEY
  [ "$mode" = live ] && k=$LIVE_KEY
  code=$(curl -sS -o /dev/null -w '%{http_code}' "$BASE/v1/invoices/$id" -H "Authorization: Bearer $k")
  [ "$code" = 404 ] || fail "invoice $id was on a test clock and answers HTTP $code after the upgrade, want 404"
done < "$OUT/clock-invoices.txt"
echo "after: every invoice that was on a test clock is gone"

say "HEAD on the upgraded database"
while read -r key; do
  mode=${key%% *} id=${key#* }
  k=$TEST_KEY
  [ "$mode" = live ] && k=$LIVE_KEY
  code=$(curl -sS -o "$WORK/inv.pdf" -w '%{http_code}' "$BASE/v1/invoices/$id/pdf" -H "Authorization: Bearer $k")
  [ "$code" = 200 ] && [ "$(head -c 4 "$WORK/inv.pdf")" = "%PDF" ] || fail "HEAD did not render the PDF of $id (HTTP $code)"
done < <(jq -r 'keys[]' "$OUT/before.json")
echo "HEAD renders the PDF of every invoice from before the upgrade"
HARDCOUNT_BASE="$BASE" "$ROOT/scripts/demo.sh" "$TEST_KEY" > "$OUT/head-demo.txt" 2>&1 \
  || { cat "$OUT/head-demo.txt"; fail "HEAD's scripts/demo.sh failed on the upgraded database"; }
echo "HEAD's demo billed a new month on the upgraded database"
snapshot after-demo
compare after-demo true
DATABASE_URL="$ADMIN_URL" "$WORK/head/hardcount-doctor" | tee "$OUT/doctor.txt" \
  || fail "hardcount-doctor found a broken money invariant on the upgraded database"
stop_api head

say "upgrade from $PREV_TAG: all checks passed in ${SECONDS}s"
