#!/usr/bin/env bash
# Hardcount Database Restore Script
#
# Restores a database from a pg_dump backup file, in one transaction, with
# each restored object's privileges as the backup holds them.
#
# A custom-format backup (what scripts/backup.sh writes) goes through
# pg_restore into psql, so psql must be at least as recent as pg_restore. A
# plain SQL dump restores only into a database where the restoring user has
# no default privileges (see "The backup's privileges" below).
#
# Usage:
#   DATABASE_URL="postgres://hardcount:hardcount@localhost:5432/hardcount?sslmode=disable" \
#     ./scripts/restore.sh /var/backups/hardcount/hardcount_20260415_030000.dump
#
# Optional environment variables:
#   HARDCOUNT_RESTORE_CONFIRM=yes   Skip the interactive confirmation prompt (for automation).
#
# Exit codes:
#   0  Success
#   1  Configuration/argument error
#   2  Restore failed
#   3  Post-restore validation failed

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

BACKUP_FILE="${1:-}"

if [ -z "$BACKUP_FILE" ]; then
  echo "Usage: ./scripts/restore.sh <backup-file>"
  echo ""
  echo "Examples:"
  echo "  ./scripts/restore.sh /var/backups/hardcount/hardcount_20260415_030000.dump"
  echo "  ./scripts/restore.sh /var/backups/hardcount/latest.dump"
  echo ""
  echo "Environment:"
  echo "  DATABASE_URL              Required. Target database connection string."
  echo "  HARDCOUNT_RESTORE_CONFIRM=yes  Skip interactive confirmation (for CI/automation)."
  exit 1
fi

if [ -z "${DATABASE_URL:-}" ]; then
  echo "ERROR: DATABASE_URL environment variable is required." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() {
  echo "[$(date -u +"%Y-%m-%d %H:%M:%S UTC")] $*"
}

die() {
  log "FATAL: $*" >&2
  exit "${2:-1}"
}

# privilege_sql reads pg_restore's SQL for a backup on stdin and writes SQL
# that leaves each object the backup holds privileges for with exactly those
# privileges: REVOKE ALL from each role a GRANT in the backup names, on that
# object, then the backup's privilege statements again, in order.
#
# pg_restore needs this help. It creates each table, and then runs the
# backup's GRANTs, which only add privileges. The target's default
# privileges (ALTER DEFAULT PRIVILEGES) have already given the new table
# theirs, and nothing takes back what the backup does not grant. Compose's
# deploy/compose/postgres-init.sh and docs/ops/postgres-requirements.md give
# hardcount_app ALL on new tables, so audit_log, where hardcount_app may only read
# and add rows (migration 0150), came back with UPDATE, DELETE and TRUNCATE.
#
# It reads only the backup's privilege entries, each headed by a
# "-- Name: ...; Type: ACL; ..." comment, from a --schema-only script, which
# holds no table rows. A GRANT it cannot read stops it.
privilege_sql() {
  awk '
    /^-- (Data for )?Name: / { acl = ($0 ~ /; Type: ACL; /); next }
    !acl || /^--/ || $0 == "" { next }
    /^GRANT / {
      s = $0
      sub(/;$/, "", s)
      sub(/ WITH GRANT OPTION$/, "", s)
      on = index(s, " ON ")
      to = 0
      for (i = length(s) - 3; on > 0 && i > on; i--)
        if (substr(s, i, 4) == " TO ") { to = i; break }
      if (to == 0) { print "cannot read this GRANT: " $0 > "/dev/stderr"; bad = 1; exit 1 }
      # CASCADE: a grant this role made with a grant option goes too; the
      # statements after the REVOKEs make it again.
      revoke = "REVOKE ALL" substr(s, on, to - on) " FROM " substr(s, to + 4) " CASCADE;"
      if (!(revoke in seen)) { seen[revoke] = 1; revokes[++nr] = revoke }
    }
    { stmts[++ns] = $0 }
    END {
      if (bad) exit 1
      for (i = 1; i <= nr; i++) print revokes[i]
      for (i = 1; i <= ns; i++) print stmts[i]
    }'
}

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------

# Resolve symlinks so we display the actual file.
if [ -L "$BACKUP_FILE" ]; then
  RESOLVED_FILE=$(readlink -f "$BACKUP_FILE" 2>/dev/null || readlink "$BACKUP_FILE")
  log "Resolved symlink: $BACKUP_FILE -> $RESOLVED_FILE"
  BACKUP_FILE="$RESOLVED_FILE"
