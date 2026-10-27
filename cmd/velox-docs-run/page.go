package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// A page is a Markdown file whose fenced blocks are all marked with an HTML
// comment on the line before the opening fence (blank lines may sit
// between). GitHub does not show the comment.
//
//	<!-- docs-run: command -->                     run this block
//	<!-- docs-run: command name=5a -->             a command a pass can --skip
//	<!-- docs-run: command repeat-until-output --> run it again, once a second, until its output block matches
//	<!-- docs-run: output -->                      what the command above prints
//	<!-- docs-run: output if=5a -->                checked only when command 5a ran in this pass
//	<!-- docs-run: output unless=5a -->            checked only when command 5a did not run
//
// An output block belongs to the nearest command block above it. In an
// output block, … stands for any text within one line.
//
// A block cannot drop out of the run:
//
//   - A fence with no marker is an error. The runner has no "skip this
//     block" marker at all.
//   - Any other code block GitHub shows (a fence in a blockquote, an
//     indented code block, <pre>) is an error, found with a CommonMark
//     parser. Write it as a fence at the start of a line.
//   - Every "You see:" must be followed by an output block, and must not
//     state a value in backticks. Describe a screen without "You see:".
//   - Every command that runs must have an output block (see makePlan).
//
// "export X=$(cmd)" is an error too: bash returns export's status, so a
// failing cmd would not stop the run.

type kind int

const (
	kindCommand kind = iota + 1
	kindOutput
)

// span is the lines of a fenced block, from its opening fence to its
// closing one, 1-based.
type span struct{ start, end int }

type block struct {
	line  int    // 1-based line of the opening fence
	label string // the page step it sits in, such as "step 5a"
	text  string // the content, without the list indent
	kind  kind

	// Command blocks.
	name   string // set by name=; a pass may skip it
	repeat bool   // repeat-until-output

	// Output blocks.
	of       int    // index of the command block it belongs to
	cond     string // "", "if" or "unless"
	condName string // the command name cond refers to
}

var (
	fenceRE  = regexp.MustCompile("^( *)(```+|~~~+)(.*)$")
	markerRE = regexp.MustCompile(`^ *<!--\s*docs-run:\s*(.*?)\s*-->\s*$`)
	// "1. **Load ready-made prices" or "**5a. Send the events yourself.**".
	stepRE = regexp.MustCompile(`^(?:(\d+)\. \*\*|\s*\*\*(\d+[a-z])\. )`)
	// The heading that starts the numbered steps ends "before you start".
	headingRE = regexp.MustCompile(`^#{1,6} (.+)$`)
	youSeeRE  = regexp.MustCompile(`(?i)you see:`)
	// export, local and the like return their own status, not that of a
	// $(...) or `...` in their value.
	hidingRE = regexp.MustCompile("^\\s*(?:export|local|declare|typeset|readonly)\\b[^#]*?\\b\\w+=\\S*?(?:\\$\\(|`)")
)

