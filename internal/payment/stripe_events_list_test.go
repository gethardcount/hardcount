package payment

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"testing"
)

// The dashboard's Stripe settings tell the operator which events to turn on
// for the webhook endpoint (STRIPE_WEBHOOK_EVENTS in Settings.tsx). That list
// must be exactly the events processEvent handles. It drifted both ways: it
// left out the three charge.dispute.* events, so disputes were never
// recorded unless the operator read the runbook, and it listed
// payment_intent.canceled, which falls to the default case.
func TestDashboardStripeEventListMatchesProcessEvent(t *testing.T) {
	handled := processEventCases(t)

	src, err := os.ReadFile("../../web-v2/src/pages/Settings.tsx")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)const STRIPE_WEBHOOK_EVENTS[^=]*= \[(.*?)\n\]`).FindSubmatch(src)
	if block == nil {
		t.Fatal("STRIPE_WEBHOOK_EVENTS not found in Settings.tsx")
	}
	var listed []string
	for _, m := range regexp.MustCompile(`name: '([^']+)'`).FindAllSubmatch(block[1], -1) {
		listed = append(listed, string(m[1]))
	}
	slices.Sort(listed)

	if !slices.Equal(handled, listed) {
		t.Fatalf("Settings.tsx lists %v\nprocessEvent handles %v", listed, handled)
	}
}

// processEventCases returns the event types in processEvent's switch cases.
func processEventCases(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "stripe.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cases []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "processEvent" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					cases = append(cases, v)
				}
			}
			return false
		})
	}
	if len(cases) == 0 {
		t.Fatal("no cases found in processEvent")
	}
	slices.Sort(cases)
	return cases
}
