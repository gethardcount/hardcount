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

	"github.com/gethardcount/hardcount/internal/domain"
)

// Every outbound webhook Hardcount sends must be one an endpoint can subscribe
// to by name. payment.currency_mismatch was sent for months while missing
// from domain.KnownWebhookEventTypes, so an endpoint that named it got 422
// "unknown event type" and only `*` and `payment.*` endpoints received it.
//
// The webhook catalog tests in internal/domain compare the catalog with the
// Event* constants declared there. This test closes the other half: it
// follows every call that hands an event type to OutboxStore.Enqueue, through
// every wrapper that passes its own parameter on (enqueueLifecycle,
// enqueueInvoiceEventTx, escalatePaymentAnomaly and so on) and through the
// interfaces a wrapper's receiver implements (payment.PaymentAnomalyRecorder,
// paymentmethods.Store), and requires the event type at each call to be a
// constant whose value is in the catalog. A string literal or another
// package's constant is checked by value, so neither can slip past. The walk
// follows calls, so a helper passed as a function value is not followed.
//
// The test also keeps every event in the transaction of its change.
// OutboxStore.Enqueue inserts the row on the caller's transaction, so the
// event commits with the change it reports (ADR-040). A method that queued
// the event in a transaction of its own would commit it after the change: a
// crash, a deploy or a cancelled request between the two commits would lose
// the event for good. (See ADR-040.) The test fails if the webhook package
// gains one: an exported function or method that takes an eventType but no
// *sql.Tx.
//
// It does not see a hand-rolled BeginTx + Enqueue + Commit with no state
// write in it, which looks like an in-transaction send. The class B question
// in docs/dev/money-path-robustness-playbook.md §4 covers that.

