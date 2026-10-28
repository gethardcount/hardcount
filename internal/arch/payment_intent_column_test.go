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

// TestPaymentIntentColumn_HoldsOnlyStripeIDs pins that no production code
// spells the synthetic "out_of_band:" PaymentIntent id an offline payment used
// to write into invoices.stripe_payment_intent_id (B3, #1370).
//
// Why: while that value lived in the column, every reader of it had to know
// the prefix, and a reader that checked only "has a PaymentIntent" offered a
// card refund on a bank transfer (HC-000068). An offline payment now sets
// paid_out_of_band and leaves the column empty, so that check is always
// right. Migration 00203 refuses the prefix in the column; this catches code
// that would still write it, or still look for it, before a database does.
// The check is on string literals in the AST, so comments that tell the
// history do not count.
func TestPaymentIntentColumn_HoldsOnlyStripeIDs(t *testing.T) {
	root := repoRoot(t)
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
				lit, ok := n.(*ast.BasicLit)
				if ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "out_of_band:") {
					t.Errorf("%s:%d: spells the synthetic out_of_band: PaymentIntent id. An offline payment sets "+
						"paid_out_of_band and records no PaymentIntent; read Invoice.PaidOutOfBand or HasCardPayment instead",
						filepath.ToSlash(rel), fset.Position(lit.Pos()).Line)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
