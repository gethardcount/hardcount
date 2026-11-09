#!/usr/bin/env bash
#
# quickstart-own-install.sh — checks that deploy/compose/quickstart.sh acts
# only on an install it made, and only on its own Compose project.
#
# It runs a copy of quickstart.sh with a fake `docker` first on PATH. The fake
# records each call and runs nothing, so this needs no Docker and cannot touch
# a real install on this machine. CI runs it in the lint job.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
dir="$work/compose"
calls="$work/docker-calls"
cp -R "$root/deploy/compose" "$dir"
rm -f "$dir/.env" "$dir/quickstart-credentials.json"

mkdir "$work/bin"
cat > "$work/bin/docker" <<'EOF'
#!/usr/bin/env bash
# The checks quickstart.sh makes before it acts pass and are not recorded.
case "$*" in "compose version" | info) exit 0 ;; esac
printf '%s\n' "$*" >> "$FAKE_DOCKER_CALLS"
case "$*" in
  *"/v1/bootstrap"*) echo '{"owner_email":"owner@example.com","owner_password":"pw","secret_key_test":"hc_secret_test_x"}' ;;
  *" ps "*) echo 0123abcd ;;
  inspect*) echo hardcount-quickstart_default ;;
esac
EOF
chmod +x "$work/bin/docker"
export PATH="$work/bin:$PATH" FAKE_DOCKER_CALLS="$calls"
unset COMPOSE_PROJECT_NAME HARDCOUNT_PORT

failures=0
# check DESCRIPTION COMMAND... runs COMMAND and reports it.
check() {
  if "${@:2}"; then
    printf 'ok    %s\n' "$1"
  else
    printf 'FAIL  %s\n' "$1"
    sed 's/^/        | /' "$work/out"
    failures=$((failures + 1))
  fi
}
# qs INPUT ARGS... runs quickstart.sh in the copy with INPUT on stdin, keeps
# its output in $work/out and its exit status in $status, and starts a new
# call log.
qs() {
  printf '%s' "$1" > "$work/in"
  shift
  : > "$calls"
  status=0
  (cd "$dir" && ./quickstart.sh "$@" < "$work/in") > "$work/out" 2>&1 || status=$?
}
said() { grep -qF -- "$1" "$work/out"; }
no_calls() { [ ! -s "$calls" ]; }
called() { grep -qxF -- "$1" "$calls"; }
# Every Compose call names the project with -p, so Compose never falls back
# to the folder's name.
every_call_names() { grep -q '^compose ' "$calls" && ! grep '^compose ' "$calls" | grep -qv "^compose -p $1 "; }

# A hand-made .env, as Production on one VM makes in this folder.
for args in "" demo stop destroy "destroy --yes"; do
  cp "$dir/.env.example" "$dir/.env"
  rm -f "$dir/quickstart-credentials.json"
  # shellcheck disable=SC2086 # split "destroy --yes" into two arguments
  qs "" $args
  check "hand-made .env: './quickstart.sh $args' refuses" test "$status" -ne 0
  check "hand-made .env: './quickstart.sh $args' says why" said "not written by quickstart.sh"
  check "hand-made .env: './quickstart.sh $args' runs no docker command" no_calls
  check "hand-made .env: './quickstart.sh $args' keeps the .env" cmp -s "$dir/.env" "$dir/.env.example"
done
rm -f "$dir/.env" "$dir/quickstart-credentials.json"

# No .env, as in a fresh clone: nothing to stop or delete.
for args in stop destroy "destroy --yes"; do
  # shellcheck disable=SC2086
  qs "" $args
  check "no .env: './quickstart.sh $args' runs no docker command" no_calls
  check "no .env: './quickstart.sh $args' writes no .env" test ! -e "$dir/.env"
done

# A fresh start: its own .env, 127.0.0.1 only, and its project on every call.
qs ""
check "fresh start succeeds" test "$status" -eq 0
check "fresh start writes NGINX_HTTP_PORT=127.0.0.1:8080" grep -qx 'NGINX_HTTP_PORT=127.0.0.1:8080' "$dir/.env"
check "fresh start prints the dashboard at port 8080" said "http://localhost:8080"
check "fresh start names its project on every Compose call" every_call_names hardcount-quickstart
qs "" demo
check "demo names its project on every Compose call" every_call_names hardcount-quickstart

# destroy asks first.
qs "" destroy
check "destroy with no answer refuses" test "$status" -ne 0
check "destroy with no answer runs no docker command" no_calls
check "destroy with no answer keeps the .env" test -s "$dir/.env"
[ -e "$dir/.env" ] || qs ""
qs $'compose\n' destroy
check "destroy with another name refuses" test "$status" -ne 0
check "destroy with another name runs no docker command" no_calls
[ -e "$dir/.env" ] || qs ""
qs $'hardcount-quickstart\n' destroy
check "destroy with the typed name succeeds" test "$status" -eq 0
check "destroy with the typed name deletes its own project" called "compose -p hardcount-quickstart down -v"
check "destroy with the typed name removes the .env" test ! -e "$dir/.env"
[ -e "$dir/.env" ] || qs ""
qs "" destroy --yes
check "destroy --yes deletes its own project" called "compose -p hardcount-quickstart down -v"
check "destroy --yes removes the .env" test ! -e "$dir/.env"
check "destroy --yes removes the credentials" test ! -e "$dir/quickstart-credentials.json"

# An .env from an earlier quickstart.sh: its project and port, as written.
rm -f "$dir/.env" "$dir/quickstart-credentials.json"
printf '%s\n' "# Written by quickstart.sh on 2026-11-01T00:00:00Z. Keep this file private." \
  "COMPOSE_PROJECT_NAME=hc-qs-earlier" "NGINX_HTTP_PORT=9000" > "$dir/.env"
qs ""
check "earlier .env: start names its project on every Compose call" every_call_names hc-qs-earlier
check "earlier .env: prints the dashboard at its port" said "http://localhost:9000"
qs "" stop
check "earlier .env: stop names its project" called "compose -p hc-qs-earlier stop"

if [ "$failures" -gt 0 ]; then
  printf '%d check(s) failed.\n' "$failures"
  exit 1
fi
echo "All quickstart.sh checks passed."
