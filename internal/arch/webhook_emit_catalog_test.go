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
// every wrapper that passes its own parameter on (fireEvent, dispatchEvent,
// enqueueLifecycle, escalatePaymentAnomaly and so on), and requires the
// event type at each call to be a constant whose value is in the catalog. A
// string literal or another package's constant is checked by value, so
// neither can slip past.
//
// The same walk tells the two ways an event reaches the outbox apart. Enqueue
// inserts the row on the caller's transaction, so the event commits with its
// change. EnqueueStandalone, and the Dispatch methods that call it, insert
// the row in a second transaction after the change committed: a crash, a
// deploy or a cancelled request between the two commits loses the event for
// good. Every site that queues a constant event type the second way must be
// in standaloneWebhookBacklog, and that list only shrinks.

// webhookSinkNames are the webhook package's entry points that write an
// event to the outbox or dispatch it.
var webhookSinkNames = map[string]bool{"Dispatch": true, "Enqueue": true, "EnqueueStandalone": true}

// standaloneSinkNames are the sink names that queue the event in a
// transaction of their own, after the caller's change has committed. An
// interface method takes the class of its name: domain.EventDispatcher's
// Dispatch is standalone, and each domain's OutboxEnqueuer.Enqueue is not.
var standaloneSinkNames = map[string]bool{"Dispatch": true, "EnqueueStandalone": true}

// standaloneWebhookBacklog lists every site that queues a webhook event after
// its business transaction commits, as "<pkg>.<Recv>.<Func> <event type>". A
// new site fails the test: queue the event in the transaction that writes the
// change instead. When a site moves into its transaction, delete its entry
// and mark the event "Always sent" in docs/api/webhook-events.md
// (TestAlwaysSentMarksMatchTheBacklog checks the two agree).
var standaloneWebhookBacklog = map[string]bool{
	"billing.Engine.DispatchInvoiceFinalized invoice.finalized":                         true,
	"billing.Engine.applyDueScheduledPlanChanges subscription.pending_change.applied":   true,
	"billing.Engine.fireThreshold subscription.threshold_crossed":                       true,
	"customer.Service.MarkEmailBounced customer.email_bounced":                          true,
	"customer.Service.MarkEmailComplained customer.email_complained":                    true,
	"dunning.Service.exhaustRun dunning.escalated":                                      true,
	"dunning.Service.resolveRunAt dunning.resolved":                                     true,
	"invoice.Service.AfterVoid invoice.voided":                                          true,
	"invoice.Service.MarkUncollectible invoice.marked_uncollectible":                    true,
	"invoice.Service.RecordOfflinePayment invoice.payment_recorded":                     true,
	"invoice.Service.finalize invoice.finalized":                                        true,
	"payment.Stripe.RecordCheckoutCard payment_method.updated":                          true,
	"payment.Stripe.SettleSucceeded payment.amount_mismatch":                            true,
	"payment.Stripe.SettleSucceeded payment.duplicate_charge":                           true,
	"payment.Stripe.SettleSucceeded payment.received_on_voided_invoice":                 true,
	"payment.Stripe.handlePaymentSucceeded payment.currency_mismatch":                   true,
	"payment.Stripe.handleSetupIntentSucceeded payment_method.attached":                 true,
	"subscription.Handler.addItem subscription.item.added":                              true,
	"subscription.Handler.cancelPendingItemChange subscription.pending_change.canceled": true,
	"subscription.Handler.clearScheduledCancel subscription.cancel_cleared":             true,
	"subscription.Handler.extendTrial subscription.trial_extended":                      true,
	"subscription.Handler.pauseCollection subscription.collection_paused":               true,
	"subscription.Handler.removeItem subscription.item.removed":                         true,
	"subscription.Handler.resumeCollection subscription.collection_resumed":             true,
	"subscription.Handler.scheduleCancel subscription.cancel_scheduled":                 true,
	"subscription.Handler.updateItem subscription.item.updated":                         true,
	"subscription.Handler.updateItem subscription.pending_change.scheduled":             true,
}

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

	// A wrapper that passes its own parameter to a sink is a sink, and it is
	// standalone when that sink is.
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
				if add(c.encl, pi) {
					changed = true
				}
				if standalone[fn][idx] && markStandalone(c.encl, pi) {
					changed = true
				}
			}
		}
	}

	checked := 0
	var problems []string
	standaloneFound := map[string]bool{} // backlog keys the walk reached
	var standaloneSites []string
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
					v := constant.StringVal(c.pkg.TypesInfo.Types[e].Value)
					if !domain.KnownWebhookEventTypes[v] {
						problems = append(problems, where+": sends "+v+", which is not in domain.KnownWebhookEventTypes (an endpoint that names it gets 422 unknown event type)")
					}
					if !standalone[fn][idx] || c.encl == nil {
						continue
					}
					key := c.pkg.Types.Name() + "." + funcName(c.encl) + " " + v
					standaloneSites = append(standaloneSites, where+" "+key)
					standaloneFound[key] = true
					if !standaloneWebhookBacklog[key] {
						problems = append(problems, where+" ("+key+") queues "+v+" after the business transaction commits, so a crash, "+
							"a deploy or a cancelled request between the two commits loses it. Queue it in the transaction that writes the change: "+
							"OutboxStore.Enqueue(ctx, tx, …) in the store method that commits the transition (ADR-040, the MarkPaid pattern), "+
							"with the payload built from the row that transaction wrote (ADR-131)")
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
	for key := range standaloneWebhookBacklog {
		if !standaloneFound[key] {
			problems = append(problems, "standaloneWebhookBacklog lists "+key+", but no site queues that event after its commit any more: "+
				"delete the entry (the backlog only shrinks) and mark the event Always sent in docs/api/webhook-events.md")
		}
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
	sort.Strings(standaloneSites)
	t.Logf("%d sites queue a webhook event after its commit (%d backlog keys):\n%s",
		len(standaloneSites), len(standaloneFound), strings.Join(standaloneSites, "\n"))
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
