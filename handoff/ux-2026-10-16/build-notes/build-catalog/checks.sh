#!/bin/bash
# usage: checks.sh "<go pkgs e.g. ./internal/usage/... ./internal/api/...>" "<web files to eslint>" [skipgo]
cd /home/user/velox/.claude/worktrees/catalog
PKGS="$1"; WEB="$2"
if [ -z "$3" ]; then
echo "== gofmt"; gofmt -l $(git diff --name-only origin/feat/app-shell -- '*.go' 2>/dev/null; git ls-files -m -o --exclude-standard -- '*.go') 2>/dev/null | sort -u
echo "== go vet"; go vet ./... 2>&1 | tail -3
echo "== golangci-lint"; /tmp/claude-0/gobin/golangci-lint run $PKGS 2>&1 | tail -5
echo "== lints"; make lint-clock lint-tz lint-funding-set lint-manual-test 2>&1 | tail -6
echo "== go test short"; go test ./... -short -count=1 2>&1 | grep -v "^ok\|no test files" | tail -15; echo "(go test short done)"
echo "== integration"; flock /tmp/claude-0/pg.lock go test -p 1 -count=1 -short=false $PKGS 2>&1 | tail -6
else
echo "== lint-manual-test"; make lint-manual-test 2>&1 | tail -2
fi
cd web-v2
echo "== tsc"; npx tsc -b 2>&1 | tail -5
echo "== eslint"; npx eslint $WEB 2>&1 | tail -4
echo "== npm test"; npm test 2>&1 | grep -E "^# (tests|pass|fail)"
echo "== build"; npm run build 2>&1 | grep -E "error|built in" | tail -3
