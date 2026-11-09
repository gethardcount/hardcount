package arch

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
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
// follows every call that hands an event type to the webhook package (the
// outbox Enqueue/EnqueueStandalone and the dispatchers' Dispatch), through
// every wrapper that passes its own parameter on (enqueueLifecycle,
// enqueueInvoiceEventTx, escalatePaymentAnomaly and so on) and through the
// interfaces a wrapper's receiver implements (payment.PaymentAnomalyRecorder,
// paymentmethods.Store), and requires the event type at each call to be a
// constant whose value is in the catalog. A string literal or another
// package's constant is checked by value, so neither can slip past.
//
// The same walk tells the two ways an event reaches the outbox apart. Enqueue
// inserts the row on the caller's transaction, so the event commits with its
// change. EnqueueStandalone, and the Dispatch methods that call it, insert
// the row in a second transaction after the change committed: a crash, a
// deploy or a cancelled request between the two commits loses the event for
// good. standaloneWebhookBacklog counts the sites that queue an event the
// second way, and the counts only go down.
//
// A site is a call that queues a fixed event type after the commit: a
// standalone sink call with a constant, or a call of a helper. A helper is a
// function whose only calls queue a fixed event type, so each caller of a
// helper holds a site too. The test does not see a new call of a function
// that holds a site and does other work too: the call reaches a site
// already counted. A PR that moves such a site must list that function's
// callers in its site-set.
// The walk follows calls, so a helper passed as a function value is not
// followed.

// webhookSinkNames are the webhook package's entry points that write an
// event to the outbox or dispatch it.
var webhookSinkNames = map[string]bool{"Dispatch": true, "Enqueue": true, "EnqueueStandalone": true}

// standaloneSinkNames are the sink names that queue the event in a
// transaction of their own, after the caller's change has committed. An
// interface method takes the class of its name: domain.EventDispatcher's
// Dispatch is standalone, and each domain's OutboxEnqueuer.Enqueue is not.
var standaloneSinkNames = map[string]bool{"Dispatch": true, "EnqueueStandalone": true}

