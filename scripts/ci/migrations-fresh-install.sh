#!/usr/bin/env bash
# The migrations CI job. Its invariant: HEAD installs on an empty database
# under each role setup Velox documents, and a second run has nothing to do.
#
# For each of three role setups it starts one Postgres container, checks
# that the limited user starts with the idle-transaction timeout
# (docs/ops/postgres-requirements.md, "Timeouts on the limited user"), then
# runs HEAD's `velox migrate` twice. The first run applies migration 180
# (the baseline) and every file after it; the second says up to date.
#
#   compose  superuser velox; velox_app made by deploy/compose/postgres-init.sh
#   managed  velox_owner, not a superuser, owns the database; the three
#            extensions made first by a superuser (docs/ops/postgres-requirements.md)
#   citest   superuser velox; the velox_test database of docker/init.sql
#
# Then two more checks:
#   restore drill  back up a HEAD database with scripts/backup.sh and restore
#                  it into an empty database with scripts/restore.sh: it must
#                  succeed, equal the database backed up, and be up to date.
#   Postgres 14    HEAD applies on postgres:14 and reports up to date.
#
# Needs Docker, psql and pg_restore on the host, and Go to build HEAD.
#   VELOX_BIN   a HEAD build of cmd/velox (default: built into a temp dir)
#   KEEP_OUT    a directory to keep the outputs in (CI uploads it on failure)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=pinned-images.sh
. "$ROOT/scripts/ci/pinned-images.sh"
WORK="$(mktemp -d)"
OUT="${KEEP_OUT:-$WORK/out}"
mkdir -p "$OUT"
CONTAINERS=()

cleanup() {
  for c in "${CONTAINERS[@]}"; do docker rm -f "$c" >/dev/null 2>&1 || true; done
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '\n== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

if [ -z "${VELOX_BIN:-}" ]; then
  VELOX_BIN="$WORK/velox"
  (cd "$ROOT" && go build -o "$VELOX_BIN" ./cmd/velox)
fi

docker pull -q "$POSTGRES16_IMAGE" >/dev/null
docker pull -q "$POSTGRES14_IMAGE" >/dev/null

# The versions HEAD ships: 0 (goose's own row), 180, then every file in sql/.
SHIPPED=$(
  {
    echo 0
    echo 180
    for f in "$ROOT"/internal/platform/migrate/sql/[0-9]*.sql; do
      b=${f##*/}
      echo $((10#${b%%_*}))
    done
  } | sort -n | paste -sd, -
)

# start_pg NAME IMAGE SETUP: starts Postgres for one role setup on a host
# port Docker picks (a fixed port can already be taken on the runner), and
# sets PG_PORT to it. The roles exist when it returns. Callers add NAME to
# CONTAINERS first: this runs in the caller's shell, not a subshell.
start_pg() {
  local name=$1 image=$2 setup=$3
  local args=(-d --name "$name" -p "127.0.0.1::5432")
  case "$setup" in
    compose)
      args+=(-e POSTGRES_USER=velox -e POSTGRES_PASSWORD=velox -e POSTGRES_DB=velox
             -e VELOX_APP_DB_PASSWORD=velox-app-ci
             -v "$ROOT/deploy/compose/postgres-init.sh:/docker-entrypoint-initdb.d/00-init.sh:ro") ;;
    managed)
      args+=(-e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=postgres) ;;
    citest)
      args+=(-e POSTGRES_USER=velox -e POSTGRES_PASSWORD=velox -e POSTGRES_DB=velox) ;;
  esac
  docker run "${args[@]}" "$image" >/dev/null
  PG_PORT=$(docker port "$name" 5432/tcp | head -1)
  PG_PORT=${PG_PORT##*:}
  [ -n "$PG_PORT" ] || fail "$name has no host port"
  local user=velox
  [ "$setup" = managed ] && user=postgres
  for _ in $(seq 1 60); do
    docker exec "$name" pg_isready -q -h 127.0.0.1 -U "$user" >&2 && break
    sleep 1
  done
  docker exec "$name" pg_isready -q -h 127.0.0.1 -U "$user" || fail "$name did not start"
  case "$setup" in
    managed)
      docker exec -i "$name" psql -v ON_ERROR_STOP=1 -q -U postgres -d postgres >&2 <<'SQL'
CREATE ROLE velox_owner WITH LOGIN PASSWORD 'velox_owner' NOSUPERUSER CREATEROLE;
CREATE DATABASE velox OWNER velox_owner;
\connect velox
CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS citext WITH SCHEMA public;
CREATE EXTENSION IF NOT EXISTS btree_gist WITH SCHEMA public;
SQL
      # docs/ops/postgres-requirements.md, "The two database users" and
      # "Timeouts on the limited user", run as the admin user.
      docker exec -i "$name" psql -v ON_ERROR_STOP=1 -q -U velox_owner -d velox >&2 <<'SQL'
CREATE ROLE velox_app WITH LOGIN PASSWORD 'velox-app-ci';
GRANT ALL PRIVILEGES ON DATABASE velox TO velox_app;
GRANT ALL ON SCHEMA public TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO velox_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO velox_app;
ALTER ROLE velox_app SET idle_in_transaction_session_timeout = '30s';
SQL
      ;;
    citest)
      docker exec -i "$name" psql -v ON_ERROR_STOP=1 -q -U velox -d velox < "$ROOT/docker/init.sql" >&2 ;;
  esac
}

