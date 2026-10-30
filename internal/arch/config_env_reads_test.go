package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configOwnedSettings are the settings config.Load parses or normalises.
// Read anywhere else, a setting gets its own parsing and its own fallback,
// and the two disagree: APP_ENV was read and normalised six ways, and the
// router used the default without a word for a HARDCOUNT_RATE_LIMIT_* value it
// could not parse while config refused other bad numbers. Code that needs
// one of these takes it from config.Config. Add a name here when config.Load
// takes over another setting.
var configOwnedSettings = map[string]bool{
	"APP_ENV":                              true,
	"RUN_MIGRATIONS_ON_BOOT":               true,
	"HARDCOUNT_RATE_LIMIT_GENERAL_PER_MIN": true,
	"HARDCOUNT_RATE_LIMIT_AUTH_PER_MIN":    true,
	"HARDCOUNT_RATE_LIMIT_INGEST_PER_SEC":  true,
	"INVOICE_GRACE_PERIOD":                 true,
	"SHUTDOWN_DRAIN_DELAY":                 true,
	"DB_MAX_OPEN_CONNS":                    true,
	"DB_MAX_IDLE_CONNS":                    true,
	"DB_CONN_MAX_LIFETIME_MIN":             true,
	"DB_CONN_MAX_IDLE_TIME_SEC":            true,
	"DB_PING_TIMEOUT_SEC":                  true,
	"DB_QUERY_TIMEOUT_MS":                  true,
}

// TestConfigOwnedSettings_ReadOnlyInConfig fails on any non-test call outside
// internal/config that passes one of those names as a string literal:
// os.Getenv("APP_ENV"), os.LookupEnv, or a package's own envOr helper.
func TestConfigOwnedSettings_ReadOnlyInConfig(t *testing.T) {
	root := repoRoot(t)
	configDir := filepath.Join(root, "internal", "config") + string(filepath.Separator)
	getenvCalls := 0
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
			inConfig := strings.HasPrefix(p, configDir)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Getenv" {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" {
						getenvCalls++
					}
				}
				if inConfig {
					return true
				}
				for _, arg := range call.Args {
					if name, ok := stringLit(arg); ok && configOwnedSettings[name] {
						rel, _ := filepath.Rel(root, p)
						t.Errorf("%s:%d reads %s; config.Load owns it, so take it from config.Config", rel, fset.Position(call.Pos()).Line, name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Guard against the walk silently matching nothing.
	if getenvCalls < 20 {
		t.Fatalf("found only %d os.Getenv calls under internal/ and cmd/; did the walk break?", getenvCalls)
	}
}
