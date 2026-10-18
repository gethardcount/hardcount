package payment

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreatePaymentIntent_OnlyAfterTheChargeRecord pins ADR-127's one write
// site (playbook gate 11): Stripe's CreatePaymentIntent is called from one
// function in the codebase, chargeInvoice (its breaker and no-breaker
// branches), and there only after beginChargeRecord, which writes the charge
// record and fails closed. A second caller, or a call moved above the record,
// would send a charge that nothing on the invoice says was sent: the #976
// double charge.
//
// The check is on the AST of every non-test .go file under internal/ and cmd/,
// so comments and strings that name the method do not count.
func TestCreatePaymentIntent_OnlyAfterTheChargeRecord(t *testing.T) {
	root := filepath.Join("..", "..")
	type call struct {
		file, fn string
		pos      token.Pos
	}
	var creates, begins []call
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
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					c, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					var name string
					switch f := c.Fun.(type) {
					case *ast.SelectorExpr:
						name = f.Sel.Name
					case *ast.Ident:
						name = f.Name
					}
					switch name {
					case "CreatePaymentIntent":
						creates = append(creates, call{p, fn.Name.Name, c.Pos()})
					case "beginChargeRecord":
						begins = append(begins, call{p, fn.Name.Name, c.Pos()})
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(creates) == 0 {
		t.Fatal("found no CreatePaymentIntent call — the scan is broken, or the charge moved")
	}
	for _, c := range creates {
		if c.fn != "chargeInvoice" || !strings.HasSuffix(filepath.ToSlash(c.file), "internal/payment/stripe.go") {
			t.Errorf("CreatePaymentIntent is called from %s in %s; only chargeInvoice may call it, after its charge record (ADR-127)", c.fn, c.file)
		}
	}
	var begin *call
	for i := range begins {
		if begins[i].fn == "chargeInvoice" && begins[i].file == creates[0].file {
			begin = &begins[i]
		}
	}
	if begin == nil {
		t.Fatal("chargeInvoice does not call beginChargeRecord: the charge would reach Stripe with no record on the invoice")
	}
	for _, c := range creates {
		if c.pos < begin.pos {
			t.Error("chargeInvoice calls CreatePaymentIntent before beginChargeRecord: the record must be on the invoice before the request leaves")
		}
	}
}
