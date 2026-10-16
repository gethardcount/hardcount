package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A civil-day date shown to a person ("Jun 1, 2026") must be rendered in an
// explicit zone, the billing or tenant zone, through .In(loc) (ADR-076). A bare
// t.Format(...) renders in the process zone, and t.UTC().Format(...) prints a
// customer's calendar date a day early or late; the ADR-074/075 arc fixed about
// eight bugs of that shape. A genuine UTC, instant or filename case carries a
// same-line //tz:ok comment.

// civilDayLayout is a time layout with a date part (2006, January or "Jan ")
// and no clock part. A layout with ':' is a timestamp, not a calendar day.
var civilDayLayout = regexp.MustCompile(`2006|January|Jan `)

func isCivilDayLayout(s string) bool {
	return civilDayLayout.MatchString(s) && !strings.Contains(s, ":")
}

type goFile struct {
	rel   string
	fset  *token.FileSet
	ast   *ast.File
	lines []string
}

func TestCivilDayFormat_UsesExplicitZone(t *testing.T) {
	root := repoRoot(t)
	var files []goFile
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, src, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			files = append(files, goFile{rel: rel, fset: fset, ast: f, lines: strings.Split(string(src), "\n")})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// Names bound to a civil-day layout (`const dateLayout = "Jan 2, 2006"`), so
	// t.Format(dateLayout) is checked the same as t.Format("Jan 2, 2006").
	layoutNames := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ValueSpec:
				for i, name := range n.Names {
					if i < len(n.Values) {
						if s, ok := stringLit(n.Values[i]); ok && isCivilDayLayout(s) {
							layoutNames[name.Name] = true
						}
					}
				}
			case *ast.AssignStmt:
				if n.Tok == token.ASSIGN && len(n.Lhs) == len(n.Rhs) {
					for i, lhs := range n.Lhs {
						if id, ok := lhs.(*ast.Ident); ok {
							if s, ok := stringLit(n.Rhs[i]); ok && isCivilDayLayout(s) {
								layoutNames[id.Name] = true
							}
						}
					}
				}
			}
			return true
		})
	}

	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f.rel, "_test.go") {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Format" {
				return true
			}
			layout := ""
			if s, ok := stringLit(call.Args[0]); ok && isCivilDayLayout(s) {
				layout = s
			} else if id, ok := call.Args[0].(*ast.Ident); ok && layoutNames[id.Name] {
				layout = id.Name
			} else {
				return true
			}
			checked++
			line := f.fset.Position(sel.Sel.Pos()).Line
			text := f.lines[line-1]
			if strings.Contains(text, ".In(") || strings.Contains(text, "//tz:ok") {
				return true
			}
			t.Errorf("%s:%d: civil-day .Format(%s) without an explicit zone (ADR-076).\n"+
				"\tRender it in the billing/tenant zone, e.g. t.In(domain.LoadLocationOrUTC(inv.BillingTimezone)).Format(\"2006-01-02\").\n"+
				"\tA genuine UTC/instant/filename case may carry a same-line //tz:ok comment.",
				f.rel, line, layout)
			return true
		})
	}
	if checked == 0 {
		t.Fatal("found no civil-day .Format calls — the scan is broken")
	}
}

// The process runs in UTC (ADR-075). Without the pin, pgx decodes timestamptz
// in the host zone and the API leaks a host offset.
func TestServerPinsProcessToUTC(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "velox", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "time.Local = time.UTC") {
		t.Error("cmd/velox/main.go must pin the process to UTC (time.Local = time.UTC), ADR-075")
	}
}
