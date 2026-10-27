package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func runPage(t *testing.T, page string, skips, replaces []string) error {
	t.Helper()
	p, err := makePlan(page, skips, replaces)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	return execute("test.md", p, t.TempDir(), 5, 20*time.Second)
}

func cmdBlock(script string) string {
	return "<!-- docs-run: command -->\n```bash\n" + script + "\n```\n\n"
}
func outBlock(text string) string { return "<!-- docs-run: output -->\n```text\n" + text + "\n```\n\n" }

func TestExecuteKeepsShellStateAndChecksOutput(t *testing.T) {
	page := cmdBlock("X=hello\nsay() { echo \"$1 $X\"; }") + outBlock("") +
		cmdBlock("say hi") + outBlock("hi hello")
	if err := runPage(t, page, nil, nil); err != nil {
		t.Fatalf("want a pass, got %v", err)
	}
	bad := cmdBlock("echo 600") + outBlock("601")
	err := runPage(t, bad, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "is not what test.md:") {
		t.Fatalf("want an output mismatch, got %v", err)
	}
}

func TestExecuteStopsOnAFailure(t *testing.T) {
	// Each output block shows what the command prints if it does not stop,
	// so the run must stop on the failure, not on the output.
	cases := map[string][2]string{
		"failing command":      {"false\necho after", "after"},
		"failing part of pipe": {"false | cat", ""},
		"unset variable":       {"echo \"$DOCS_RUN_NOT_SET\"", ""},
		"failing substitution": {"X=$(false)", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			err := runPage(t, cmdBlock(c[0])+outBlock(c[1])+cmdBlock("echo next")+outBlock("next"), nil, nil)
			if err == nil || strings.Contains(err.Error(), "is not what") {
				t.Fatalf("want the run to stop on the failure, got %v", err)
			}
		})
	}
}

// A command that reads stdin gets nothing, and the next block still runs.
func TestExecuteBlockStdinIsEmpty(t *testing.T) {
	page := cmdBlock("cat") + outBlock("") + cmdBlock("echo next") + outBlock("next")
	if err := runPage(t, page, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteCurlFailsOnNon2xx(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/moved":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"already exists"}`))
		}
	}))
	defer srv.Close()

	ok := cmdBlock(`curl -s "BASE/ok"`) + outBlock(`{"status":"ok"}`)
	if err := runPage(t, ok, nil, []string{"BASE=" + srv.URL}); err != nil {
		t.Fatalf("200: %v", err)
	}
	for _, path := range []string{"conflict", "moved"} {
		page := cmdBlock(`ID=$(curl -s "BASE/`+path+`")`) + outBlock("") + cmdBlock("echo next") + outBlock("next")
		if err := runPage(t, page, nil, []string{"BASE=" + srv.URL}); err == nil {
			t.Fatalf("%s: want the run to stop", path)
		}
	}

	// Most of the page pipes curl into jq. The answer's body still reaches
	// the log, so a failure says why.
	piped := cmdBlock(`ID=$(curl -s "BASE/conflict" | jq -r .id)`) + outBlock("")
	stderr := captureStderr(t, func() {
		if err := runPage(t, piped, nil, []string{"BASE=" + srv.URL}); err == nil {
			t.Fatal("piped: want the run to stop")
		}
	})
	if !strings.Contains(stderr, `{"error":"already exists"}`) {
		t.Fatalf("piped: the answer's body is not in the log:\n%s", stderr)
	}
}

// captureStderr returns what fn, and the shells it starts, write to stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	var buf bytes.Buffer
	read := make(chan struct{})
	go func() { _, _ = io.Copy(&buf, r); close(read) }()
	defer func() {
		os.Stderr = old
		_ = w.Close()
		select {
		case <-read:
		case <-time.After(10 * time.Second):
			t.Error("stderr was never closed")
		}
		_ = r.Close()
	}()
	fn()
	os.Stderr = old
	_ = w.Close()
	<-read
	return buf.String()
}

func TestExecuteJqFailsOnNull(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	page := cmdBlock(`ID=$(echo '{"id":"x"}' | jq -r .nope)`) + outBlock("") + cmdBlock("echo next") + outBlock("next")
	if err := runPage(t, page, nil, nil); err == nil {
		t.Fatal("want the run to stop on a jq lookup that finds nothing")
	}
	found := cmdBlock(`echo '{"id":"x"}' | jq -r .id`) + outBlock("x")
	if err := runPage(t, found, nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteRepeatUntilOutput(t *testing.T) {
	count := "n=$(( $(cat n 2>/dev/null || echo 0) + 1 )); echo $n > n\nif [ $n -ge 3 ]; then echo ready; else echo waiting; fi"
	page := "<!-- docs-run: command repeat-until-output -->\n```bash\n" + count + "\n```\n\n" + outBlock("ready")
	if err := runPage(t, page, nil, nil); err != nil {
		t.Fatalf("want ready on the third try: %v", err)
	}
	never := "<!-- docs-run: command repeat-until-output -->\n```bash\necho waiting\n```\n\n" + outBlock("ready")
	err := runPage(t, never, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "after 5 tries") {
		t.Fatalf("want a failure after 5 tries, got %v", err)
	}
}

func TestExecuteSkipsANamedCommandAndItsOutput(t *testing.T) {
	page := "<!-- docs-run: command name=a -->\n```bash\nfalse\n```\n\n" + outBlock("never") + cmdBlock("echo b") + outBlock("b")
	if err := runPage(t, page, []string{"a"}, nil); err != nil {
		t.Fatal(err)
	}
}
