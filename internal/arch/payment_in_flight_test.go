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

// paymentInFlightHome is the one file allowed to call
// InvoicePaymentStatus.IsInFlight: it defines it, and Invoice.PaymentInFlight
// wraps it there.
const paymentInFlightHome = "internal/domain/invoice.go"

// TestPaymentInFlight_AskedOfTheInvoice pins that every "may a payment be open
// on this invoice?" check asks domain.Invoice.PaymentInFlight, never the
// payment status's IsInFlight directly.
//
// Why: payment_status need not be the whole answer. A fact about an open
// payment kept elsewhere on the invoice row joins PaymentInFlight, once; the
// #976 fix is one, a record of each charge written before Velox calls Stripe.
// A caller that read the status directly would miss such a fact and keep
// voiding, writing off, crediting or canceling while a payment may still take
// money. The check is on the AST, so comments and strings that name IsInFlight
// do not count.
func TestPaymentInFlight_AskedOfTheInvoice(t *testing.T) {
	root := repoRoot(t)
	homeCalls := 0
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
			rel = filepath.ToSlash(rel)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "IsInFlight" || len(call.Args) != 0 {
					return true
				}
				if rel == paymentInFlightHome {
					homeCalls++
					return true
				}
				t.Errorf("%s:%d: calls IsInFlight on a payment status — ask the invoice instead (inv.PaymentInFlight()), "+
					"so a payment recorded on the invoice row outside payment_status still counts as in flight",
					rel, fset.Position(call.Pos()).Line)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if homeCalls == 0 {
		t.Fatalf("found no IsInFlight call in %s — the scan is broken, or PaymentInFlight moved", paymentInFlightHome)
	}
}
