package arch

import (
	"go/ast"
	"go/types"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// A store or service that queues webhook events in its transaction gets the
// outbox from a setter after construction, and queues nothing while it is
// nil. webhook.MustWiredOutbox in internal/api/router.go refuses to boot when
// one of the components it is given has no outbox. This test makes sure it is
// given every one: each production struct that declares SetOutboxEnqueuer, or
// holds a field whose interface has the outbox's Enqueue method.
func TestMustWiredOutbox_CoversEveryOutboxHolder(t *testing.T) {
	root := repoRoot(t)
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo,
		Dir: root,
	}
	pkgs, err := packages.Load(cfg, "./internal/...", "./cmd/...")
	if err != nil {
		t.Fatalf("load packages: %v", err)
	}
	const webhookPkg = "github.com/gethardcount/hardcount/internal/webhook"
	const apiPkg = "github.com/gethardcount/hardcount/internal/api"
	var enqueuer *types.Interface
	for _, p := range pkgs {
		for _, e := range p.Errors {
			t.Fatalf("load %s: %v", p.PkgPath, e)
		}
		if p.PkgPath == webhookPkg {
			if tn, ok := p.Types.Scope().Lookup("outboxEnqueuer").(*types.TypeName); ok {
				enqueuer, _ = tn.Type().Underlying().(*types.Interface)
			}
		}
	}
	if enqueuer == nil {
		t.Fatal("webhook.outboxEnqueuer not found; it is the interface MustWiredOutbox checks fields against")
	}

	// holders: every named struct type that holds an outbox, with why.
	holders := map[*types.TypeName]string{}
	for _, p := range pkgs {
		for _, name := range p.Types.Scope().Names() {
			tn, ok := p.Types.Scope().Lookup(name).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok {
				continue
			}
			st, ok := named.Underlying().(*types.Struct)
			if !ok {
				continue
			}
			// Declared on the type itself: an adapter that embeds a store
			// only promotes the store's setter, and the store is checked.
			declares := false
			for i := range named.NumMethods() {
				if named.Method(i).Name() == "SetOutboxEnqueuer" {
					declares = true
				}
			}
			if declares {
				holders[tn] = "declares SetOutboxEnqueuer"
				continue
			}
			for i := range st.NumFields() {
				f := st.Field(i)
				if _, isIface := f.Type().Underlying().(*types.Interface); isIface && types.Implements(f.Type(), enqueuer) {
					holders[tn] = "holds the outbox in field " + f.Name()
					break
				}
			}
		}
	}
	if len(holders) < 4 {
		t.Fatalf("found %d outbox holders; want at least the invoice, credit, subscription and dunning stores. The walk is broken, not the code", len(holders))
	}

	// gated: the named type of every argument to webhook.MustWiredOutbox in
	// the composition root.
	gated := map[*types.TypeName]bool{}
	calls := 0
	for _, p := range pkgs {
		if p.PkgPath != apiPkg {
			continue
		}
		for _, f := range p.Syntax {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				fn, ok := p.TypesInfo.Uses[sel.Sel].(*types.Func)
				if !ok || fn.Name() != "MustWiredOutbox" || fn.Pkg() == nil || fn.Pkg().Path() != webhookPkg {
					return true
				}
				calls++
				for _, arg := range call.Args {
					typ := p.TypesInfo.TypeOf(arg)
					if ptr, ok := typ.(*types.Pointer); ok {
						typ = ptr.Elem()
					}
					if named, ok := typ.(*types.Named); ok {
						gated[named.Obj()] = true
					}
				}
				return true
			})
		}
	}
	if calls == 0 {
		t.Fatal("no webhook.MustWiredOutbox call found in internal/api; the composition root must make one")
	}

	var problems, names []string
	for tn, why := range holders {
		name := tn.Pkg().Name() + "." + tn.Name()
		names = append(names, name)
		if !gated[tn] {
			problems = append(problems, name+" "+why+" but is not passed to webhook.MustWiredOutbox in internal/api/router.go. "+
				"Add it, so a dropped outbox setter line fails at boot instead of silently queuing none of its webhook events")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	sort.Strings(names)
	t.Logf("%d outbox holders: %s", len(holders), strings.Join(names, ", "))
}
