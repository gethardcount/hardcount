#!/usr/bin/env bash
# Usage: checks.sh <worktree> <outfile> <go pkgs (space-sep, e.g. ./internal/invoice/...)> [-- web files for eslint]
WT=$1; OUT=$2; shift 2
PKGS=(); WEB=()
mode=go
for a in "$@"; do if [ "$a" = "--" ]; then mode=web; continue; fi; if [ $mode = go ]; then PKGS+=("$a"); else WEB+=("$a"); fi; done
cd "$WT" || exit 1
{
echo "== gofmt"; gofmt -l internal cmd | head
echo "== go vet"; go vet ./... 2>&1 | tail -5; echo "vet exit ${PIPESTATUS[0]}"
if [ ${#PKGS[@]} -gt 0 ]; then echo "== golangci-lint ${PKGS[*]}"; /tmp/claude-0/gobin/golangci-lint run "${PKGS[@]}" 2>&1 | tail -15; echo "lint exit ${PIPESTATUS[0]}"; fi
echo "== make lint-clock lint-tz lint-funding-set lint-manual-test"; make lint-clock lint-tz lint-funding-set lint-manual-test 2>&1 | tail -8; echo "make exit ${PIPESTATUS[0]}"
echo "== go test ./... -short"; go test ./... -short -count=1 2>&1 | grep -v "^ok\|no test files" | tail -20; echo "gotest exit ${PIPESTATUS[0]}"
cd web-v2
echo "== tsc -b"; npx tsc -b 2>&1 | tail -10; echo "tsc exit ${PIPESTATUS[0]}"
if [ ${#WEB[@]} -gt 0 ]; then echo "== eslint ${WEB[*]}"; npx eslint "${WEB[@]}" 2>&1 | tail -10; echo "eslint exit ${PIPESTATUS[0]}"; fi
echo "== npm test"; npm test 2>&1 | grep -E "^# (tests|pass|fail)" ; echo "npmtest exit ${PIPESTATUS[0]}"
echo "== npm run build"; npm run build 2>&1 | tail -3; echo "build exit ${PIPESTATUS[0]}"
} > "$OUT" 2>&1
echo done
