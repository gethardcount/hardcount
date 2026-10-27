package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The page CI runs parses, and every block in it is a command or an output.
func TestFirstInvoicePageParses(t *testing.T) {
	src, err := os.ReadFile("../../docs/first-invoice.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := parse(string(src))
	if err != nil {
		t.Fatalf("docs/first-invoice.md:\n%v", err)
	}
	fences := 0
	for _, ln := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			fences++
		}
	}
	if len(blocks)*2 != fences {
		t.Fatalf("%d blocks parsed from %d fence lines", len(blocks), fences)
	}
	// Each pass the CI job runs is a valid plan for the page.
	passes := ciPasses(t)
	if len(passes) != 2 {
		t.Fatalf("want the two passes of the golden-path job in ci.yml, found %d", len(passes))
	}
	for _, args := range passes {
		var skips, replaces []string
		for k := 0; k+1 < len(args); k += 2 {
			switch args[k] {
			case "-skip":
				skips = append(skips, args[k+1])
			case "-replace":
				replaces = append(replaces, args[k+1])
			default:
				t.Fatalf("ci.yml: unexpected argument %q", args[k])
			}
		}
		if _, err := makePlan(string(src), skips, replaces); err != nil {
			t.Fatalf("ci.yml pass %v:\n%v", args, err)
		}
	}
}

// ciPasses returns the arguments of each velox-docs-run call in ci.yml that
// runs docs/first-invoice.md.
func ciPasses(t *testing.T) [][]string {
	t.Helper()
	ci, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`velox-docs-run"((?: +\S+)*?) +docs/first-invoice\.md\s*$`)
	var passes [][]string
	for _, ln := range strings.Split(string(ci), "\n") {
		if m := re.FindStringSubmatch(ln); m != nil {
			passes = append(passes, strings.Fields(m[1]))
		}
	}
	return passes
}

