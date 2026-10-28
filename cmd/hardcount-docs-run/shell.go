package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// preamble runs once, before the page's first command. The page's commands
// run as a reader types them; these lines only make a failure stop the run:
//
//   - set -euo pipefail: a failing command, a failing part of a pipe or an
//     unset variable stops the run.
//   - curl: an answer other than 2xx fails the command. Its body goes to
//     stderr, into the log, since stdout is often piped into jq.
//   - jq --exit-status: a lookup that finds nothing (null or false) fails.
const preamble = `set -euo pipefail
shopt -s inherit_errexit 2>/dev/null || true
curl() {
  local __body __code __rc=0
  __body=$(mktemp)
  __code=$(command curl "$@" --output "$__body" --write-out '%{http_code}') || __rc=$?
  if [ "$__rc" -eq 0 ] && [[ "$__code" == 2?? ]]; then
    cat "$__body"
    rm -f "$__body"
    return 0
  fi
  if [ "$__rc" -ne 0 ]; then
    echo "docs-run: curl failed (exit $__rc). The body:" >&2
  else
    echo "docs-run: the answer was HTTP $__code, not 2xx. The body:" >&2
    __rc=22
  fi
  cat "$__body" >&2
  echo >&2
  rm -f "$__body"
  return "$__rc"
}
jq() { command jq --exit-status "$@"; }
`

// shell is one bash process that runs the page's commands in turn, so a
// variable or function one command sets is there for the next, as in the
// reader's terminal.
type shell struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	out      chan string // stdout lines
	done     chan error  // the process exit
	tmp      string
	sentinel string
	n        int
}

func startShell(dir string) (*shell, error) {
	tmp, err := os.MkdirTemp("", "docs-run-")
	if err != nil {
		return nil, err
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	s := &shell{
		tmp:      tmp,
		sentinel: "docs-run-done-" + hex.EncodeToString(rnd[:]),
		out:      make(chan string, 1024),
		done:     make(chan error, 1),
	}
	s.cmd = exec.Command("bash", "--noprofile", "--norc")
	s.cmd.Dir = dir
	s.cmd.Stderr = os.Stderr
	if s.stdin, err = s.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := s.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := s.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			s.out <- sc.Text()
		}
		close(s.out)
	}()
	go func() { s.done <- s.cmd.Wait() }()

	if _, err := s.run(preamble, 10*time.Second); err != nil {
		s.close()
		return nil, fmt.Errorf("preamble: %w", err)
	}
	return s, nil
}

var errExited = errors.New("the command stopped the run")

// run runs one block in the shell and returns what it printed on stdout.
// The block reads its stdin from /dev/null, so a command that reads stdin
// cannot swallow the next block.
func (s *shell) run(script string, timeout time.Duration) (string, error) {
	s.n++
	path := filepath.Join(s.tmp, fmt.Sprintf("block-%03d.sh", s.n))
	if err := os.WriteFile(path, []byte(script+"\n"), 0o600); err != nil {
		return "", err
	}
	if _, err := fmt.Fprintf(s.stdin, ". %q </dev/null\nprintf '\\n%%s\\n' %q\n", path, s.sentinel); err != nil {
		return "", s.exitErr()
	}
	var got []string
	deadline := time.After(timeout)
	for {
		select {
		case ln, ok := <-s.out:
			if !ok {
				return strings.Join(got, "\n"), s.exitErr()
			}
			if ln == s.sentinel {
				// Drop the newline printed before the sentinel.
				if len(got) > 0 && got[len(got)-1] == "" {
					got = got[:len(got)-1]
				}
				return strings.Join(got, "\n"), nil
			}
			got = append(got, ln)
		case <-deadline:
			s.close()
			return strings.Join(got, "\n"), fmt.Errorf("no end after %s", timeout)
		}
	}
}

func (s *shell) exitErr() error {
	select {
	case err := <-s.done:
		if err == nil {
			return errExited
		}
		return fmt.Errorf("%w: %v", errExited, err)
	case <-time.After(5 * time.Second):
		return errExited
	}
}

func (s *shell) close() {
	_ = s.stdin.Close()
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = os.RemoveAll(s.tmp)
}