# The database each setup migrates, and the role that migrates it.
dsn() {
  local setup=$1 port=$2
  case "$setup" in
    compose) echo "postgres://velox:velox@127.0.0.1:$port/velox?sslmode=disable" ;;
    managed) echo "postgres://velox_owner:velox_owner@127.0.0.1:$port/velox?sslmode=disable" ;;
    citest)  echo "postgres://velox:velox@127.0.0.1:$port/velox_test?sslmode=disable" ;;
  esac
}
dbname() { case "$1" in citest) echo velox_test ;; *) echo velox ;; esac; }
superuser() { case "$1" in managed) echo postgres ;; *) echo velox ;; esac; }

# q CONTAINER SETUP SQL: one value, as the superuser.
q() { docker exec -i "$1" psql -v ON_ERROR_STOP=1 -qAt -U "$(superuser "$2")" -d "$(dbname "$2")" -c "$3"; }

head_velox() { local url=$1; shift; DATABASE_URL="$url" "$VELOX_BIN" "$@"; }

# dump CONTAINER SETUP KIND: a comparable dump, without the version record
# and without the lines that differ between any two dumps.
dump() {
  local c=$1 setup=$2 kind=$3
  local flags=(--schema-only)
  [ "$kind" = data ] && flags=(--data-only --column-inserts)
  docker exec "$c" pg_dump -U "$(superuser "$setup")" -d "$(dbname "$setup")" "${flags[@]}" \
      -T public.goose_db_version \
    | grep -v -E '^\\(un)?restrict |^-- Dumped (from database|by pg_dump) version ' \
    | sed -E "s/^(INSERT INTO public\.leader_leases \([^)]*\) VALUES \('[a-z_]+', )[0-9]+,/\1<holder_token>,/"
}

versions() { q "$1" "$2" "SELECT string_agg(version_id::text, ',' ORDER BY version_id) FROM public.goose_db_version"; }

# check_app_timeout CONTAINER SETUP: the limited user must start every
# session with a 30s idle-transaction timeout, the value
# docs/ops/postgres-requirements.md gives. Without it, a Velox process that
# freezes mid-transaction keeps its locks until something ends the session.
# It asks as that user, so it sees what the API's connections get.
check_app_timeout() {
  local c=$1 setup=$2 role=velox_app got
  [ "$setup" = citest ] && role=velox_test_app
  got=$(docker exec -i "$c" psql -v ON_ERROR_STOP=1 -qAt -U "$role" -d "$(dbname "$setup")" \
        -c "SHOW idle_in_transaction_session_timeout")
  [ "$got" = 30s ] || fail "$c: $role starts with idle_in_transaction_session_timeout = $got, want 30s"
}