fi

if [ ! -f "$BACKUP_FILE" ]; then
  die "Backup file not found: $BACKUP_FILE" 1
fi

BACKUP_SIZE=$(wc -c < "$BACKUP_FILE" | tr -d ' ')
if [ "$BACKUP_SIZE" -eq 0 ]; then
  die "Backup file is empty: $BACKUP_FILE" 1
fi

# Detect file format.
FILE_TYPE="unknown"
if file "$BACKUP_FILE" | grep -q "PostgreSQL custom database dump"; then
  FILE_TYPE="custom"
elif file "$BACKUP_FILE" | grep -q "ASCII\|UTF-8\|text"; then
  FILE_TYPE="sql"
else
  # Try pg_restore --list as a heuristic for custom format.
  if pg_restore --list "$BACKUP_FILE" >/dev/null 2>&1; then
    FILE_TYPE="custom"
  fi
fi

log "Backup file: $BACKUP_FILE"
log "File size:   $(numfmt --to=iec "$BACKUP_SIZE" 2>/dev/null || echo "${BACKUP_SIZE} bytes")"
log "File type:   $FILE_TYPE"

# Extract target database info for display.
DB_HOST=$(echo "$DATABASE_URL" | sed -E 's|.*@([^:/]*)[:/].*|\1|')
DB_PORT=$(echo "$DATABASE_URL" | sed -E 's|.*:([0-9]+)/.*|\1|')
DB_NAME=$(echo "$DATABASE_URL" | sed -E 's|.*://[^/]*/([^?]*).*|\1|')

# Check required tools. PG_RESTORE lets an operator point at a
# version-matched binary when host tools don't match the target server —
# e.g. PG_RESTORE="docker run --rm -i --network host postgres:16-alpine pg_restore"
# (the archive is fed via stdin, so a wrapped binary needs no mounts).
PG_RESTORE="${PG_RESTORE:-pg_restore}"
command -v ${PG_RESTORE%% *} >/dev/null 2>&1 || die "pg_restore not found in PATH." 1
command -v psql >/dev/null 2>&1 || die "psql not found in PATH." 1

# pg_restore emits client-version SET commands (SET transaction_timeout is
# PG17+) regardless of the archive's origin, so a pg_restore major newer
# than the TARGET server rolls the whole single-transaction restore back.
# The 2026-08-11 drill hit exactly this. Fail before touching the target.
RESTORE_TOOL_MAJOR=$($PG_RESTORE --version | sed -E 's/.* ([0-9]+)(\.[0-9]+)?.*/\1/')
TARGET_MAJOR=$(psql "$DATABASE_URL" -tAc "SHOW server_version" 2>/dev/null | sed -E 's/^([0-9]+).*/\1/')
if [ -n "$TARGET_MAJOR" ] && [ -n "$RESTORE_TOOL_MAJOR" ] && [ "$RESTORE_TOOL_MAJOR" -gt "$TARGET_MAJOR" ] \
   && [ "${HARDCOUNT_RESTORE_TOOL_MISMATCH_OK:-no}" != "yes" ]; then
  die "pg_restore major ($RESTORE_TOOL_MAJOR) is newer than the target server major ($TARGET_MAJOR) — its client SET commands will roll the restore back. Use matching tools (e.g. PG_RESTORE=\"docker run --rm -i --network host postgres:${TARGET_MAJOR}-alpine pg_restore\") or set HARDCOUNT_RESTORE_TOOL_MISMATCH_OK=yes." 1
fi

# Hardcount archives carry GRANTs to the hardcount_app runtime role (ADR-073). On a
# FRESH target — the disaster-recovery case — that role does not exist, one
# GRANT fails, and --single-transaction rolls the whole restore back. Roles
# are cluster-level, so pg_dump of a database cannot carry them; the target
# must provision them first. Fail before touching anything, with the remedy.
if ! psql "$DATABASE_URL" -tAc "SELECT 1 FROM pg_roles WHERE rolname='hardcount_app'" 2>/dev/null | grep -q 1; then
  die "Target cluster has no 'hardcount_app' role — the archive's GRANTs will roll the restore back. Create it first (CREATE ROLE hardcount_app LOGIN PASSWORD '<your-password>'; see docker/init.sql for the dev shape and docs/ops/backup-considerations.md for the restore runbook), then re-run." 1
