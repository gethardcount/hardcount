#!/bin/bash
# Re-run checks per branch worktree. Output one summary line per check.
cd /home/user/velox
LINT=/tmp/claude-0/gobin/golangci-lint
for wt in .claude/worktrees/*/; do
  wt=${wt%/}; b=$(git -C $wt rev-parse --abbrev-ref HEAD)
  files=$(git -C $wt diff --name-only origin/main...HEAD)
  gopkgs=$(echo "$files" | grep '\.go$' | grep -v '\.gen\.go$' | xargs -r -n1 dirname | sort -u | sed 's#^#./#')
  echo "### $b ($(git -C $wt rev-parse --short HEAD))"
  [ -n "$(git -C $wt status --porcelain | grep -v node_modules)" ] && echo "  DIRTY: $(git -C $wt status --porcelain | grep -v node_modules | tr '\n' ' ')"
  if [ -n "$gopkgs" ]; then
    (cd $wt && f=$(gofmt -l $(echo "$files" | grep '\.go$')); [ -z "$f" ] && echo "  gofmt ok" || echo "  gofmt FAIL $f")
    (cd $wt && go vet $gopkgs >/dev/null 2>&1 && echo "  vet ok" || echo "  vet FAIL")
    (cd $wt && out=$($LINT run $gopkgs 2>&1 | tail -3); echo "$out" | grep -q "0 issues" && echo "  lint ok" || echo "  lint: $out")
    (cd $wt && go test -short -count=1 $gopkgs 2>&1 | grep -v '^ok\|no test files' | head -5 | sed 's/^/  test: /'; echo "  short tests done")
  fi
  if echo "$files" | grep -q '^api/openapi.yaml'; then
    (cd $wt && make gen >/dev/null 2>&1; d=$(git status --porcelain | grep -v node_modules); [ -z "$d" ] && echo "  gen no drift" || { echo "  gen DRIFT: $d"; git checkout -- . ; })
  fi
  if echo "$files" | grep -q '^web-v2/'; then
    [ -e $wt/web-v2/node_modules ] || ln -s /home/user/velox/web-v2/node_modules $wt/web-v2/node_modules
    (cd $wt/web-v2 && npx tsc -b >/tmp/claude-0/tsc.$$ 2>&1 && echo "  tsc ok" || echo "  tsc FAIL $(head -3 /tmp/claude-0/tsc.$$)")
    chg=$(echo "$files" | grep '^web-v2/src.*\.tsx\?$\|^web-v2/tests.*\.ts$' | grep -v '\.gen\.' | sed 's#^web-v2/##')
    [ -n "$chg" ] && (cd $wt/web-v2 && npx eslint $chg >/tmp/claude-0/esl.$$ 2>&1; grep -q " error " /tmp/claude-0/esl.$$ && echo "  eslint ERRORS" || echo "  eslint ok (0 errors)")
    (cd $wt/web-v2 && npm test 2>&1 | grep -E '^# (pass|fail)' | tr '\n' ' '; echo)
  fi
done
