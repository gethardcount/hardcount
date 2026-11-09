#!/usr/bin/env bash
#
# quickstart-own-install.sh — checks that deploy/compose/quickstart.sh acts
# only on an install it made, and only on its own Compose project, never on
# one that a quick start in another folder holds. It also checks that the
# script takes its settings only from its .env, never from the shell.
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
# The checks quickstart.sh makes before it acts are not recorded. Two of them
# list what Docker holds for project hardcount-quickstart: the folders its
# containers were started from (FAKE_OWNER_DIRS) and its volumes
# (FAKE_VOLUMES). Both are empty unless a check sets them.
case "$*" in
  "compose version" | info) exit 0 ;;
  'ps -a --filter label=com.docker.compose.project=hardcount-quickstart --format {{.Label "com.docker.compose.project.working_dir"}}')
    [ -z "${FAKE_OWNER_DIRS:-}" ] || printf '%s\n' "$FAKE_OWNER_DIRS"
    exit 0 ;;
  'volume ls -q --filter label=com.docker.compose.project=hardcount-quickstart')
    [ -z "${FAKE_VOLUMES:-}" ] || printf '%s\n' "$FAKE_VOLUMES"
    exit 0 ;;
esac
printf '%s\n' "$*" >> "$FAKE_DOCKER_CALLS"
# A setting exported in the shell wins over the .env when Compose reads it.
# Record each one that reaches a Compose call.
if [ "$1" = compose ]; then
  for name in NGINX_HTTP_PORT HARDCOUNT_ENCRYPTION_KEY APP_ENV SMTP_HOST \
      COMPOSE_FILE COMPOSE_ENV_FILES COMPOSE_PROFILES; do
    if [ -n "${!name+set}" ]; then printf '%s\n' "$name" >> "$FAKE_DOCKER_LEAKS"; fi
  done
fi
case "$*" in
  *"/v1/bootstrap"*) echo '{"owner_email":"owner@example.com","owner_password":"pw","secret_key_test":"hc_secret_test_x"}' ;;
  *" ps "*) echo 0123abcd ;;
  inspect*) echo hardcount-quickstart_default ;;
esac
EOF
chmod +x "$work/bin/docker"
leaks="$work/docker-leaks"
export PATH="$work/bin:$PATH" FAKE_DOCKER_CALLS="$calls" FAKE_DOCKER_LEAKS="$leaks"
unset COMPOSE_PROJECT_NAME HARDCOUNT_PORT FAKE_OWNER_DIRS FAKE_VOLUMES

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
  : > "$leaks"
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

# Another folder's quick start has the project. Every quick start names
# hardcount-quickstart, so a second clone or worktree would share its
# database: a start here would run it with this folder's secrets, and
# destroy here would delete it.
rm -f "$dir/.env" "$dir/quickstart-credentials.json"
qs ""
cp "$dir/.env" "$work/own.env"
other="$work/other/deploy/compose"
mkdir -p "$other"
export FAKE_OWNER_DIRS="$other"
for args in "" demo stop destroy "destroy --yes"; do
  # shellcheck disable=SC2086 # split "destroy --yes" into two arguments
  qs $'hardcount-quickstart\n' $args
  check "other folder's project: './quickstart.sh $args' refuses" test "$status" -ne 0
  check "other folder's project: './quickstart.sh $args' names that folder" said "$other"
  check "other folder's project: './quickstart.sh $args' runs no docker command" no_calls
  check "other folder's project: './quickstart.sh $args' keeps the .env" cmp -s "$dir/.env" "$work/own.env"
done
rm -f "$dir/.env" "$dir/quickstart-credentials.json"
qs ""
check "other folder's project: a fresh start refuses" test "$status" -ne 0
check "other folder's project: a fresh start runs no docker command" no_calls
check "other folder's project: a fresh start writes no .env" test ! -e "$dir/.env"
export FAKE_OWNER_DIRS="$work/gone/deploy/compose"
qs ""
check "a deleted folder's project: a fresh start refuses" test "$status" -ne 0
check "a deleted folder's project: says how to delete what is left" said "docker compose -p hardcount-quickstart down -v"

# This folder's own containers, labelled with the path as typed, which can go
# through a symlink.
ln -s "$dir" "$work/link"
export FAKE_OWNER_DIRS="$dir"$'\n'"$work/link"
qs ""
check "own project, reached through a symlink: start succeeds" test "$status" -eq 0
unset FAKE_OWNER_DIRS

# Docker still holds the project's data, but this folder has no .env for it:
# its containers were removed, or its .env deleted. New secrets cannot open
# that database.
export FAKE_VOLUMES=hardcount-quickstart_pgdata
qs ""
check "own .env, own volume: start succeeds" test "$status" -eq 0
rm -f "$dir/.env" "$dir/quickstart-credentials.json"
qs ""
check "a volume and no .env: a fresh start refuses" test "$status" -ne 0
check "a volume and no .env: says how to delete it" said "docker compose -p hardcount-quickstart down -v"
check "a volume and no .env: runs no docker command" no_calls
check "a volume and no .env: writes no .env" test ! -e "$dir/.env"
unset FAKE_VOLUMES

# The shell of an operator who ran `set -a; . ./.env` for a server install
# holds that server's port, mode and secrets. Compose takes a shell value
# over the .env, so none may reach it.
export NGINX_HTTP_PORT=80 HARDCOUNT_ENCRYPTION_KEY=from-the-shell APP_ENV=production \
  SMTP_HOST=smtp.example.com COMPOSE_FILE=/elsewhere/docker-compose.yml \
  COMPOSE_ENV_FILES=/elsewhere/.env COMPOSE_PROFILES=elsewhere
qs ""
check "server settings in the shell: start succeeds" test "$status" -eq 0
check "server settings in the shell: the .env still says 127.0.0.1:8080" grep -qx 'NGINX_HTTP_PORT=127.0.0.1:8080' "$dir/.env"
check "server settings in the shell: none reaches Compose" test ! -s "$leaks"
qs "" stop
check "server settings in the shell: none reaches Compose on stop" test ! -s "$leaks"
unset NGINX_HTTP_PORT HARDCOUNT_ENCRYPTION_KEY APP_ENV SMTP_HOST COMPOSE_FILE \
  COMPOSE_ENV_FILES COMPOSE_PROFILES

if [ "$failures" -gt 0 ]; then
  printf '%d check(s) failed.\n' "$failures"
  exit 1
fi
echo "All quickstart.sh checks passed."