fi

# The backup's privileges: SQL the restore runs last (see privilege_sql).
# A plain SQL dump holds its table rows in the same text, so the script does
# not search it for privilege statements: it restores one only into a
# database where the restoring user has no default privileges, which is
# where the dump's GRANTs alone decide each table's privileges.
PRIVILEGE_SQL=""
if [ "$FILE_TYPE" = "custom" ]; then
  log "Reading the privileges the backup holds..."
  PRIVILEGE_SQL=$($PG_RESTORE --schema-only --no-owner -f - < "$BACKUP_FILE" | privilege_sql) \
    || die "Could not read the privileges in the backup file." 1
elif [ "$FILE_TYPE" = "sql" ]; then
  HAS_DEFAULT_PRIVILEGES=$(psql "$DATABASE_URL" -X -tAc \
    "SELECT EXISTS (SELECT 1 FROM pg_default_acl WHERE defaclrole = current_user::regrole)" 2>/dev/null || echo "unknown")
  if [ "$HAS_DEFAULT_PRIVILEGES" != "f" ]; then
    die "The target database gives the restoring user's new tables default privileges (ALTER DEFAULT PRIVILEGES), or the check failed, and restore.sh cannot take those back from a plain SQL dump: tables such as audit_log would come back with more access for hardcount_app than the backup gives. Restore a custom-format backup (scripts/backup.sh makes one), or restore this file into a database without default privileges." 1
  fi
fi

# Test database connectivity.
if command -v pg_isready >/dev/null 2>&1; then
  pg_isready -h "${DB_HOST:-localhost}" -p "${DB_PORT:-5432}" -q || die "Database is not reachable at ${DB_HOST}:${DB_PORT}." 1
fi

# ---------------------------------------------------------------------------
# Confirmation
# ---------------------------------------------------------------------------

echo ""
echo "================================================================"
echo "  WARNING: This will OVERWRITE data in the target database."
echo ""
echo "  Target:  ${DB_NAME} @ ${DB_HOST}:${DB_PORT}"
echo "  Source:   ${BACKUP_FILE}"
echo "  Size:    $(numfmt --to=iec "$BACKUP_SIZE" 2>/dev/null || echo "${BACKUP_SIZE} bytes")"
echo "================================================================"
echo ""

if [ "${HARDCOUNT_RESTORE_CONFIRM:-}" = "yes" ]; then
  log "HARDCOUNT_RESTORE_CONFIRM=yes — skipping interactive prompt."
else
  printf "Type 'yes' to proceed: "
  read -r CONFIRM
  if [ "$CONFIRM" != "yes" ]; then
    log "Restore cancelled by operator."
    exit 0
  fi
fi

# ---------------------------------------------------------------------------
# Pre-restore: capture current state
# ---------------------------------------------------------------------------

log "Capturing pre-restore row counts for comparison..."