// standaloneWebhookBacklog counts the sites that queue a webhook event after
// its business transaction commits, by "<pkg>.<Recv>.<Func> <event type>". A
// new site fails the test, even in a function already listed: queue the
// event in the transaction that writes the change instead. When a site moves
// into its transaction, lower its count or delete its entry. When an event
// type leaves the list, mark it "Always sent" in docs/api/webhook-events.md
// (TestAlwaysSentMarksMatchTheBacklog checks the two agree).
var standaloneWebhookBacklog = map[string]int{}

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
	// standalone marks the ones that reach a standalone sink.
	sinks := map[*types.Func]map[int]bool{}
	standalone := map[*types.Func]map[int]bool{}
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
	markStandalone := func(fn *types.Func, i int) bool {
		if standalone[fn] == nil {
			standalone[fn] = map[int]bool{}
		}
		if standalone[fn][i] {
			return false
		}
		standalone[fn][i] = true
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
					if standaloneSinkNames[fn.Name()] {
						markStandalone(fn, idx)
					}
					seedRecv = append(seedRecv, ptr)
				}
			}
		}
	}
	if len(sinks) < 3 {
		t.Fatalf("found %d webhook entry points; want OutboxStore.Enqueue, OutboxStore.EnqueueStandalone and OutboxDispatcher.Dispatch at least", len(sinks))
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
					if standaloneSinkNames[m.Name()] {
						markStandalone(m, idx)
					}
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
		outer  bool // not inside another call's arguments
	}
	var calls []call
	inArgs := map[*ast.CallExpr]bool{}
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
						calls = append(calls, call{p, file, encl, params, locals, n, !inArgs[n]})
						for _, a := range n.Args {
							ast.Inspect(a, func(m ast.Node) bool {
								if inner, ok := m.(*ast.CallExpr); ok {
									inArgs[inner] = true
								}
								return true
							})
						}
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

	// A wrapper that passes its own parameter to a sink is a sink, and it is
	// standalone when that sink is. So is the method of each interface the
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
				alone := standalone[fn][idx]
				for _, w := range append([]*types.Func{c.encl}, sameNamed(c.encl)...) {
					if add(w, pi) {
						changed = true
					}
					if alone && markStandalone(w, pi) {
						changed = true
					}
				}
			}
		}
	}

	// A helper queues a fixed event type and does nothing else: its only
	// calls are standalone sink calls with a constant, calls of other
	// helpers, and the calls that build their arguments. Its callers decide
	// to send the event, so each call of a helper is a site in its caller.
	// An interface method that a helper's receiver implements is a helper
	// too, as for the sinks.
	outerCalls := map[*types.Func][]call{}
	for _, c := range calls {
		if c.encl != nil && c.outer {
			outerCalls[c.encl] = append(outerCalls[c.encl], c)
		}
	}
	helpers := map[*types.Func]map[string]bool{} // helper -> the event types it queues
	onlyQueues := func(cs []call) (map[string]bool, bool) {
		evs := map[string]bool{}
		for _, c := range cs {
			fn := callee(c)
			if fn == nil {
				return nil, false
			}
			if hv, ok := helpers[fn]; ok {
				for v := range hv {
					evs[v] = true
				}
				continue
			}
			if standalone[fn] == nil {
				return nil, false
			}
			for idx := range sinks[fn] {
				if idx >= len(c.expr.Args) {
					return nil, false
				}
				vs, ok := constValues(c, c.expr.Args[idx])
				if !ok {
					return nil, false
				}
				for _, v := range vs {
					evs[v] = true
				}
			}
		}
		return evs, len(evs) > 0
	}
	for changed := true; changed; {
		changed = false
		for fn, cs := range outerCalls {
			if helpers[fn] != nil {
				continue
			}
			evs, ok := onlyQueues(cs)
			if !ok {
				continue
			}
			helpers[fn] = evs
			changed = true
			for _, m := range sameNamed(fn) {
				if helpers[m] == nil {
					helpers[m] = map[string]bool{}
				}
				for v := range evs {
					helpers[m][v] = true
				}
			}
		}
	}

	checked := 0
	var problems []string
	standaloneFound := map[string]int{} // backlog key -> the sites the walk counted
	sitesOf := map[string][]string{}    // backlog key -> where those sites are
	addSite := func(c call, where, v string) {
		key := c.pkg.Types.Name() + "." + funcName(c.encl) + " " + v
		standaloneFound[key]++
		sitesOf[key] = append(sitesOf[key], where)
	}
	for _, c := range calls {
		fn := callee(c)
		if fn == nil || helpers[fn] == nil || c.encl == nil {
			continue
		}
		pos := c.pkg.Fset.Position(c.expr.Pos())
		rel, _ := filepath.Rel(root, pos.Filename)
		for v := range helpers[fn] {
			addSite(c, rel+":"+strconv.Itoa(pos.Line), v)
		}
	}
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
					if standalone[fn][idx] && c.encl != nil {
						addSite(c, where, v)
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
	var standaloneSites []string
	for key, n := range standaloneFound {
		sort.Strings(sitesOf[key])
		for _, where := range sitesOf[key] {
			standaloneSites = append(standaloneSites, where+" "+key)
		}
		allowed := standaloneWebhookBacklog[key]
		if n <= allowed {
			continue
		}
		typ := key[strings.LastIndex(key, " ")+1:]
		what := strings.Join(sitesOf[key], ", ") + " (" + key + ") queues " + typ
		if allowed > 0 {
			what = key + " has " + strconv.Itoa(n) + " sites (" + strings.Join(sitesOf[key], ", ") + "), and standaloneWebhookBacklog allows " +
				strconv.Itoa(allowed) + ". The new one queues " + typ
		}
		problems = append(problems, what+" after the business transaction commits, so a crash, "+
			"a deploy or a cancelled request between the two commits loses it. Queue it in the transaction that writes the change: "+
			"OutboxStore.Enqueue(ctx, tx, …) in the store method that commits the transition (ADR-040, the MarkPaid pattern), "+
			"with the payload built from the row that transaction wrote (ADR-131)")
	}
	for key, allowed := range standaloneWebhookBacklog {
		n := standaloneFound[key]
		switch {
		case allowed < 1:
			problems = append(problems, "standaloneWebhookBacklog lists "+key+" with a count of "+strconv.Itoa(allowed)+": delete the entry")
		case n == 0:
			problems = append(problems, "standaloneWebhookBacklog lists "+key+", but no site queues that event after its commit any more: "+
				"delete the entry (the backlog only shrinks) and, if no other entry names the event, mark it Always sent in docs/api/webhook-events.md")
		case n < allowed:
			problems = append(problems, "standaloneWebhookBacklog allows "+strconv.Itoa(allowed)+" sites for "+key+", but the walk finds "+
				strconv.Itoa(n)+": lower the count to "+strconv.Itoa(n)+" (the backlog only shrinks)")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	sort.Strings(standaloneSites)
	t.Logf("%d sites queue a webhook event after its commit (%d backlog keys):\n%s",
		len(standaloneSites), len(standaloneFound), strings.Join(standaloneSites, "\n"))
	var helperNames []string
	for fn := range helpers {
		helperNames = append(helperNames, fn.Pkg().Name()+"."+funcName(fn))
	}
	sort.Strings(helperNames)
	t.Logf("%d helpers, whose callers hold sites too: %s", len(helperNames), strings.Join(helperNames, ", "))
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

// TestAlwaysSentMarksMatchTheBacklog keeps docs/api/webhook-events.md true.
// The page marks an event "Always sent" when it is queued in the transaction
// of its change, so a crash cannot drop it. That holds for every catalog
// event with no site in standaloneWebhookBacklog, and for no other.
func TestAlwaysSentMarksMatchTheBacklog(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "docs", "api", "webhook-events.md"))
	if err != nil {
		t.Fatal(err)
	}
	heading := regexp.MustCompile("^### `([a-z_.]+)`$")
	marked := map[string]bool{}
	headings := 0
	current := ""
	for _, line := range strings.Split(string(b), "\n") {
		if m := heading.FindStringSubmatch(line); m != nil {
			current = m[1]
			headings++
			continue
		}
		if strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") {
			current = ""
			continue
		}
		if current != "" && strings.HasPrefix(line, "**Always sent.**") {
			marked[current] = true
		}
	}
	if headings < 30 || len(marked) == 0 {
		t.Fatalf("found %d event headings and %d Always sent marks in webhook-events.md; the parser no longer matches the page", headings, len(marked))
	}

	after := map[string][]string{} // event type -> its backlog keys
	for key := range standaloneWebhookBacklog {
		typ := key[strings.LastIndex(key, " ")+1:]
		after[typ] = append(after[typ], key)
	}
	var problems []string
	for typ := range domain.KnownWebhookEventTypes {
		keys := after[typ]
		switch {
		case len(keys) == 0 && !marked[typ]:
			problems = append(problems, typ+" is queued in the transaction of its change on every path now: mark it **Always sent.** in docs/api/webhook-events.md")
		case len(keys) > 0 && marked[typ]:
			sort.Strings(keys)
			problems = append(problems, typ+" is marked Always sent in docs/api/webhook-events.md, but a site queues it after its commit, "+
				"so a crash can drop it ("+strings.Join(keys, ", ")+"): remove the mark")
		}
	}
	for typ := range marked {
		if !domain.KnownWebhookEventTypes[typ] {
			problems = append(problems, typ+" is marked Always sent but is not in domain.KnownWebhookEventTypes")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	t.Logf("%d of %d event types are marked Always sent", len(marked), len(domain.KnownWebhookEventTypes))
}
