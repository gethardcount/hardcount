#!/usr/bin/env bash
#
# Runs ShellCheck on every shell script git tracks: each *.sh file, and each
# other file whose first line is a sh, bash, dash or ksh shebang. A finding of
# severity warning or error fails the run. Info and style findings do not.
# (A comment line that starts with the word "shellcheck" is read as a
# directive, so this header does not start with the file's name.)
#
# CI runs it in the lint job, with the ShellCheck release pinned there
# (.github/workflows/ci.yml). Locally it runs the shellcheck on your PATH.
#
# To waive a finding, put `# shellcheck disable=SCnnnn` on the line before the
# one it covers, and the reason in a comment on the line above that.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

shebang='^#!.*[/[:space:]](sh|bash|dash|ksh)([[:space:]]|$)'
scripts=()
while IFS= read -r -d '' f; do
  # A file deleted from the working tree but still in the index has nothing
  # to check.
  [ -f "$f" ] || continue
  if [[ $f == *.sh ]]; then
    scripts+=("$f")
    continue
  fi
  first=""
  IFS= read -r first < "$f" || true
  if [[ $first =~ $shebang ]]; then scripts+=("$f"); fi
done < <(git ls-files -z)

if [ ${#scripts[@]} -eq 0 ]; then
  echo "found no shell scripts: is this a Hardcount checkout?" >&2
  exit 1
fi

echo "ShellCheck $(shellcheck --version | sed -n 's/^version: //p'), severity warning, on ${#scripts[@]} scripts:"
printf '  %s\n' "${scripts[@]}"
# --external-sources follows each `source` to the file it names, so a
# variable a sourced file defines does not read as undefined.
shellcheck --external-sources --severity=warning "${scripts[@]}"
echo "ShellCheck: no findings."