# 0174's seed takes each row's token from clock_timestamp(), so the five
# tokens can straddle a millisecond: they are checked for age, not equality.
check_leases() {
  local c=$1 setup=$2 got
  got=$(q "$c" "$setup" "SELECT count(*) || ',' ||
         bool_and(abs(holder_token - (extract(epoch FROM now()) * 1000)::bigint) < 86400000) FROM public.leader_leases")
  [ "$got" = "5,true" ] || fail "$c: leader_leases want 5 rows with tokens from the last day, got $got"
}

fresh_install() {
  local setup=$1
  say "setup $setup"
  local c="mig-$setup" url
  CONTAINERS+=("$c")
  start_pg "$c" "$POSTGRES16_IMAGE" "$setup"
  url=$(dsn "$setup" "$PG_PORT")
  check_app_timeout "$c" "$setup"

  echo "status on the empty database"
  head_velox "$url" migrate status | tee "$OUT/$setup-status-before.txt"
  grep -q '^state: *empty' "$OUT/$setup-status-before.txt" || fail "$setup: status on the empty database does not say empty"
  [ "$(q "$c" "$setup" "SELECT to_regclass('public.goose_db_version') IS NULL")" = t ] \
    || fail "$setup: status created goose_db_version"

  echo "HEAD migrate, twice"
  head_velox "$url" migrate 2>&1 | tee "$OUT/$setup-migrate-1.txt"
  head_velox "$url" migrate 2>&1 | tee "$OUT/$setup-migrate-2.txt"
  grep -q 'migrations up to date' "$OUT/$setup-migrate-2.txt" || fail "$setup: the second run did not say up to date"
  if grep -q 'migration applied' "$OUT/$setup-migrate-2.txt"; then
    fail "$setup: the second run applied a migration"
  fi
  [ "$(versions "$c" "$setup")" = "$SHIPPED" ] \
    || fail "$setup: goose_db_version holds $(versions "$c" "$setup"), want $SHIPPED"
  head_velox "$url" migrate status | tee "$OUT/$setup-status-after.txt"
  grep -q 'up to date, nothing to apply' "$OUT/$setup-status-after.txt" || fail "$setup: status after migrate is not up to date"

  dump "$c" "$setup" schema > "$OUT/$setup-schema.sql"
  dump "$c" "$setup" data > "$OUT/$setup-data.sql"
  grep -q '^GRANT .* TO velox_app;$' "$OUT/$setup-schema.sql" || fail "$setup: the schema dump has no grants to velox_app"
  if grep '^INSERT INTO' "$OUT/$setup-data.sql" | grep -v -q '^INSERT INTO public\.leader_leases '; then
    fail "$setup: a table other than leader_leases has rows"
  fi
  check_leases "$c" "$setup"
  echo "$setup: installed, versions $(versions "$c" "$setup")"
}

restore_drill() {
  say "restore drill: a HEAD backup restored into an empty database"
  local c=mig-restore e=mig-restore-empty url eurl kind backup
  CONTAINERS+=("$c" "$e")
  start_pg "$c" "$POSTGRES16_IMAGE" compose
  url=$(dsn compose "$PG_PORT")
  head_velox "$url" migrate
  DATABASE_URL="$url" BACKUP_DIR="$WORK/backups" \
    PG_DUMP="docker run --rm -i --network host $POSTGRES16_IMAGE pg_dump" \
    "$ROOT/scripts/backup.sh"
  backup=$(find "$WORK/backups" -type f -name 'velox_*.dump' | head -1) # not the latest.dump link
  [ -n "$backup" ] || fail "restore drill: scripts/backup.sh wrote no backup"
  for kind in schema data; do
    dump "$c" compose "$kind" > "$OUT/restore-source-$kind.sql"
  done

  start_pg "$e" "$POSTGRES16_IMAGE" compose
  eurl=$(dsn compose "$PG_PORT")
  DATABASE_URL="$eurl" VELOX_RESTORE_CONFIRM=yes \
    PG_RESTORE="docker run --rm -i --network host $POSTGRES16_IMAGE pg_restore" \
    "$ROOT/scripts/restore.sh" "$backup" 2>&1 | tee "$OUT/restore.txt" \
    || fail "restore drill: scripts/restore.sh failed to restore the backup into an empty database"
  grep -q 'Restore complete' "$OUT/restore.txt" \
    || fail "restore drill: scripts/restore.sh did not report a complete restore"
  for kind in schema data; do
    dump "$e" compose "$kind" > "$OUT/restore-$kind.sql"
    if ! diff -u "$OUT/restore-source-$kind.sql" "$OUT/restore-$kind.sql" > "$OUT/restore-$kind.diff"; then
      head -100 "$OUT/restore-$kind.diff"
      fail "restore drill: the restored $kind differs from the database backed up"
    fi
  done
  [ "$(versions "$e" compose)" = "$(versions "$c" compose)" ] \
    || fail "restore drill: goose_db_version differs: backed up $(versions "$c" compose), restored $(versions "$e" compose)"
  head_velox "$eurl" migrate status | tee "$OUT/restore-status.txt"
  grep -q 'up to date, nothing to apply' "$OUT/restore-status.txt" \
    || fail "restore drill: the restored database is not up to date"
  echo "restore drill: the backup restored into an empty database, up to date"
}

pg14() {
  say "Postgres 14: HEAD applies"
  local c=mig-pg14 url
  CONTAINERS+=("$c")
  start_pg "$c" "$POSTGRES14_IMAGE" compose
  check_app_timeout "$c" compose
  url=$(dsn compose "$PG_PORT")
  head_velox "$url" migrate
  head_velox "$url" migrate status | tee "$OUT/pg14-status.txt"
  grep -q 'up to date, nothing to apply' "$OUT/pg14-status.txt" || fail "postgres 14: status is not up to date"
}

fresh_install compose
fresh_install managed
fresh_install citest
restore_drill
pg14
say "migrations: all checks passed"
