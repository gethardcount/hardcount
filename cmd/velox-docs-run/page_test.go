package main

import (
	"os"
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
	names := map[string]bool{}
	for _, b := range blocks {
		if b.kind == kindCommand && b.name != "" {
			names[b.name] = true
		}
	}
	// The CI job skips 5a in its second pass.
	if !names["5a"] || !names["5b"] {
		t.Fatalf("want commands named 5a and 5b, have %v", names)
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
		{"a screen in words", cmd + "**You see:** the invoice, with two lines.\n", ""},
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
	"<!-- docs-run: command -->\n```bash\necho b\n```\n\n" +
	"<!-- docs-run: output unless=a -->\n```text\nb alone\n```\n\n" +
	"Did a first? Then you see:\n\n" +
	"<!-- docs-run: output if=a -->\n```text\nb after a\n```\n"

func TestMakePlanOutputsPerPass(t *testing.T) {
	p, err := makePlan(branchPage, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.outputs[1]; len(got) != 1 || p.blocks[got[0]].text != "b after a" {
		t.Fatalf("with a: outputs %v", got)
	}
	p, err = makePlan(branchPage, []string{"a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.outputs[1]; len(got) != 1 || p.blocks[got[0]].text != "b alone" {
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
	repeat := "<!-- docs-run: command repeat-until-output -->\n```bash\necho a\n```\n"
	if _, err := makePlan(repeat, nil, nil); err == nil || !strings.Contains(err.Error(), "exactly one output") {
		t.Fatalf("repeat with no output: %v", err)
	}
}
