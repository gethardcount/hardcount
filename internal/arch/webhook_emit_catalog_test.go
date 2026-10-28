package arch

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Every outbound webhook Hardcount sends must be one an endpoint can subscribe
// to by name. payment.currency_mismatch was sent for months while missing
// from domain.KnownWebhookEventTypes, so an endpoint that named it got 422
// "unknown event type" and only `*` and `payment.*` endpoints received it.
//
// The webhook catalog tests in internal/domain compare the catalog with the
// Event* constants declared there. This test closes the other half: it
// follows every call that hands an event type to the webhook package (the
// outbox Enqueue/EnqueueStandalone and the dispatchers' Dispatch), through
// every wrapper that passes its own parameter on (fireEvent, dispatchEvent,
// enqueueLifecycle, escalatePaymentAnomaly and so on), and requires the
// event type at each call to be a constant whose value is in the catalog. A
// string literal or another package's constant is checked by value, so
// neither can slip past.

// webhookSinkNames are the webhook package's entry points that write an
// event to the outbox or dispatch it.
var webhookSinkNames = map[string]bool{"Dispatch": true, "Enqueue": true, "EnqueueStandalone": true}

// webhookDynamicEmitAllowlist names the functions that hand the webhook
// package an event type that is not a constant, each with its reason. The
// value they pass was checked when it was first enqueued.
var webhookDynamicEmitAllowlist = map[string]string{}

