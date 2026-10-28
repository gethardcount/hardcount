#!/bin/bash
# Hardcount single-VM Postgres init — runs INSIDE the postgres container on
# first boot of a fresh pgdata volume (docker-entrypoint-initdb.d).
#
# Hardcount connects via two roles:
#   $POSTGRES_USER — owner / migration role (superuser in this image)
#   hardcount_app      — least-privilege runtime role; RLS is enforced here.
#
# Why a separate app role: superusers and BYPASSRLS roles defeat
# row-level security wholesale (every RLS table is FORCE ROW LEVEL
# SECURITY, so plain ownership does not). The API refuses to boot in
# production when APP_DATABASE_URL is missing, carries a default
# password, or points at a bypass-capable role (ADR-073).
#
# The password comes from HARDCOUNT_APP_DB_PASSWORD (compose passes it to
# this container). psql's :'var' quoting is used so any legal password
# survives — do NOT inline it into the SQL string.
#
# The idle-transaction timeout is required (docs/ops/postgres-requirements.md,
# "Timeouts on the limited user"). Closing a billing period locks the
# subscription row until the invoice is saved; if a Hardcount process freezes in
# between (a paused VM, say), the timeout ends its session and frees the lock.
# Only hardcount_app gets it: migrations run as $POSTGRES_USER.
#
# Existing pgdata volumes: this script does NOT re-run. Rotate the role
# password manually instead:
#   docker compose exec postgres psql -U hardcount -d hardcount \
#     -v pw="$HARDCOUNT_APP_DB_PASSWORD" \
#     -c "ALTER ROLE hardcount_app PASSWORD :'pw'"
# and set the timeout once (new connections pick it up):
#   docker compose exec postgres psql -U hardcount -d hardcount \
#     -c "ALTER ROLE hardcount_app SET idle_in_transaction_session_timeout = '30s'"
set -euo pipefail

: "${HARDCOUNT_APP_DB_PASSWORD:?HARDCOUNT_APP_DB_PASSWORD is required — set it in .env (see .env.example)}"

psql -v ON_ERROR_STOP=1 \
     -v pw="$HARDCOUNT_APP_DB_PASSWORD" \
     -v dbname="$POSTGRES_DB" \
     --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'EOSQL'
CREATE ROLE hardcount_app WITH LOGIN PASSWORD :'pw';
GRANT ALL PRIVILEGES ON DATABASE :"dbname" TO hardcount_app;
GRANT ALL ON SCHEMA public TO hardcount_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO hardcount_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO hardcount_app;
ALTER ROLE hardcount_app SET idle_in_transaction_session_timeout = '30s';
EOSQL
