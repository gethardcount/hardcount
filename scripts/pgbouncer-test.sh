#!/usr/bin/env bash
# Runs the integration suite with the APP pool behind PgBouncer in transaction
# mode (ADR-114 PR-E). The admin pool (migrations, truncation, row forcing)
# stays direct, as production migrations do.
#
#   scripts/pgbouncer-test.sh                 # picks the mode for your Docker
#   PGB_MODE=ci scripts/pgbouncer-test.sh     # host network, DB at 127.0.0.1
#   PGB_MODE=local scripts/pgbouncer-test.sh  # bridge network, DB at host.docker.internal
#   PGB_PACKAGES="./internal/platform/leader/" scripts/pgbouncer-test.sh
#
# The dev Postgres listens only on 127.0.0.1 (the root docker-compose.yml).
# Local mode reaches it through host.docker.internal, which Docker Desktop
# (macOS, Windows, Linux) connects to this computer's 127.0.0.1. Docker
# Engine on Linux connects that name to the Docker bridge, which reaches no
# port bound to 127.0.0.1, so there the script uses ci mode: on the host
# network, 127.0.0.1 is this computer's. CI sets PGB_MODE=ci.
#
# Any statement that assumes a session — SET without LOCAL, a session
# advisory lock, a prepared statement across transactions — passes on a
# direct connection and can misbehave here. PgBouncer does not reject such a
# statement; the proof is that the whole suite still passes through it.
set -euo pipefail

SCRIPTS="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=SCRIPTDIR/ci/pinned-images.sh
. "$SCRIPTS/ci/pinned-images.sh"

MODE="${PGB_MODE:-}"
if [ -z "$MODE" ]; then
  if [ "$(uname -s)" = Linux ] && [ "$(docker info --format '{{.OperatingSystem}}' 2>/dev/null)" != "Docker Desktop" ]; then
    MODE=ci
  else
    MODE=local
  fi
  echo "PGB_MODE not set: using $MODE mode (set PGB_MODE=ci or PGB_MODE=local to choose)"
fi
IMAGE="${PGB_IMAGE:-$PGBOUNCER_IMAGE}"
NAME="hardcount-pgbouncer-test"
PKGS="${PGB_PACKAGES:-./...}"
DIR="$SCRIPTS/pgbouncer"
CONF="$(mktemp -d)"

# LISTEN_ADDR is where PgBouncer listens inside its container. On the host
# network that is this computer, and the test credentials are in the repo,
# so it is 127.0.0.1. On the bridge network Docker forwards the published
# 127.0.0.1:6432 to the container's own address, so it is 0.0.0.0.
if [ "$MODE" = "ci" ]; then
  DB_HOST=127.0.0.1; LISTEN_ADDR=127.0.0.1; NETFLAGS=(--network host)
else
  LISTEN_ADDR=0.0.0.0
  # PgBouncer resolves hosts with its own resolver (evdns), which does not
  # see Docker's magic host.docker.internal name — hand it the IP.
  DB_HOST=$(docker run --rm --add-host=host.docker.internal:host-gateway alpine:3 getent ahostsv4 host.docker.internal | awk '{print $1}' | head -1)
  [ -n "$DB_HOST" ] || { echo "could not resolve the Docker host IP"; exit 1; }
  NETFLAGS=(-p 127.0.0.1:6432:6432 --add-host=host.docker.internal:host-gateway)
fi
sed -e "s/@DB_HOST@/${DB_HOST}/g" -e "s/@LISTEN_ADDR@/${LISTEN_ADDR}/g" "$DIR/pgbouncer.ini.tmpl" > "$CONF/pgbouncer.ini"
cp "$DIR/userlist.txt" "$CONF/userlist.txt"
# The image's entrypoint runs as an unprivileged user and touches the
# userlist; a mktemp dir is mode 700 and root-owned on a Linux runner, so
# open it up (test-only, throwaway) and mount it read-write.
chmod 777 "$CONF" && chmod 666 "$CONF"/*

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; rm -rf "$CONF"; }
trap cleanup EXIT
docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" "${NETFLAGS[@]}" -v "$CONF:/etc/pgbouncer" "$IMAGE" >/dev/null

# Each probe is bounded (perl alarm — portable): a client of a PgBouncer
# whose backend is unreachable otherwise waits query_wait_timeout (120 s).
probe() { PGPASSWORD=hardcount_test_app perl -e 'alarm 5; exec @ARGV' psql -h localhost -p 6432 -U hardcount_test_app -d hardcount_test -A -t -c "SELECT 1" >/dev/null 2>&1; }
for _ in $(seq 1 12); do
  if probe; then break; fi
  sleep 1
done
probe || { echo "pgbouncer not reachable"; docker logs "$NAME" 2>&1 | tail -20; exit 1; }
echo "pgbouncer up (transaction mode) -> ${DB_HOST}:5432; app pool via localhost:6432"

export TEST_ADMIN_DATABASE_URL="${TEST_ADMIN_DATABASE_URL:-postgres://hardcount:hardcount@localhost:5432/hardcount_test?sslmode=disable}"
export TEST_DATABASE_URL="postgres://hardcount_test_app:hardcount_test_app@localhost:6432/hardcount_test?sslmode=disable"
# PKGS stays unquoted: PGB_PACKAGES may name several packages.
# shellcheck disable=SC2086
go test -p 1 $PKGS -count=1 -short=false
