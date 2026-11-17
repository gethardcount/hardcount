package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// respond.Error takes (status, code, message), two strings side by side, and
// a client switches on code. A literal code must be a snake_case word, which
// a message never is, so a swapped pair fails here. The type comes from the
// status (respond's errorType). The respond helpers (ValidationField,
// ConflictCoded, ...) fill the code for you and are the better call.
var respondErrorCode = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func TestRespondErrorCalls_PassCodeBeforeMessage(t *testing.T) {
	root := repoRoot(t)
	calls := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Error" {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "respond" || len(call.Args) != 5 {
					return true
				}
				calls++
				pos := fset.Position(call.Pos())
				if code, ok := stringLit(call.Args[3]); ok && !respondErrorCode.MatchString(code) {
					t.Errorf("%s:%d: respond.Error code %q is not a snake_case code (is the message in the code slot?)", rel, pos.Line, code)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls == 0 {
		t.Fatal("found no respond.Error calls — the scan is broken")
	}
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