func TestWebhookEmitSitesSendACatalogEvent(t *testing.T) {
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
	for _, p := range pkgs {
		for _, e := range p.Errors {
			t.Fatalf("load %s: %v", p.PkgPath, e)
		}
	}

	const webhookPkg = "github.com/sagarsuperuser/velox/internal/webhook"

	// sinks maps a function (or interface method) to the indexes of its
	// parameters that carry an event type into the webhook package.
	sinks := map[*types.Func]map[int]bool{}
	add := func(fn *types.Func, i int) bool {
		if sinks[fn] == nil {
			sinks[fn] = map[int]bool{}
		}
		if sinks[fn][i] {
			return false
		}
		sinks[fn][i] = true
		return true
	}
	eventParam := func(sig *types.Signature) int {
		for i := 0; i < sig.Params().Len(); i++ {
			if sig.Params().At(i).Name() == "eventType" {
				return i
			}
		}
		return -1
	}

	// Seeds: the webhook package's own methods.
	var seedRecv []types.Type
	for _, p := range pkgs {
		if p.PkgPath != webhookPkg {
			continue
		}
		for _, name := range p.Types.Scope().Names() {
			tn, ok := p.Types.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			ptr := types.NewPointer(tn.Type())
			ms := types.NewMethodSet(ptr)
			for i := 0; i < ms.Len(); i++ {
				fn := ms.At(i).Obj().(*types.Func)
				if !webhookSinkNames[fn.Name()] {
					continue
				}
				if idx := eventParam(fn.Type().(*types.Signature)); idx >= 0 {
					add(fn, idx)
					seedRecv = append(seedRecv, ptr)
				}
			}
		}
	}
	if len(sinks) < 3 {
		t.Fatalf("found %d webhook entry points; want Service.Dispatch, OutboxStore.Enqueue and EnqueueStandalone at least", len(sinks))
	}

	// The consumer-defined interfaces those methods satisfy (domain's
	// EventDispatcher, each domain's outbox interface) are sinks too.
	for _, p := range pkgs {
		for _, name := range p.Types.Scope().Names() {
			tn, ok := p.Types.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			iface, ok := tn.Type().Underlying().(*types.Interface)
			if !ok {
				continue
			}
			implemented := false
			for _, r := range seedRecv {
				if types.Implements(r, iface) {
					implemented = true
					break
				}
			}
			if !implemented {
				continue
			}
			for i := 0; i < iface.NumMethods(); i++ {
				m := iface.Method(i)
				if !webhookSinkNames[m.Name()] {
					continue
				}
				if idx := eventParam(m.Type().(*types.Signature)); idx >= 0 {
					add(m, idx)
				}
			}
		}
	}

	type call struct {
		pkg    *packages.Package
		file   string
		encl   *types.Func               // the FuncDecl the call is in
		params map[*types.Var]int        // that FuncDecl's parameters
		locals map[*types.Var][]ast.Expr // each value assigned to a local; nil entry = not a plain assignment
		expr   *ast.CallExpr
	}
	var calls []call
	for _, p := range pkgs {
		for _, f := range p.Syntax {
			file := p.Fset.Position(f.Pos()).Filename
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				encl, _ := p.TypesInfo.Defs[fd.Name].(*types.Func)
				params := map[*types.Var]int{}
				if encl != nil {
					sig := encl.Type().(*types.Signature)
					for j := 0; j < sig.Params().Len(); j++ {
						params[sig.Params().At(j)] = j
					}
				}
				locals := map[*types.Var][]ast.Expr{}
				assign := func(lhs ast.Expr, rhs ast.Expr) {
					id, ok := ast.Unparen(lhs).(*ast.Ident)
					if !ok {
						return
					}
					obj := p.TypesInfo.Defs[id]
					if obj == nil {
						obj = p.TypesInfo.Uses[id]
					}
					if v, ok := obj.(*types.Var); ok {
						locals[v] = append(locals[v], rhs)
					}
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.CallExpr:
						calls = append(calls, call{p, file, encl, params, locals, n})
					case *ast.AssignStmt:
						for i, l := range n.Lhs {
							if len(n.Lhs) == len(n.Rhs) {
								assign(l, n.Rhs[i])
							} else {
								assign(l, nil)
							}
						}
					case *ast.ValueSpec:
						for i, name := range n.Names {
							if i < len(n.Values) {
								assign(name, n.Values[i])
							} else {
								assign(name, nil)
							}
						}
					case *ast.UnaryExpr:
						if n.Op == token.AND {
							assign(n.X, nil) // address taken: anything may write it
						}
					}
					return true
				})
			}
		}
	}
	callee := func(c call) *types.Func {
		var id *ast.Ident
		switch fun := ast.Unparen(c.expr.Fun).(type) {
		case *ast.Ident:
			id = fun
		case *ast.SelectorExpr:
			id = fun.Sel
		default:
			return nil
		}
		fn, _ := c.pkg.TypesInfo.Uses[id].(*types.Func)
		if fn == nil {
			return nil
		}
		return fn.Origin()
	}
	passThrough := func(c call, arg ast.Expr) (int, bool) {
		id, ok := ast.Unparen(arg).(*ast.Ident)
		if !ok {
			return 0, false
		}
		v, ok := c.pkg.TypesInfo.Uses[id].(*types.Var)
		if !ok {
			return 0, false
		}
		i, ok := c.params[v]
		return i, ok
	}

	// A wrapper that passes its own parameter to a sink is a sink.
	for changed := true; changed; {
		changed = false
		for _, c := range calls {
			fn := callee(c)
			if fn == nil || sinks[fn] == nil || c.encl == nil {
				continue
			}
			for idx := range sinks[fn] {
				if idx >= len(c.expr.Args) {
					continue
				}
				if pi, ok := passThrough(c, c.expr.Args[idx]); ok && add(c.encl, pi) {
					changed = true
				}
			}
		}
	}

	checked := 0
	var problems []string
	for _, c := range calls {
		fn := callee(c)
		if fn == nil || sinks[fn] == nil {
			continue
		}
		for idx := range sinks[fn] {
			if idx >= len(c.expr.Args) {
				continue
			}
			arg := c.expr.Args[idx]
			pos := c.pkg.Fset.Position(arg.Pos())
			rel, _ := filepath.Rel(root, pos.Filename)
			where := rel + ":" + strconv.Itoa(pos.Line)
			// The constant values this argument can hold: the argument itself,
			// or every value assigned to the local variable it names.
			values := []ast.Expr{arg}
			if id, ok := ast.Unparen(arg).(*ast.Ident); ok {
				if v, ok := c.pkg.TypesInfo.Uses[id].(*types.Var); ok {
					if as, ok := c.locals[v]; ok {
						values = as
					}
				}
			}
			allConst := true
			for _, e := range values {
				tv, ok := c.pkg.TypesInfo.Types[e]
				if e == nil || !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
					allConst = false
					break
				}
			}
			if allConst {
				for _, e := range values {
					checked++
					if v := constant.StringVal(c.pkg.TypesInfo.Types[e].Value); !domain.KnownWebhookEventTypes[v] {
						problems = append(problems, where+": sends "+v+", which is not in domain.KnownWebhookEventTypes (an endpoint that names it gets 422 unknown event type)")
					}
				}
				continue
			}
			if pi, ok := passThrough(c, arg); ok && sinks[c.encl][pi] {
				continue // checked at the wrapper's callers
			}
			key := ""
			if c.encl != nil {
				key = c.pkg.PkgPath[strings.LastIndex(c.pkg.PkgPath, "/")+1:] + "." + funcName(c.encl)
			}
			if _, ok := webhookDynamicEmitAllowlist[key]; ok {
				continue
			}
			problems = append(problems, where+" ("+key+"): passes an event type that is not a constant to "+fn.Name()+
				"; pass a domain.Event* constant, or add the function to webhookDynamicEmitAllowlist with the reason its value is already in the catalog")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	var names []string
	for fn := range sinks {
		names = append(names, fn.Pkg().Name()+"."+funcName(fn))
	}
	sort.Strings(names)
	t.Logf("%d emit sites checked through %d functions: %s", checked, len(names), strings.Join(names, ", "))
	if checked < 30 {
		t.Fatalf("checked only %d emit sites; the call graph walk is not finding them", checked)
	}
}

func funcName(fn *types.Func) string {
	if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() != nil {
		rt := sig.Recv().Type()
		if p, ok := rt.(*types.Pointer); ok {
			rt = p.Elem()
		}
		if n, ok := rt.(*types.Named); ok {
			return n.Obj().Name() + "." + fn.Name()
		}
	}
	return fn.Name()
}
