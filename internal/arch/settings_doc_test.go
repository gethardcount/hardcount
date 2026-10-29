package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// envReaders are the calls that read an environment variable by name:
// os.Getenv and os.LookupEnv, and the helpers internal/config,
// internal/email and internal/testutil wrap them in. "n" is loadDBConfig's
// closure over intEnv; it counts only inside internal/config.
var envReaders = map[string]bool{
	"Getenv": true, "LookupEnv": true,
	"envOr": true, "intEnv": true, "boolEnv": true, "rateLimitEnv": true,
}

var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// TestSettingsDoc_ListsEverySettingTheCodeReads fails when a non-test file
// under cmd/ or internal/ reads an environment variable that docs/settings.md
// does not name in backticks. The page is the one list of settings, so a new
// setting ships with its row. A name passed through a constant, such as
// migrate.LockTimeoutEnv, is resolved from the package's string constants.
func TestSettingsDoc_ListsEverySettingTheCodeReads(t *testing.T) {
	root := repoRoot(t)
	page, err := os.ReadFile(filepath.Join(root, "docs", "settings.md"))
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "internal", "config") + string(filepath.Separator)

	read := map[string]string{} // setting -> first file:line that reads it
	for _, dir := range []string{"cmd", "internal"} {
		pkgs := map[string][]*ast.File{}
		fset := token.NewFileSet()
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				return err
			}
			pkgs[filepath.Dir(p)] = append(pkgs[filepath.Dir(p)], f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for pkgDir, files := range pkgs {
			consts := map[string]string{}
			for _, f := range files {
				for _, decl := range f.Decls {
					gd, ok := decl.(*ast.GenDecl)
					if !ok || gd.Tok != token.CONST {
						continue
					}
					for _, spec := range gd.Specs {
						vs := spec.(*ast.ValueSpec)
						for i, name := range vs.Names {
							if i < len(vs.Values) {
								if s, ok := stringLit(vs.Values[i]); ok {
									consts[name.Name] = s
								}
							}
						}
					}
				}
			}
			inConfig := strings.HasPrefix(pkgDir+string(filepath.Separator), configDir)
			for _, f := range files {
				ast.Inspect(f, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok || len(call.Args) == 0 {
						return true
					}
					var fn string
					switch c := call.Fun.(type) {
					case *ast.Ident:
						fn = c.Name
					case *ast.SelectorExpr:
						fn = c.Sel.Name
					}
					if !envReaders[fn] && (!inConfig || fn != "n") {
						return true
					}
					name, ok := stringLit(call.Args[0])
					if !ok {
						if id, isIdent := call.Args[0].(*ast.Ident); isIdent {
							name, ok = consts[id.Name]
						}
					}
					if !ok || !envName.MatchString(name) {
						return true
					}
					if _, seen := read[name]; !seen {
						rel, _ := filepath.Rel(root, fset.Position(call.Pos()).Filename)
						read[name] = rel + ":" + strconv.Itoa(fset.Position(call.Pos()).Line)
					}
					return true
				})
			}
		}
	}

	// Guard against the walk silently matching nothing.
	if len(read) < 40 {
		t.Fatalf("found only %d settings read under cmd/ and internal/; did the walk break?", len(read))
	}
	var missing []string
	for name, at := range read {
		if !strings.Contains(string(page), "`"+name+"`") {
			missing = append(missing, name+" (read at "+at+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("docs/settings.md does not list %s; add a row for it", m)
	}
}
