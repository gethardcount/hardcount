// hardcount-docs-run runs the commands of a Markdown page, in order, in one bash
// shell, and checks that each prints what the page says it prints. CI runs
// docs/first-invoice.md with it, so the tutorial cannot drift from the API.
//
//	go run ./cmd/hardcount-docs-run docs/first-invoice.md
//	go run ./cmd/hardcount-docs-run -skip 5a -replace acme-1=acme-2 docs/first-invoice.md
//
// The page marks each fenced block as a command or as the output of the
// command above it (see page.go). The run stops at the first command that
// fails, answers other than 2xx, finds nothing with jq, or prints something
// other than the page shows.
//
// -skip <name> leaves out a named command, as a reader who takes the other
// branch of a step does. -replace old=new changes the page's text before it
// runs, as a reader who follows "change acme-1 to acme-2" does, or to point
// it at another address. Each -skip and -replace must match the page, or
// the run stops before it starts.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var skips, replaces multi
	flag.Var(&skips, "skip", "leave out the command named `name` (repeatable)")
	flag.Var(&replaces, "replace", "replace `old=new` in every block before running (repeatable)")
	dir := flag.String("dir", ".", "directory the commands run in")
	tries := flag.Int("tries", 60, "how many times a repeat-until-output command runs, once a second, before the run fails")
	timeout := flag.Duration("timeout", 2*time.Minute, "longest one command may take")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: hardcount-docs-run [-skip name] [-replace old=new] page.md")
		os.Exit(2)
	}
	page := flag.Arg(0)
	src, err := os.ReadFile(page)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hardcount-docs-run: %v\n", err)
		os.Exit(2)
	}
	plan, err := makePlan(string(src), skips, replaces)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hardcount-docs-run: %s:\n%v\n", page, err)
		os.Exit(2)
	}
	if err := execute(page, plan, *dir, *tries, *timeout); err != nil {
		fmt.Fprintf(os.Stderr, "\nhardcount-docs-run: FAIL: %v\n", err)
		os.Exit(1)
	}
}

// A plan is the page's blocks with this pass's choices applied.
type plan struct {
	blocks  []block
	skipped map[string]bool
	outputs map[int][]int // command index -> output indexes checked this pass
}

func makePlan(src string, skips, replaces []string) (*plan, error) {
	blocks, err := parse(src)
	if err != nil {
		return nil, err
	}
	var errs []string
	for _, r := range replaces {
		old, repl, ok := strings.Cut(r, "=")
		if !ok || old == "" {
			errs = append(errs, fmt.Sprintf("-replace %q: want old=new", r))
			continue
		}
		hits := 0
		for k := range blocks {
			hits += strings.Count(blocks[k].text, old)
			blocks[k].text = strings.ReplaceAll(blocks[k].text, old, repl)
		}
		if hits == 0 {
			errs = append(errs, fmt.Sprintf("-replace %q: %q is in no block of the page", r, old))
		}
	}
	p := &plan{blocks: blocks, skipped: map[string]bool{}, outputs: map[int][]int{}}
	names := map[string]bool{}
	for _, b := range blocks {
		if b.kind == kindCommand && b.name != "" {
			names[b.name] = true
		}
	}
	for _, s := range skips {
		if !names[s] {
			errs = append(errs, fmt.Sprintf("-skip %q: the page has no command named %q", s, s))
		}
		p.skipped[s] = true
	}
	for k, b := range blocks {
		if b.kind != kindOutput {
			continue
		}
		if p.skipped[blocks[b.of].name] {
			continue
		}
		switch b.cond {
		case "if":
			if p.skipped[b.condName] {
				continue
			}
		case "unless":
			if !p.skipped[b.condName] {
				continue
			}
		}
		p.outputs[b.of] = append(p.outputs[b.of], k)
	}
	for k, b := range blocks {
		if b.kind != kindCommand || p.skipped[b.name] {
			continue
		}
		// Every command that runs is checked, so an output block cannot be
		// deleted, marker and all, without the run failing.
		if len(p.outputs[k]) == 0 {
			errs = append(errs, fmt.Sprintf("line %d: the command has no output block in this pass; add a <!-- docs-run: output --> block under it (an empty one if it prints nothing)", b.line))
		} else if b.repeat && len(p.outputs[k]) != 1 {
			errs = append(errs, fmt.Sprintf("line %d: a repeat-until-output command needs exactly one output block to check, has %d", b.line, len(p.outputs[k])))
		}
	}
	if len(errs) > 0 {
		return nil, errors.New(strings.Join(errs, "\n"))
	}
	return p, nil
}

func execute(page string, p *plan, dir string, tries int, timeout time.Duration) error {
	sh, err := startShell(dir)
	if err != nil {
		return err
	}
	defer sh.close()

	ran, checked := 0, 0
	for k, b := range p.blocks {
		if b.kind != kindCommand {
			continue
		}
		where := fmt.Sprintf("%s, %s:%d", b.label, page, b.line)
		if b.name != "" && p.skipped[b.name] {
			fmt.Printf("\n=== %s: skipped (-skip %s)\n", where, b.name)
			continue
		}
		fmt.Printf("\n=== %s\n%s\n--- output\n", where, indent(b.text))
		var got string
		attempt := 1
		for ; ; attempt++ {
			got, err = sh.run(b.text, timeout)
			if err != nil {
				fmt.Println(indent(got))
				return fmt.Errorf("%s: %v", where, err)
			}
			if !b.repeat || matchOutput(p.blocks[p.outputs[k][0]].text, got) {
				break
			}
			if attempt >= tries {
				fmt.Println(indent(got))
				return fmt.Errorf("%s: still not the page's output after %d tries", where, tries)
			}
			time.Sleep(time.Second)
		}
		fmt.Println(indent(got))
		if b.repeat {
			fmt.Printf("    (after %d tries)\n", attempt)
		}
		ran++
		for _, o := range p.outputs[k] {
			want := p.blocks[o].text
			if !matchOutput(want, got) {
				return fmt.Errorf("%s: the output is not what %s:%d shows.\nThe page shows:\n%s\nThe command printed:\n%s",
					where, page, p.blocks[o].line, indent(want), indent(got))
			}
			checked++
		}
	}
	var skipped []string
	for s := range p.skipped {
		skipped = append(skipped, s)
	}
	fmt.Printf("\nhardcount-docs-run: PASS: %s: %d commands ran, %d outputs matched", page, ran, checked)
	if len(skipped) > 0 {
		fmt.Printf(", skipped %s", strings.Join(skipped, ", "))
	}
	fmt.Println()
	return nil
}

func indent(s string) string {
	if s == "" {
		return "    (nothing)"
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
