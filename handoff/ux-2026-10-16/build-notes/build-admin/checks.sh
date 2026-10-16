#!/bin/bash
# usage: checks.sh <worktree> <go pkgs (space sep, e.g. ./internal/analytics/...)> <web files...>
WT=$1; PKGS=$2; shift 2
cd $WT
echo "== gofmt"; gofmt -l $(git diff --name-only origin/feat/app-shell -- '*.go' | xargs -r ls 2>/dev/null) ; echo "(end gofmt)"
echo "== go vet"; go vet ./... 2>&1 | tail -5; echo "vet exit ${PIPESTATUS[0]}"
if [ -n "$PKGS" ]; then echo "== golangci-lint $PKGS"; /tmp/claude-0/gobin/golangci-lint run $PKGS 2>&1 | tail -10; fi
echo "== make lints"; make lint-clock lint-tz lint-funding-set lint-manual-test 2>&1 | tail -6
echo "== go test short"; go test ./... -short -count=1 2>&1 | grep -v "^ok\|no test files" | tail -20; echo "go test done"
cd web-v2
echo "== tsc"; npx tsc -b 2>&1 | tail -10; echo "tsc exit $?"
if [ $# -gt 0 ]; then echo "== eslint"; npx eslint "$@" 2>&1 | tail -15; echo "eslint done"; fi
echo "== npm test"; npm test 2>&1 | grep -E "^# (pass|fail)"
echo "== build"; npm run build 2>&1 | grep -iE "error|built in" | tail -5