// parse reads a page into its blocks and checks the marker rules.
func parse(src string) ([]block, error) {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var (
		blocks  []block
		fences  []span // every fenced block found, marked or not
		errs    []string
		label   = "start of page"
		pending string // the marker waiting for its fence
		pendAt  int
		lastCmd = -1
		names   = map[string]int{}
	)
	fail := func(line int, format string, a ...any) {
		errs = append(errs, fmt.Sprintf("line %d: %s", line, fmt.Sprintf(format, a...)))
	}

	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if m := headingRE.FindStringSubmatch(ln); m != nil {
			label = strings.ToLower(m[1])
		}
		if m := stepRE.FindStringSubmatch(ln); m != nil {
			label = "step " + m[1] + m[2]
		}
		if m := markerRE.FindStringSubmatch(ln); m != nil {
			if pending != "" {
				fail(pendAt, "marker %q has no fenced block after it", pending)
			}
			pending, pendAt = m[1], i+1
			continue
		}
		if strings.Contains(ln, "docs-run:") {
			fail(i+1, "a docs-run marker must be a whole line: <!-- docs-run: ... -->")
			continue
		}
		fm := fenceRE.FindStringSubmatch(ln)
		if fm == nil {
			if pending != "" && strings.TrimSpace(ln) != "" {
				fail(pendAt, "marker %q is not followed by a fenced block", pending)
				pending = ""
			}
			continue
		}

		// A fenced block: find its end.
		indent, fence := len(fm[1]), fm[2]
		start := i + 1
		var body []string
		closed := false
		for i++; i < len(lines); i++ {
			if closesFence(lines[i], fence) {
				closed = true
				break
			}
			body = append(body, dedent(lines[i], indent))
		}
		fences = append(fences, span{start, i + 1})
		if !closed {
			fail(start, "fenced block is never closed")
			break
		}
		b := block{line: start, label: label, text: strings.Join(body, "\n")}
		if pending == "" {
			fail(start, "fenced block has no docs-run marker; mark it <!-- docs-run: command --> or <!-- docs-run: output -->")
			continue
		}
		marker := pending
		pending = ""
		fields := strings.Fields(marker)
		switch fields[0] {
		case "command":
			b.kind = kindCommand
			for _, f := range fields[1:] {
				switch {
				case strings.HasPrefix(f, "name="):
					b.name = strings.TrimPrefix(f, "name=")
					if b.name == "" {
						fail(start, "name= is empty")
					} else if prev, dup := names[b.name]; dup {
						fail(start, "command name %q is already used at line %d", b.name, blocks[prev].line)
					}
				case f == "repeat-until-output":
					b.repeat = true
				default:
					fail(start, "unknown command option %q", f)
				}
			}
			if b.name != "" {
				names[b.name] = len(blocks)
			}
			for k, ln := range strings.Split(b.text, "\n") {
				if hidingRE.MatchString(ln) {
					fail(start+1+k, "%q hides a failure of the command inside; write X=$(...) on its own line, then export X", strings.TrimSpace(ln))
				}
			}
			lastCmd = len(blocks)
		case "output":
			b.kind = kindOutput
			b.of = lastCmd
			if lastCmd < 0 {
				fail(start, "output block has no command block above it")
			}
			for _, f := range fields[1:] {
				k, v, ok := strings.Cut(f, "=")
				if !ok || (k != "if" && k != "unless") || v == "" || b.cond != "" {
					fail(start, "unknown output option %q (want one if=<name> or unless=<name>)", f)
					continue
				}
				b.cond, b.condName = k, v
			}
		default:
			fail(start, "unknown marker %q (want command or output)", marker)
		}
		blocks = append(blocks, b)
	}
	if pending != "" {
		fail(pendAt, "marker %q has no fenced block after it", pending)
	}

	for _, b := range blocks {
		if b.kind == kindOutput && b.cond != "" {
			if _, ok := names[b.condName]; !ok {
				fail(b.line, "output %s=%s names no command", b.cond, b.condName)
			}
		}
	}
	errs = append(errs, checkYouSee(lines, blocks)...)
	errs = append(errs, checkCodeBlocks(strings.Join(lines, "\n"), fences)...)

	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return blocks, nil
}

