#!/usr/bin/env bash
#
# lint-clock.sh — flags bare `time.Now()` calls in packages where
# clock-pinned entities live, so we don't regress ADR-030
# (simulated-time-everywhere on clock-pinned entities).
#
# What this enforces:
#   In every non-test .go file of the packages listed below,
#   `time.Now()` must be replaced with one of:
#     - s.clock.Now(ctx)        — services with a Clock field
#     - clock.Now(ctx)          — postgres stores / render code
#   OR carry an explicit justification on the same line:
#     - // wall-clock: <reason>
#
# Why this matters:
#   The Stripe-style "no semantic change" guarantee means every
#   timestamp on a clock-pinned entity should be in simulated time.
#   ctx-bound effective-now propagates from the entry point down
#   automatically — but only if every site reads through the clock
#   abstraction. A bare `time.Now()` in a high-risk package leaks
#   wall-clock onto a clock-pinned row and strands it outside the
#   catchup window.
#
# How to bypass for a legitimate wall-clock case:
#   Add `// wall-clock: <one-line reason>` to the line. Reviewers
#   can grep for these to audit the carve-outs.
#
# Run with:
#   make lint-clock
# Exits non-zero if any violation is found, prints offending lines.

set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

# Packages where clock-pinned entities are written. Adding a new
# package here is the right move when it joins the simulated-time
# domain.
PACKAGES=(
  "internal/subscription"
  "internal/invoice"
  "internal/credit"
  "internal/dunning"
  "internal/customer"
  "internal/billing"
  "internal/creditnote"
  # Ingest doors write usage_event timestamps on clock-pinned customers, so a
  # bare time.Now() here leaks wall-clock into simulated time. The LiteLLM door
  # regressed exactly this (a manufactured time.Now() default defeated the test
  # clock); scanning it keeps the fix from silently coming back.
  "internal/integrations/litellm"
)

# Every non-test .go file in each package is scanned. An earlier version
# matched a short list of file names (service.go, postgres.go, ...) and so
# read 15 of the 48 files; handler.go, where a wall-clock proration bug once
# lived, was never looked at.
violations=0
violation_lines=""
files_scanned=0

for pkg in "${PACKAGES[@]}"; do
  pkg_files=0
  while IFS= read -r file; do
    [ -z "$file" ] && continue
    pkg_files=$((pkg_files + 1))
    # Match `time.Now(` excluding lines with the wall-clock marker.
    while IFS=: read -r line_num line_content; do
      [ -z "$line_num" ] && continue
      # Skip if the line carries a // wall-clock: justification.
      if echo "$line_content" | grep -qE "//\s*wall-clock:"; then
        continue
      fi
      # Skip pure comment lines — `time.Now()` mentioned in a doc
      # comment isn't an actual call.
      if echo "$line_content" | grep -qE "^\s*//"; then
        continue
      fi
      violations=$((violations + 1))
      violation_lines="${violation_lines}${file}:${line_num}: ${line_content}\n"
    done < <(grep -nE "time\.Now\(\)" "$file" 2>/dev/null || true)
  done < <(find "$pkg" -name "*.go" -not -name "*_test.go" 2>/dev/null || true)
  # A package that yields no files means the list above is stale (renamed or
  # moved package) and the gate would pass while checking nothing.
  if [ "$pkg_files" -eq 0 ]; then
    echo "lint-clock: package $pkg has no .go files to scan — fix the PACKAGES list" >&2
    exit 1
  fi
  files_scanned=$((files_scanned + pkg_files))
done

if [ "$violations" -gt 0 ]; then
  echo "ADR-030 violation: $violations bare time.Now() call(s) in clock-pinned packages."
  echo ""
  echo -e "$violation_lines" | head -50
  echo ""
  echo "Fix:"
  echo "  - In a service with a Clock field: use s.clock.Now(ctx)"
  echo "  - In a postgres store or render code: use clock.Now(ctx)"
  echo "  - If wall-clock is genuinely intended, add a same-line"
  echo "    comment: // wall-clock: <one-line reason>"
  echo ""
  echo "See docs/adr/030-simulated-time-everywhere-on-clock-pinned-entities.md"
  exit 1
fi

echo "lint-clock: ok (${#PACKAGES[@]} packages, $files_scanned files scanned, no violations)"