// webhookSinkNames are the webhook package's entry points that write an
// event to the outbox.
var webhookSinkNames = map[string]bool{"Enqueue": true}

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

	const webhookPkg = "github.com/gethardcount/hardcount/internal/webhook"

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
	// takesTx reports whether a signature has a *sql.Tx parameter: the
	// caller's transaction, which the event commits with.
	takesTx := func(sig *types.Signature) bool {
		for i := 0; i < sig.Params().Len(); i++ {
			ptr, ok := sig.Params().At(i).Type().(*types.Pointer)
			if !ok {
				continue
			}
			if n, ok := ptr.Elem().(*types.Named); ok && n.Obj().Pkg() != nil &&
				n.Obj().Pkg().Path() == "database/sql" && n.Obj().Name() == "Tx" {
				return true
			}
		}
		return false
	}

	// Seeds: the webhook package's own Enqueue methods. Every other exported
	// function or method there that takes an event type but no *sql.Tx is a
	// standalone sink: it queues the event in a transaction of its own.
	var seedRecv []types.Type
	var standaloneSeeds []string
	outboxEnqueue := false
	for _, p := range pkgs {
		if p.PkgPath != webhookPkg {
			continue
		}
		for _, name := range p.Types.Scope().Names() {
			switch obj := p.Types.Scope().Lookup(name).(type) {
			case *types.Func:
				sig := obj.Type().(*types.Signature)
				if obj.Exported() && eventParam(sig) >= 0 && !takesTx(sig) {
					standaloneSeeds = append(standaloneSeeds, obj.Name())
				}
			case *types.TypeName:
				ptr := types.NewPointer(obj.Type())
				ms := types.NewMethodSet(ptr)
				for i := 0; i < ms.Len(); i++ {
					fn := ms.At(i).Obj().(*types.Func)
					sig := fn.Type().(*types.Signature)
					idx := eventParam(sig)
					if idx < 0 {
						continue
					}
					if fn.Exported() && !takesTx(sig) {
						standaloneSeeds = append(standaloneSeeds, obj.Name()+"."+fn.Name())
						continue
					}
					if !webhookSinkNames[fn.Name()] {
						continue
					}
					add(fn, idx)
					seedRecv = append(seedRecv, ptr)
					if obj.Name() == "OutboxStore" {
						outboxEnqueue = true
					}
				}
			}
		}
	}
	if !outboxEnqueue {
		t.Fatalf("found no OutboxStore.Enqueue with an eventType parameter in %s; the walk has no sink to follow", webhookPkg)
	}
	sort.Strings(standaloneSeeds)
	for _, name := range standaloneSeeds {
		t.Errorf("internal/webhook: %s takes an event type but no *sql.Tx, so it queues the event in a transaction of its own, "+
			"after the caller's change has committed. A crash, a deploy or a cancelled request between the two commits loses the event. "+
			"Queue it with OutboxStore.Enqueue(ctx, tx, …) on the transaction that writes the change, in the store method that commits "+
			"the transition (ADR-040, the MarkPaid pattern), with the payload built from the row that transaction wrote (ADR-131)", name)
	}
	if len(standaloneSeeds) > 0 {
		t.FailNow()
	}

	// The consumer-defined interfaces those methods satisfy (each domain's
	// outbox interface) are sinks too.
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
	// constValues returns the values an event-type argument can hold, when
	// each is a string constant: the argument itself, or every value assigned
	// to the local variable it names.
	constValues := func(c call, arg ast.Expr) ([]string, bool) {
		values := []ast.Expr{arg}
		if id, ok := ast.Unparen(arg).(*ast.Ident); ok {
			if v, ok := c.pkg.TypesInfo.Uses[id].(*types.Var); ok {
				if as, ok := c.locals[v]; ok {
					values = as
				}
			}
		}
		var out []string
		for _, e := range values {
			tv, ok := c.pkg.TypesInfo.Types[e]
			if e == nil || !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
				return nil, false
			}
			out = append(out, constant.StringVal(tv.Value))
		}
		return out, true
	}

	// sameNamed returns the method named like fn of every interface that
	// fn's receiver implements. A call through one of those interfaces may
	// reach fn, so the walk treats it as a call of fn.
	var ifaces []*types.Interface
	for _, p := range pkgs {
		for _, name := range p.Types.Scope().Names() {
			if tn, ok := p.Types.Scope().Lookup(name).(*types.TypeName); ok {
				if it, ok := tn.Type().Underlying().(*types.Interface); ok {
					ifaces = append(ifaces, it)
				}
			}
		}
	}
	sameNamed := func(fn *types.Func) []*types.Func {
		sig := fn.Type().(*types.Signature)
		if sig.Recv() == nil {
			return nil
		}
		rt := sig.Recv().Type()
		if _, ok := rt.(*types.Pointer); !ok {
			rt = types.NewPointer(rt)
		}
		var out []*types.Func
		for _, it := range ifaces {
			if !types.Implements(rt, it) {
				continue
			}
			for i := 0; i < it.NumMethods(); i++ {
				if m := it.Method(i); m.Name() == fn.Name() {
					out = append(out, m)
				}
			}
		}
		return out
	}

	// A wrapper that passes its own parameter to a sink is a sink. So is the
	// method of each interface the
	// wrapper's receiver implements: payment.PaymentAnomalyRecorder for
	// invoice.PostgresStore.RecordPaymentAnomaly, paymentmethods.Store for
	// paymentmethods.PostgresStore.Upsert. Without that step a call through
	// the interface reaches no sink, and the event types its callers pass go
	// unchecked.
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
				pi, ok := passThrough(c, c.expr.Args[idx])
				if !ok {
					continue
				}
				for _, w := range append([]*types.Func{c.encl}, sameNamed(c.encl)...) {
					if add(w, pi) {
						changed = true
					}
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
			if values, ok := constValues(c, arg); ok {
				for _, v := range values {
					if v == "" {
						// "No event": paymentmethods.Store.Upsert queues
						// nothing for it, and OutboxStore.Enqueue refuses
						// it, so it never reaches an endpoint.
						continue
					}
					checked++
					if !domain.KnownWebhookEventTypes[v] {
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
	isSink := map[string]bool{}
	for fn := range sinks {
		name := fn.Pkg().Name() + "." + funcName(fn)
		names = append(names, name)
		isSink[name] = true
	}
	sort.Strings(names)
	t.Logf("%d emit sites checked through %d functions: %s", checked, len(names), strings.Join(names, ", "))
	if checked < 30 {
		t.Fatalf("checked only %d emit sites; the call graph walk is not finding them", checked)
	}
	// The walk reaches these wrappers only through a consumer-defined
	// interface that another wrapper's receiver implements. If it stops
	// following such interfaces, the event types their callers pass are no
	// longer checked, and nothing else fails.
	for _, want := range []string{
		"payment.PaymentAnomalyRecorder.RecordPaymentAnomaly",
		"payment.Stripe.escalatePaymentAnomaly",
		"paymentmethods.Store.Upsert",
		"paymentmethods.Service.AttachForWebhook",
		"payment.PaymentMethodAttacher.AttachForWebhook",
	} {
		if !isSink[want] {
			t.Errorf("%s does not reach the webhook package in the walk; it hands its event type on to a wrapper behind an interface, so the walk must follow that interface", want)
		}
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
