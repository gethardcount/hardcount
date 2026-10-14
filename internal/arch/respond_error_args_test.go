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

// respondErrorTypes is every error `type` the API sends. respond.Error takes
// (status, errType, code, message) as four loose values, and nine calls once
// passed them wrong: three shifted by one (the message text in `code`, the
// field name in `message`, a made-up word in `type`), six with "invalid_state"
// as the type. A client switches on type and code, so either mistake makes an
// error unrecognisable. The respond helpers (ValidationField, ConflictCoded,
// ...) fill type for you and are the better call.
var respondErrorTypes = map[string]bool{
	"api_error":             true,
	"invalid_request_error": true,
	"authentication_error":  true,
	"rate_limit_error":      true,
}

var respondErrorCode = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func TestRespondErrorCalls_PassTypeAndCodeInOrder(t *testing.T) {
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
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "respond" || len(call.Args) != 6 {
					return true
				}
				calls++
				pos := fset.Position(call.Pos())
				typ, ok := stringLit(call.Args[3])
				if !ok {
					t.Errorf("%s:%d: respond.Error type must be a string literal", rel, pos.Line)
				} else if !respondErrorTypes[typ] {
					t.Errorf("%s:%d: respond.Error type %q is not an API error type (arguments are status, type, code, message)", rel, pos.Line, typ)
				}
				if code, ok := stringLit(call.Args[4]); ok && !respondErrorCode.MatchString(code) {
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
