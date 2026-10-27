package main

import (
	"net/http"
	"net/http/httptest"
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
	page := cmdBlock("X=hello\nsay() { echo \"$1 $X\"; }") +
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
	cases := map[string]string{
		"failing command":      "false\necho after",
		"failing part of pipe": "false | cat",
		"unset variable":       "echo \"$DOCS_RUN_NOT_SET\"",
		"failing substitution": "X=$(false)",
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			if err := runPage(t, cmdBlock(script)+cmdBlock("echo next"), nil, nil); err == nil {
				t.Fatal("want the run to stop")
			}
		})
	}
}

// A command that reads stdin gets nothing, and the next block still runs.
func TestExecuteBlockStdinIsEmpty(t *testing.T) {
	page := cmdBlock("cat") + cmdBlock("echo next") + outBlock("next")
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
		page := cmdBlock(`ID=$(curl -s "BASE/`+path+`")`) + cmdBlock("echo next")
		if err := runPage(t, page, nil, []string{"BASE=" + srv.URL}); err == nil {
			t.Fatalf("%s: want the run to stop", path)
		}
	}
}

func TestExecuteJqFailsOnNull(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not installed")
	}
	page := cmdBlock(`ID=$(echo '{"id":"x"}' | jq -r .nope)`) + cmdBlock("echo next")
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