// checkYouSee holds every stated result to an output block. Every "You
// see:" must be followed by an output block, after the rest of its
// paragraph. A "You see:" whose paragraph names a value in backticks is
// refused: the value must be in the output block.
func checkYouSee(lines []string, blocks []block) []string {
	outputAt := map[int]bool{} // fence line -> is an output block
	for _, b := range blocks {
		outputAt[b.line] = b.kind == kindOutput
	}
	var errs []string
	inFence := false
	var fence string
	for i := 0; i < len(lines); i++ {
		if m := fenceRE.FindStringSubmatch(lines[i]); m != nil {
			if !inFence {
				inFence, fence = true, m[2]
				continue
			}
			if closesFence(lines[i], fence) {
				inFence = false
			}
			continue
		}
		if inFence {
			continue
		}
		loc := youSeeRE.FindStringIndex(lines[i])
		if loc == nil {
			continue
		}
		// The rest of the paragraph after "You see:".
		para := lines[i][loc[1]:]
		j := i + 1
		for ; j < len(lines) && strings.TrimSpace(lines[j]) != "" && fenceRE.FindStringSubmatch(lines[j]) == nil; j++ {
			para += " " + strings.TrimSpace(lines[j])
		}
		para = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(para), "**"))
		if strings.Contains(para, "`") {
			errs = append(errs, fmt.Sprintf("line %d: \"You see:\" states a value in backticks; put the result in a <!-- docs-run: output --> block so it is checked", i+1))
			continue
		}
		// The next non-blank, non-marker line must open an output block.
		for ; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" || markerRE.MatchString(lines[j]) {
				continue
			}
			break
		}
		if j >= len(lines) || !outputAt[j+1] {
			errs = append(errs, fmt.Sprintf("line %d: \"You see:\" is not followed by a <!-- docs-run: output --> block", i+1))
		}
	}
	return errs
}

// checkCodeBlocks finds every code block a CommonMark parser sees, as
// GitHub shows the page, and refuses any that is not one of the fenced
// blocks parse found. (A fenced block without a marker has its own error.)
func checkCodeBlocks(src string, fences []span) []string {
	source := []byte(src)
	lineAt := func(offset int) int { return bytes.Count(source[:offset], []byte("\n")) + 1 }
	read := func(line int) bool {
		for _, f := range fences {
			if line >= f.start && line <= f.end {
				return true
			}
		}
		return false
	}
	var errs []string
	doc := goldmark.New().Parser().Parse(text.NewReader(source))
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		line := 0
		switch n := n.(type) {
		case *ast.FencedCodeBlock:
			// An empty block with no info string has no line to name, and
			// nothing to run.
			if n.Info != nil {
				line = lineAt(n.Info.Segment.Start)
			} else if n.Lines().Len() > 0 {
				line = lineAt(n.Lines().At(0).Start)
			}
		case *ast.CodeBlock:
			line = lineAt(n.Lines().At(0).Start)
		case *ast.HTMLBlock:
			var raw []byte
			for k := 0; k < n.Lines().Len(); k++ {
				seg := n.Lines().At(k)
				raw = append(raw, seg.Value(source)...)
			}
			if !bytes.Contains(bytes.ToLower(raw), []byte("<pre")) {
				return ast.WalkContinue, nil
			}
			line = lineAt(n.Lines().At(0).Start)
		default:
			return ast.WalkContinue, nil
		}
		if line > 0 && !read(line) {
			errs = append(errs, fmt.Sprintf("line %d: a code block the runner does not read; write it as a fenced block (```) at the start of the line, after a docs-run marker", line))
		}
		return ast.WalkSkipChildren, nil
	})
	return errs
}

// closesFence reports whether line closes a block opened with fence: the
// same character, at least as many, and nothing else on the line.
func closesFence(line, fence string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, fence) && strings.Trim(t, fence[:1]) == ""
}

// dedent removes up to n leading spaces: the list indent of a block inside
// a numbered step.
func dedent(s string, n int) string {
	for k := 0; k < n && strings.HasPrefix(s, " "); k++ {
		s = s[1:]
	}
	return s
}

// matchOutput reports whether got is what the page shows. Lines are
// compared without trailing spaces, and trailing blank lines are ignored.
// … in want stands for any text within one line.
func matchOutput(want, got string) bool {
	return outputPattern(want).MatchString(normalize(got))
}

func outputPattern(want string) *regexp.Regexp {
	parts := strings.Split(normalize(want), "…")
	for k := range parts {
		parts[k] = regexp.QuoteMeta(parts[k])
	}
	return regexp.MustCompile(`\A` + strings.Join(parts, `[^\n]*`) + `\z`)
}

func normalize(s string) string {
	ls := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	for k := range ls {
		ls[k] = strings.TrimRight(ls[k], " \t")
	}
	return strings.TrimRight(strings.Join(ls, "\n"), "\n")
}