PRE_COUNTS=$(psql "$DATABASE_URL" -t -A -c "
  SELECT coalesce(sum(n_live_tup), 0)
  FROM pg_stat_user_tables;
" 2>/dev/null || echo "unknown")

log "Pre-restore total rows: ${PRE_COUNTS}"

# ---------------------------------------------------------------------------
# Restore
# ---------------------------------------------------------------------------

START_TIME=$(date +%s)

if [ "$FILE_TYPE" = "custom" ]; then
  log "Restoring using pg_restore (custom format)..."

  # One transaction, all or nothing: pg_restore writes the archive as SQL,
  # and psql runs BEGIN, that SQL, the privilege SQL, COMMIT. pg_restore
  # cannot run the privilege SQL itself, and a second transaction after it
  # could leave a restored database with the wider privileges.
  # --clean drops existing objects before recreating.
  # --if-exists avoids errors when objects don't exist.
  # --no-owner skips ownership commands (the restoring user becomes owner).
  # COMMIT is written only after pg_restore has written the whole archive:
  # if pg_restore stops early, psql reaches the end of its input with the
  # transaction open, and Postgres rolls it back. ON_ERROR_STOP stops psql at
  # the first error, which rolls back the same way.
  # Archive via STDIN (custom format supports it) so PG_RESTORE can be a
  # wrapped binary with no view of the host filesystem.
  {
    echo 'BEGIN;'
    $PG_RESTORE --clean --if-exists --no-owner --verbose -f - < "$BACKUP_FILE" \
      2> >(while IFS= read -r line; do log "  pg_restore: $line"; done >&2) \
      || exit 1
    printf '%s\n' "$PRIVILEGE_SQL"
    echo 'COMMIT;'
  } | psql "$DATABASE_URL" -X -q -v ON_ERROR_STOP=1 -o /dev/null -f - 2>&1 \
    | while IFS= read -r line; do log "  psql: $line"; done \
    || die "The restore failed — the single-transaction restore was rolled back." 2

elif [ "$FILE_TYPE" = "sql" ]; then
  log "Restoring using psql (SQL format)..."

  psql "$DATABASE_URL" \
    --single-transaction \
    --set ON_ERROR_STOP=on \
    -f "$BACKUP_FILE" \
    2>&1 | while IFS= read -r line; do log "  psql: $line"; done \
    || die "psql restore failed — the single-transaction restore was rolled back." 2

else
  # Input problem, not a failed restore — exit 1 per the header's taxonomy.
  die "Unrecognized backup file format. Expected pg_dump custom format or plain SQL." 1
fi

END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

log "Restore completed in ${DURATION}s."

# ---------------------------------------------------------------------------
# Post-restore validation
# ---------------------------------------------------------------------------

log "Running post-restore validation..."

# Check that critical tables exist and have data.
VALIDATION_QUERY="
  SELECT relname AS table_name, n_live_tup AS row_count
  FROM pg_stat_user_tables
  WHERE schemaname = 'public'
  ORDER BY n_live_tup DESC
  LIMIT 20;
"

log "Table row counts after restore:"
psql "$DATABASE_URL" -c "$VALIDATION_QUERY" 2>&1 | while IFS= read -r line; do log "  $line"; done

POST_COUNTS=$(psql "$DATABASE_URL" -t -A -c "
  SELECT coalesce(sum(n_live_tup), 0)
  FROM pg_stat_user_tables;
" 2>/dev/null || echo "unknown")

log "Post-restore total rows: ${POST_COUNTS}"

# Check critical Hardcount tables exist.
CRITICAL_TABLES="tenants customers subscriptions invoices customer_credit_ledger"
MISSING_TABLES=""

for table in $CRITICAL_TABLES; do
  EXISTS=$(psql "$DATABASE_URL" -t -A -c "
    SELECT EXISTS (
      SELECT 1 FROM information_schema.tables
      WHERE table_schema = 'public' AND table_name = '$table'
    );
  " 2>/dev/null || echo "f")

  if [ "$EXISTS" != "t" ]; then
    MISSING_TABLES="${MISSING_TABLES} ${table}"
  fi
done

if [ -n "$MISSING_TABLES" ]; then
  log "WARNING: The following critical tables are missing after restore:${MISSING_TABLES}"
  log "The restore may be incomplete. Check the backup file and try again."
  exit 3
fi

log "All critical tables present."

# ---------------------------------------------------------------------------
# Migration check
# ---------------------------------------------------------------------------

log "Checking migration state..."

# Report the record as it is; `hardcount migrate status` decides what this
# release would do with the database.
GOOSE_VERSION=$(psql "$DATABASE_URL" -t -A -c "
  SELECT CASE WHEN to_regclass('public.goose_db_version') IS NULL THEN ''
              ELSE (SELECT max(version_id)::text FROM public.goose_db_version) END;
" 2>/dev/null || echo "unknown")

if [ -n "$GOOSE_VERSION" ]; then
  log "goose_db_version: ${GOOSE_VERSION}"
  LATEST_MIGRATION="$GOOSE_VERSION"
else
  log "WARNING: No migration record found."
fi
log "NOTE: Run \"hardcount migrate status\" with the Hardcount release you will start, to check it can upgrade this database."

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

echo ""
log "========================================"
log "Restore Summary"
log "  Source:    ${BACKUP_FILE}"
log "  Target:    ${DB_NAME} @ ${DB_HOST}:${DB_PORT}"
log "  Duration:  ${DURATION}s"
log "  Rows:      ${PRE_COUNTS} (before) -> ${POST_COUNTS} (after)"
log "  Tables:    All critical tables present"
log "  Migration: ${LATEST_MIGRATION:-check manually}"
log "========================================"
echo ""
log "Restore complete. Verify application behavior before resuming traffic."

exit 0
