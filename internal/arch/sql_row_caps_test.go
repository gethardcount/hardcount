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

// A list route pages with limit and offset, and its total counts every row
// (docs/api/pagination.md). A fixed `LIMIT n` in the SQL behind a list
// cuts it short with no sign: total counts only what was read, and paging
// never reaches the rest. Plans, meters, test clocks and price overrides
// stopped at 500 that way; payment methods at 100; a test clock's customers
// and subscriptions at 1000.
//
// This pins every function (outside tests) whose SQL has a literal LIMIT
// above 1. None of them backs a list route. A new one fails until it is
// added here with the reason it is not a list's cap.
var sqlRowCaps = map[string]string{
	"internal/analytics/usage.go Handler.usageAnalytics":                       "the analytics chart's top 10 meters",
	"internal/audit/audit.go Logger.SimClocks":                                 "the audit page's clock filter menu: the 100 most recent clocks",
	"internal/invoice/postgres.go PostgresStore.ListCustomerDataInvalidErrors": "a tax retry fan-out that retries the oldest 1000, not a list route",
	"internal/invoice/postgres.go PostgresStore.ListProviderConfigErrors":      "a tax retry fan-out that retries the oldest 1000, not a list route",
	"internal/subscription/postgres.go PostgresStore.ListLiveByCustomer":       "usage ingest's read of a customer's live subscriptions, not a list route",
	"internal/testclock/postgres.go PostgresStore.ListAllAdvancing":            "start-up recovery of clocks that were advancing, not a list route",
}

var sqlLimitLiteral = regexp.MustCompile(`(?i)\bLIMIT\s+(\d+)`)

func sqlRowCapFuncs(t *testing.T, root string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					lit, ok := n.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					for _, m := range sqlLimitLiteral.FindAllStringSubmatch(lit.Value, -1) {
						if n, _ := strconv.Atoi(m[1]); n > 1 {
							found[rel+" "+funcKey(fd)] = true
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", top, err)
		}
	}
	return found
}

func TestSQLRowCaps_NoneBehindAList(t *testing.T) {
	found := sqlRowCapFuncs(t, repoRoot(t))
	var missing, stale []string
	for k := range found {
		if _, ok := sqlRowCaps[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range sqlRowCaps {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, k := range missing {
		t.Errorf("%s has a fixed LIMIT above 1 in its SQL. If a list route reads it, the list stops there with no sign: "+
			"page it in SQL with the request's limit and offset and count the total, or read it whole. "+
			"If it backs no list route, add it to sqlRowCaps with the reason.", k)
	}
	for _, k := range stale {
		t.Errorf("%s is listed in sqlRowCaps but has no fixed LIMIT above 1: remove it", k)
	}
}