// The quick start runs the newest release, and a reader on that release
// needs the page as it was then. The page names the release quickstart.sh
// starts, so a release that bumps it must update the page too.
func TestFirstInvoicePageNamesTheQuickStartRelease(t *testing.T) {
	qs, err := os.ReadFile("../../deploy/compose/quickstart.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^VELOX_VERSION="(v[0-9.]+)"$`).FindSubmatch(qs)
	if m == nil {
		t.Fatal("deploy/compose/quickstart.sh: no VELOX_VERSION line")
	}
	release := string(m[1])
	src, err := os.ReadFile("../../docs/first-invoice.md")
	if err != nil {
		t.Fatal(err)
	}
	link := "https://github.com/getvelox/velox/blob/" + release + "/docs/first-invoice.md"
	if !strings.Contains(string(src), link) {
		t.Fatalf("docs/first-invoice.md does not link the page as of the quick start's release %s (%s)", release, link)
	}
	for _, v := range regexp.MustCompile(`\bv\d+\.\d+\.\d+\b`).FindAllString(string(src), -1) {
		if v != release {
			t.Errorf("docs/first-invoice.md names %s; the quick start starts %s", v, release)
		}
	}
}

// Step 8 prints a dashboard link. CI only echoes it, so check here that the
// dashboard has that page.
func TestFirstInvoiceDashboardLinksExist(t *testing.T) {
	src, err := os.ReadFile("../../docs/first-invoice.md")
	if err != nil {
		t.Fatal(err)
	}
	routes, err := os.ReadFile("../../web-v2/src/main.tsx")
	if err != nil {
		t.Fatal(err)
	}
	blocks, err := parse(string(src))
	if err != nil {
		t.Fatal(err)
	}
	link := regexp.MustCompile(`^http://localhost:8080(/\S*)$`)
	found := 0
	for _, b := range blocks {
		if b.kind != kindOutput {
			continue
		}
		for _, ln := range strings.Split(b.text, "\n") {
			m := link.FindStringSubmatch(ln)
			if m == nil {
				continue
			}
			found++
			// /invoices/vlx_inv_… is the route /invoices/:<param>.
			var pat []string
			for _, seg := range strings.Split(m[1], "/") {
				if strings.HasPrefix(seg, "vlx_") {
					pat = append(pat, ":[A-Za-z]+")
				} else {
					pat = append(pat, regexp.QuoteMeta(seg))
				}
			}
			route := regexp.MustCompile(`<Route path="` + strings.Join(pat, "/") + `"`)
			if !route.Match(routes) {
				t.Errorf("docs/first-invoice.md:%d shows %s, and web-v2/src/main.tsx has no route for it", b.line, ln)
			}
		}
	}
	if found == 0 {
		t.Fatal("no dashboard link found in the page's output blocks")
	}
}

func TestParseRefusesUnmarkedAndBadMarkers(t *testing.T) {
	cases := []struct {
		name, page, want string
	}{
		{"unmarked block", "Run it.\n\n```bash\necho hi\n```\n", "has no docs-run marker"},
		{"unmarked output block", "<!-- docs-run: command -->\n```bash\necho hi\n```\n\n```text\nhi\n```\n", "has no docs-run marker"},
		{"marker with no block", "<!-- docs-run: command -->\n\nSome words.\n", "is not followed by a fenced block"},
		{"marker at the end", "<!-- docs-run: command -->\n", "has no fenced block after it"},
		{"unknown marker", "<!-- docs-run: skip -->\n```bash\necho hi\n```\n", "unknown marker"},
		{"unknown command option", "<!-- docs-run: command later -->\n```bash\necho hi\n```\n", "unknown command option"},
		{"output first", "<!-- docs-run: output -->\n```text\nhi\n```\n", "no command block above it"},
		{"duplicate name", "<!-- docs-run: command name=a -->\n```bash\necho 1\n```\n<!-- docs-run: command name=a -->\n```bash\necho 2\n```\n", "already used"},
		{"if names nothing", "<!-- docs-run: command -->\n```bash\necho 1\n```\n<!-- docs-run: output if=zz -->\n```text\n1\n```\n", "names no command"},
		{"marker inside text", "Here <!-- docs-run: command --> it is.\n", "must be a whole line"},
		{"unclosed block", "<!-- docs-run: command -->\n```bash\necho hi\n", "never closed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(c.page)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

func TestYouSeeMustBeChecked(t *testing.T) {
	cmd := "<!-- docs-run: command -->\n```bash\necho hi\n```\n\n"
	cases := []struct {
		name, page, want string
	}{
		{"value in backticks", cmd + "**You see:** `hi`\n", "states a value in backticks"},
		{"value in backticks on the next line", cmd + "**You see:** the word\n`hi`.\n", "states a value in backticks"},
		{"colon, then nothing", cmd + "**You see:**\n\nMore words.\n", "is not followed by"},
		{"colon, then a command", cmd + "Then you see:\n\n" + cmd, "is not followed by"},
		{"colon, then an output", cmd + "**You see:**\n\n<!-- docs-run: output -->\n```text\nhi\n```\n", ""},
		{"words, then an output", cmd + "**You see:** the plan ID.\n\n<!-- docs-run: output -->\n```text\nhi\n```\n", ""},
		// A result stated in words is a result too: dropping its output
		// block must not pass.
		{"words, then nothing", cmd + "**You see:** the plan ID.\n\nMore words.\n", "is not followed by"},
		{"a screen in words", cmd + "**You see:** the invoice, with two lines.\n", "is not followed by"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parse(c.page)
			if c.want == "" {
				if err != nil {
					t.Fatalf("want no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error with %q, got %v", c.want, err)
			}
		})
	}
}

// GitHub shows each of these as code a reader can copy, but none is a
// fenced block at the start of a line. The runner must refuse them, not
// leave them out of the run.
func TestParseRefusesCodeTheRunnerCannotSee(t *testing.T) {
	cases := map[string]string{
		"fence in a blockquote":            "Tip:\n\n> ```bash\n> false\n> ```\n",
		"indented code block":              "Run:\n\n    false\n",
		"indented code in a list item":     "1. **Do it.**\n\n       false\n",
		"pre block":                        "<pre><code>false</code></pre>\n",
		"tab-indented fence in a list":     "1. **Do it.**\n\n\t```bash\n\tfalse\n\t```\n",
		"fence after a marker, in a quote": "<!-- docs-run: command -->\n> ```bash\n> false\n> ```\n",
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parse(page)
			if err == nil || !strings.Contains(err.Error(), "code block the runner does not read") {
				t.Fatalf("want an error naming the code block, got %v", err)
			}
		})
	}
	// Prose indented under a list item, and a nested list, are not code.
	ok := "1. **Do it.**\n   More about it.\n\n   - a point\n     that wraps\n"
	if _, err := parse(ok); err != nil {
		t.Fatalf("want no error, got %v", err)
	}
}

// bash returns the status of export, not of the command inside $(...), so
// "export X=$(cmd)" would carry on after cmd fails.
func TestParseRefusesASubstitutionThatHidesAFailure(t *testing.T) {
	for _, script := range []string{
		"export X=$(jq -r .k f.json)",
		"f() {\n  local x=\"$(false)\"\n}",
		"readonly Y=`false`",
		"declare -x Z=$(false)",
	} {
		_, err := parse("<!-- docs-run: command -->\n```bash\n" + script + "\n```\n")
		if err == nil || !strings.Contains(err.Error(), "hides a failure") {
			t.Errorf("%q: want an error, got %v", script, err)
		}
	}
	for _, script := range []string{"X=$(false)\nexport X", "export X=plain", "export VELOX=http://localhost:8080"} {
		if _, err := parse("<!-- docs-run: command -->\n```bash\n" + script + "\n```\n"); err != nil {
			t.Errorf("%q: want no error, got %v", script, err)
		}
	}
}

func TestParseListIndentAndLabels(t *testing.T) {
	page := "## Steps\n\n1. **Do it.**\n\n   <!-- docs-run: command -->\n   ```bash\n   echo a \\\n     b\n   ```\n\n   **5a. Or this.**\n\n   <!-- docs-run: command name=5a -->\n   ```bash\n   echo c\n   ```\n"
	blocks, err := parse(page)
	if err != nil {
		t.Fatal(err)
	}
	if blocks[0].text != "echo a \\\n  b" || blocks[0].label != "step 1" {
		t.Fatalf("block 0: %q %q", blocks[0].text, blocks[0].label)
	}
	if blocks[1].name != "5a" || blocks[1].label != "step 5a" {
		t.Fatalf("block 1: %q %q", blocks[1].name, blocks[1].label)
	}
}

func TestMatchOutput(t *testing.T) {
	cases := []struct {
		want, got string
		ok        bool
	}{
		{"vlx_pln_…", "vlx_pln_abc", true},
		{"vlx_pln_…", "null", false},
		{"vlx_pln_…", "vlx_pln_a\nvlx_pln_b", false}, // … stays within one line
		{`"id": "vlx_sub_…"`, `"id": null`, false},
		{`"total_cents": 600,`, `"total_cents": 601,`, false},
		{"a\nb", "a  \nb\n\n", true}, // trailing spaces and blank lines
		{"a\nb", "a\nb\nc", false},   // an extra line fails
		{"a.b", "axb", false},        // the page's text is literal
	}
	for _, c := range cases {
		if got := matchOutput(c.want, c.got); got != c.ok {
			t.Errorf("matchOutput(%q, %q) = %v, want %v", c.want, c.got, got, c.ok)
		}
	}
}

const branchPage = "<!-- docs-run: command name=a -->\n```bash\necho a\n```\n\n" +
	"<!-- docs-run: output -->\n```text\na\n```\n\n" +
	"<!-- docs-run: command -->\n```bash\necho b\n```\n\n" +
	"<!-- docs-run: output unless=a -->\n```text\nb alone\n```\n\n" +
	"Did a first? Then you see:\n\n" +
	"<!-- docs-run: output if=a -->\n```text\nb after a\n```\n"

func TestMakePlanOutputsPerPass(t *testing.T) {
	p, err := makePlan(branchPage, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.outputs[2]; len(got) != 1 || p.blocks[got[0]].text != "b after a" {
		t.Fatalf("with a: outputs %v", got)
	}
	p, err = makePlan(branchPage, []string{"a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.outputs[2]; len(got) != 1 || p.blocks[got[0]].text != "b alone" {
		t.Fatalf("without a: outputs %v", got)
	}
}

func TestMakePlanRefusesWhatMatchesNothing(t *testing.T) {
	if _, err := makePlan(branchPage, []string{"zz"}, nil); err == nil || !strings.Contains(err.Error(), "no command named") {
		t.Fatalf("-skip zz: %v", err)
	}
	if _, err := makePlan(branchPage, nil, []string{"nowhere=x"}); err == nil || !strings.Contains(err.Error(), "is in no block") {
		t.Fatalf("-replace nowhere=x: %v", err)
	}
	if _, err := makePlan(branchPage, nil, []string{"noequals"}); err == nil {
		t.Fatal("-replace noequals: want an error")
	}
	repeat := "<!-- docs-run: command repeat-until-output -->\n```bash\necho a\n```\n" +
		"<!-- docs-run: output -->\n```text\na\n```\n<!-- docs-run: output -->\n```text\na\n```\n"
	if _, err := makePlan(repeat, nil, nil); err == nil || !strings.Contains(err.Error(), "exactly one output") {
		t.Fatalf("repeat with two outputs: %v", err)
	}
}

// Every command that runs is checked against the page: deleting an output
// block, marker and all, must not pass.
func TestMakePlanRefusesACommandWithNoOutput(t *testing.T) {
	alone := "<!-- docs-run: command -->\n```bash\necho a\n```\n"
	if _, err := makePlan(alone, nil, nil); err == nil || !strings.Contains(err.Error(), "no output block") {
		t.Fatalf("command with no output: %v", err)
	}
	// Only "if=a" output: fine with a, missing when a is skipped.
	onlyIf := "<!-- docs-run: command name=a -->\n```bash\necho a\n```\n<!-- docs-run: output -->\n```text\na\n```\n" +
		"<!-- docs-run: command -->\n```bash\necho b\n```\n<!-- docs-run: output if=a -->\n```text\nb\n```\n"
	if _, err := makePlan(onlyIf, nil, nil); err != nil {
		t.Fatalf("with a: %v", err)
	}
	if _, err := makePlan(onlyIf, []string{"a"}, nil); err == nil || !strings.Contains(err.Error(), "no output block") {
		t.Fatalf("without a: %v", err)
	}
	// A skipped command needs nothing.
	skipped := "<!-- docs-run: command name=a -->\n```bash\necho a\n```\n"
	if _, err := makePlan(skipped, []string{"a"}, nil); err != nil {
		t.Fatalf("skipped: %v", err)
	}
}
