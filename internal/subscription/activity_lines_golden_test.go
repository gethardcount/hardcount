package subscription

import (
	"bytes"
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/activity"
)

// update regenerates testdata/activity_lines.golden.json:
//
//	go test ./internal/subscription -run TestActivityLinesGolden -update
//
// The reviewer reads the JSON diff: it is every subscription activity line
// the backend can build, in the words an operator sees.
var update = flag.Bool("update", false, "rewrite the activity-line golden file")

const goldenPath = "testdata/activity_lines.golden.json"

// ── Writer tokens, read from the code that writes them ──
//
// B2 was a writer's token (triggered_by=schedule) and the renderer's switch
// drifting apart. The golden cases and the no-fallback test range over the
// domain lists, and TestSubscriptionAuditWriters_UseTheVocabulary proves the
// lists are exactly what the writers emit: every subscription audit row in
// the code is written with a domain constant, never a literal, and every
// constant in the lists has a writer.

// repoRoot is the module root, two levels above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// domainStringConsts returns name → value for every string constant in the
// domain package.
func domainStringConsts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	paths, err := filepath.Glob(filepath.Join(repoRoot(t), "internal", "domain", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						v, _ := strconv.Unquote(lit.Value)
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out
}

// writerScan is what the subscription audit writers emit, read from the code.
type writerScan struct {
	actions    map[string][]string // action value → writer positions
	subActions map[string][]string // metadata "action" value → writer positions
	literals   []string            // writers that use a string literal instead of a constant
}

// scanSubscriptionAuditWriters walks every non-test Go file under internal/
// and finds each subscription audit write: a Log(ctx, tenant, action,
// "subscription", ...) call or an audit.Entry{ResourceType: "subscription"}.
// In a function holding one, every metadata "action" key is a sub-action.
func scanSubscriptionAuditWriters(t *testing.T) writerScan {
	t.Helper()
	consts := domainStringConsts(t)
	w := writerScan{actions: map[string][]string{}, subActions: map[string][]string{}}
	fset := token.NewFileSet()
	resolve := func(e ast.Expr, into map[string][]string, what string) {
		pos := fset.Position(e.Pos()).String()
		switch v := e.(type) {
		case *ast.SelectorExpr:
			if x, ok := v.X.(*ast.Ident); ok && x.Name == "domain" {
				if val, ok := consts[v.Sel.Name]; ok {
					into[val] = append(into[val], pos)
					return
				}
			}
		case *ast.BasicLit:
			w.literals = append(w.literals, pos+": "+what+" "+v.Value)
			return
		}
		w.literals = append(w.literals, pos+": "+what+" is not a domain constant")
	}
	isSubscriptionLit := func(e ast.Expr) bool {
		lit, ok := e.(*ast.BasicLit)
		return ok && lit.Value == `"subscription"`
	}
	root := filepath.Join(repoRoot(t), "internal")
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, string(filepath.Separator)+"generated"+string(filepath.Separator)) {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var actionExprs []ast.Expr
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.CallExpr:
					if sel, ok := v.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Log" && len(v.Args) == 7 && isSubscriptionLit(v.Args[3]) {
						actionExprs = append(actionExprs, v.Args[2])
					}
				case *ast.CompositeLit:
					sel, ok := v.Type.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Entry" {
						return true
					}
					if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "audit" {
						return true
					}
					var action ast.Expr
					sub := false
					for _, el := range v.Elts {
						kv, ok := el.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						switch k, _ := kv.Key.(*ast.Ident); {
						case k != nil && k.Name == "ResourceType":
							sub = isSubscriptionLit(kv.Value)
						case k != nil && k.Name == "Action":
							action = kv.Value
						}
					}
					if sub && action != nil {
						actionExprs = append(actionExprs, action)
					}
				}
				return true
			})
			if len(actionExprs) == 0 {
				continue
			}
			for _, e := range actionExprs {
				resolve(e, w.actions, "audit action")
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.KeyValueExpr:
					if k, ok := v.Key.(*ast.BasicLit); ok && k.Value == `"action"` {
						resolve(v.Value, w.subActions, "metadata action")
					}
				case *ast.AssignStmt:
					for i, lhs := range v.Lhs {
						if ix, ok := lhs.(*ast.IndexExpr); ok {
							if k, ok := ix.Index.(*ast.BasicLit); ok && k.Value == `"action"` && i < len(v.Rhs) {
								resolve(v.Rhs[i], w.subActions, "metadata action")
							}
						}
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.actions) == 0 || len(w.subActions) == 0 {
		t.Fatal("found no subscription audit writers; the scan no longer matches how they are written")
	}
	return w
}

func TestSubscriptionAuditWriters_UseTheVocabulary(t *testing.T) {
	w := scanSubscriptionAuditWriters(t)
	for _, l := range w.literals {
		t.Errorf("a subscription audit writer does not use the vocabulary: %s", l)
	}
	check := func(what string, written map[string][]string, listed []string) {
		in := map[string]bool{}
		for _, v := range listed {
			in[v] = true
			if _, ok := written[v]; !ok {
				t.Errorf("%s %q is listed but no writer emits it", what, v)
			}
		}
		for v, at := range written {
			if !in[v] {
				t.Errorf("%s %q is written (%s) but not listed in domain, so the timeline tests do not cover it", what, v, strings.Join(at, ", "))
			}
		}
	}
	check("audit action", w.actions, domain.SubscriptionAuditActions())
	check("metadata action", w.subActions, domain.SubscriptionAuditSubActions())
}

// subActionParent is the audit action each metadata "action" rides.
func subActionParent(sub string) string {
	switch sub {
	case domain.SubAuditTrialEndCancel:
		return domain.AuditActionCancel
	case domain.SubAuditItemPlanChanged, domain.SubAuditItemQuantityChanged:
		return domain.AuditActionSubscriptionItemUpdated
	}
	return domain.AuditActionUpdate
}

// ── Golden cases ──

type goldenCase struct {
	Case string        `json:"case"`
	Line activity.Line `json:"line"`
	// Text is the line as a reader sees it: title, problem and meta.
	Text []string `json:"text"`
}

func lineText(l activity.Line) []string {
	out := []string{l.Title}
	if l.Problem != "" {
		out = append(out, "problem: "+l.Problem)
	}
	if m := activity.MetaText(l); m != "" {
		out = append(out, "meta: "+m)
	}
	return append(out, "tone: "+string(l.Tone))
}

var (
	goldenAt     = time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	goldenLabels = lineLabels{
		plans:    map[string]string{"pln_starter": "Starter", "pln_pro": "Pro", "pln_basic": "Basic", "pln_seat": "Seat"},
		invoices: map[string]string{"vlx_inv_1": "VLX-000123", "vlx_inv_src": "VLX-000120"},
	}
)

// itemChangeTypes reads the ItemChangeType constants from the domain package.
func itemChangeTypes(t *testing.T) []string {
	t.Helper()
	consts := domainStringConsts(t)
	var out []string
	for name, v := range consts {
		if strings.HasPrefix(name, "ItemChangeType") {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("no ItemChangeType constants found")
	}
	return out
}

type actionCase struct {
	name   string
	action string
	meta   map[string]any
}

// goldenActionCases is every fact variant a subscription audit row is written
// with, for every action and metadata action in the vocabulary.
func goldenActionCases(t *testing.T) []actionCase {
	t.Helper()
	at := func(d int) string { return goldenAt.AddDate(0, 0, d).Format(time.RFC3339) }
	var cs []actionCase
	add := func(name, action string, meta map[string]any) {
		cs = append(cs, actionCase{name: name, action: action, meta: meta})
	}
	// Proration outcomes a change can carry.
	prorations := map[string]map[string]any{
		"none":                {},
		"invoice":             {"proration_type": "invoice", "proration_amount_cents": float64(1800), "currency": "USD", "proration_invoice_id": "vlx_inv_1"},
		"invoice gone":        {"proration_type": "invoice", "proration_amount_cents": float64(1800), "currency": "USD", "proration_invoice_id": "vlx_inv_gone"},
		"credit":              {"proration_type": "credit", "proration_amount_cents": float64(1980), "currency": "EUR"},
		"adjustment":          {"proration_type": "adjustment", "proration_amount_cents": float64(5444), "currency": "USD", "adjusted_invoice_id": "vlx_inv_src"},
		"adjustment legacy":   {"proration_type": "adjustment", "proration_amount_cents": float64(5444), "currency": "USD"},
		"invoice no currency": {"proration_type": "invoice", "proration_amount_cents": float64(1800)},
	}
	with := func(base map[string]any, extra map[string]any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	proNames := make([]string, 0, len(prorations))
	for n := range prorations {
		proNames = append(proNames, n)
	}
	sort.Strings(proNames)

	for _, action := range domain.SubscriptionAuditActions() {
		switch action {
		case domain.AuditActionCreate, domain.AuditActionActivate:
			add(action, action, map[string]any{"customer_id": "cus_1"})
		case domain.AuditActionCancel:
			for _, by := range []string{"operator", "customer", "schedule", "dunning", "system", ""} {
				add("cancel/canceled_by="+by, action, map[string]any{"canceled_by": by})
				add("cancel/canceled_by="+by+"/credit", action, map[string]any{"canceled_by": by, "prorated_credit_cents": float64(1200), "currency": "CAD"})
			}
		case domain.AuditActionSubscriptionPendingChangeApplied:
			add(action, action, map[string]any{"item_id": "si_1", "old_plan_id": "pln_pro", "new_plan_id": "pln_basic"})
		case domain.AuditActionSubscriptionThresholdCrossed:
			add(action+"/linked", action, map[string]any{"invoice_id": "vlx_inv_1", "invoice_number": "VLX-000123", "amount_cents": float64(5000), "currency": "USD"})
			add(action+"/invoice gone", action, map[string]any{"invoice_id": "vlx_inv_gone", "invoice_number": "VLX-000099", "amount_cents": float64(5000), "currency": "USD"})
			add(action+"/no number", action, map[string]any{})
		case domain.AuditActionSubscriptionThresholdDeferred:
			add(action, action, map[string]any{"amount_gte": float64(50000), "running_subtotal": float64(51000), "reason": "max_last_meters_bill_at_cycle_close"})
		case domain.AuditActionSubscriptionProrationFailed:
			for _, ct := range append(itemChangeTypes(t), "") {
				add(action+"/change_type="+ct, action, map[string]any{
					"item_id": "si_1", "change_type": ct, "plan_id": "pln_seat", "old_plan_id": "pln_starter", "new_plan_id": "pln_pro",
					"old_quantity": float64(3), "new_quantity": float64(5), "error": "pq: deadlock detected",
				})
			}
		case domain.AuditActionSubscriptionNetNegativeUsage:
			add(action+"/named", action, map[string]any{"meters": []any{
				map[string]any{"meter_id": "mtr_1", "meter_key": "api_calls", "meter_name": "API calls", "quantity": "-120"},
			}})
			add(action+"/two meters", action, map[string]any{"meters": []any{
				map[string]any{"meter_id": "mtr_1", "meter_key": "api_calls", "meter_name": "API calls", "quantity": "-120"},
				map[string]any{"meter_id": "mtr_2", "meter_key": "tokens", "meter_name": "Tokens", "quantity": "-0.5"},
			}})
			add(action+"/legacy key only", action, map[string]any{"meters": []any{
				map[string]any{"meter_id": "mtr_1", "meter_key": "api_calls", "quantity": "-120"},
			}})
		}
	}

	for _, sub := range domain.SubscriptionAuditSubActions() {
		parent := subActionParent(sub)
		name := parent + "/" + sub
		m := func(extra map[string]any) map[string]any { return with(map[string]any{"action": sub}, extra) }
		switch sub {
		case domain.SubAuditCancelScheduled:
			add(name+"/at period end", parent, m(map[string]any{"cancel_at_period_end": true, "status": "active", "cancel_effective_at": at(30)}))
			add(name+"/at period end while trialing", parent, m(map[string]any{"cancel_at_period_end": true, "status": "trialing", "cancel_effective_at": at(14)}))
			add(name+"/at period end legacy", parent, m(map[string]any{"cancel_at_period_end": true}))
			add(name+"/on a date", parent, m(map[string]any{"cancel_at_period_end": false, "cancel_at": at(45), "cancel_effective_at": at(45), "status": "active"}))
			add(name+"/no date", parent, m(map[string]any{}))
		case domain.SubAuditCollectionPaused:
			add(name+"/by dunning", parent, m(map[string]any{"triggered_by": "dunning", "behavior": "keep_as_draft"}))
			add(name+"/until a date", parent, m(map[string]any{"triggered_by": "operator", "behavior": "keep_as_draft", "resumes_at": at(30)}))
			add(name+"/open ended", parent, m(map[string]any{"triggered_by": "operator", "behavior": "keep_as_draft"}))
		case domain.SubAuditCollectionResumed:
			add(name+"/on schedule", parent, m(map[string]any{"triggered_by": "schedule", "resumed_at": at(30)}))
			add(name+"/by hand", parent, m(map[string]any{"triggered_by": "operator"}))
		case domain.SubAuditTrialEnded:
			add(name+"/on schedule", parent, m(map[string]any{"triggered_by": "schedule", "trial_end_at": at(0)}))
			add(name+"/by hand", parent, m(map[string]any{}))
		case domain.SubAuditTrialExtended:
			add(name, parent, m(map[string]any{"trial_end": at(21)}))
			add(name+"/no date", parent, m(map[string]any{}))
		case domain.SubAuditBillingRetried:
			for _, n := range []float64{0, 1, 3} {
				add(name+"/attempts_before="+strconv.Itoa(int(n)), parent, m(map[string]any{"attempts_before": n, "was_parked": n > 2}))
			}
		case domain.SubAuditThresholdsSet:
			add(name+"/amount and items", parent, m(map[string]any{"amount_gte": float64(5000), "item_threshold_count": float64(2), "currency": "USD"}))
			add(name+"/large amount, other currency", parent, m(map[string]any{"amount_gte": float64(123456789), "item_threshold_count": float64(0), "currency": "INR"}))
			add(name+"/one item threshold", parent, m(map[string]any{"amount_gte": float64(0), "item_threshold_count": float64(1)}))
			add(name+"/legacy no currency", parent, m(map[string]any{"amount_gte": float64(5000), "item_threshold_count": float64(0)}))
		case domain.SubAuditItemAdded:
			for _, pn := range proNames {
				add(name+"/proration "+pn, parent, with(m(map[string]any{"item_id": "si_2", "plan_id": "pln_seat", "quantity": float64(3)}), prorations[pn]))
			}
			add(name+"/deleted plan", parent, m(map[string]any{"item_id": "si_2", "plan_id": "pln_deleted", "quantity": float64(1)}))
		case domain.SubAuditItemRemoved:
			for _, pn := range proNames {
				add(name+"/proration "+pn, parent, with(m(map[string]any{"item_id": "si_2", "plan_id": "pln_seat"}), prorations[pn]))
			}
		case domain.SubAuditTrialEndCancel:
			add(name, parent, m(map[string]any{"triggered_by": "schedule", "canceled_at": at(14), "trial_end_at": at(14)}))
		case domain.SubAuditItemPlanChanged:
			for _, pn := range proNames {
				add(name+"/now/proration "+pn, parent, with(m(map[string]any{"item_id": "si_1", "immediate": true, "old_plan_id": "pln_starter", "new_plan_id": "pln_pro"}), prorations[pn]))
			}
			add(name+"/at renewal", parent, m(map[string]any{"item_id": "si_1", "immediate": false, "old_plan_id": "pln_pro", "new_plan_id": "pln_basic", "effective_at": at(28)}))
			add(name+"/at renewal legacy", parent, m(map[string]any{"item_id": "si_1", "immediate": false, "old_plan_id": "pln_pro", "new_plan_id": "pln_basic"}))
		case domain.SubAuditItemQuantityChanged:
			add(name, parent, m(map[string]any{"item_id": "si_1", "plan_id": "pln_seat", "old_quantity": float64(3), "quantity": float64(5), "immediate": false}))
			add(name+"/proration invoice", parent, with(m(map[string]any{"item_id": "si_1", "plan_id": "pln_seat", "old_quantity": float64(3), "quantity": float64(5), "immediate": true}), prorations["invoice"]))
			add(name+"/legacy", parent, m(map[string]any{"item_id": "si_1", "quantity": float64(5), "immediate": true}))
		default:
			add(name, parent, m(map[string]any{}))
		}
	}
	return cs
}

func buildGoldenCases(t *testing.T) []goldenCase {
	t.Helper()
	var cases []goldenCase
	for _, c := range goldenActionCases(t) {
		l := describeSubscriptionAction(c.action, c.meta, goldenLabels)
		l = activity.Stamp(l, goldenAt, time.Time{}, false)
		l.ID = c.name
		cases = append(cases, goldenCase{Case: c.name, Line: l, Text: lineText(l)})
	}
	// The actor comes from the audit row, in the fixed set's words.
	for _, a := range []struct{ kind, name string }{{"user", "alice@example.com"}, {"api_key", "billing-sync"}, {"customer", "Northwind"}, {"system", ""}} {
		l := describeSubscriptionAction(domain.AuditActionCreate, map[string]any{}, goldenLabels)
		l.Actor = activity.ActorFromAudit(a.kind, a.name)
		l = activity.Stamp(l, goldenAt, time.Time{}, false)
		name := "actor/" + a.kind
		l.ID = name
		cases = append(cases, goldenCase{Case: name, Line: l, Text: lineText(l)})
	}
	return cases
}

func TestActivityLinesGolden(t *testing.T) {
	cases := buildGoldenCases(t)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cases); err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		var old []goldenCase
		_ = json.Unmarshal(want, &old)
		byName := map[string]goldenCase{}
		for _, c := range old {
			byName[c.Case] = c
		}
		shown := 0
		for _, c := range cases {
			o, ok := byName[c.Case]
			if ok && strings.Join(o.Text, "\n") == strings.Join(c.Text, "\n") {
				continue
			}
			if shown++; shown > 10 {
				break
			}
			t.Errorf("%s:\n  golden: %q\n  now:    %q", c.Case, o.Text, c.Text)
		}
		t.Fatalf("activity lines differ from %s (%d cases now, %d in the file); review the diff and regenerate with -update", goldenPath, len(cases), len(old))
	}
}

// TestActivityLines_NoFallback is the test that would have caught B2: no
// token a writer emits may reach the fallback line, and no line shows a raw
// code, cents or a Go error.
func TestActivityLines_NoFallback(t *testing.T) {
	w := scanSubscriptionAuditWriters(t)
	for action := range w.actions {
		if action == domain.AuditActionUpdate || action == domain.AuditActionSubscriptionItemUpdated {
			continue // worded by their metadata action, below
		}
		if l := describeSubscriptionAction(action, map[string]any{}, lineLabels{}); l.Kind == kindChanged {
			t.Errorf("writers emit audit action %q, and the timeline renders it as the fallback %q", action, l.Title)
		}
	}
	for sub := range w.subActions {
		l := describeSubscriptionAction(subActionParent(sub), map[string]any{"action": sub}, lineLabels{})
		if l.Kind == kindChanged {
			t.Errorf("writers emit metadata action %q, and the timeline renders it as the fallback %q", sub, l.Title)
		}
	}
	if l := describeSubscriptionAction("subscription.not_a_writer_token", nil, lineLabels{}); l.Kind != kindChanged || l.Title != "Subscription changed" {
		t.Errorf("an unknown action renders %q, want the one fallback", l.Title)
	}

	// B2: only the operator's End trial ends a trial early.
	if l := describeSubscriptionAction(domain.AuditActionUpdate, map[string]any{"action": domain.SubAuditTrialEnded, "triggered_by": "schedule"}, lineLabels{}); l.Title != "Trial ended" {
		t.Errorf("a trial that ran out reads %q, want Trial ended (B2)", l.Title)
	}
	if l := describeSubscriptionAction(domain.AuditActionUpdate, map[string]any{"action": domain.SubAuditTrialEnded}, lineLabels{}); l.Title != "Trial ended early" {
		t.Errorf("a trial ended by hand reads %q, want Trial ended early", l.Title)
	}

	for _, c := range buildGoldenCases(t) {
		l := c.Line
		if l.Kind == kindChanged {
			t.Errorf("%s: renders the fallback", c.Case)
		}
		if strings.ContainsAny(l.Title, "_—()") || strings.Contains(l.Title, " by ") {
			t.Errorf("%s: title %q has a code, a dash, brackets or an actor", c.Case, l.Title)
		}
		text := strings.Join(c.Text, "\n")
		for _, raw := range []string{"¢", "pq:", "deadlock", "subscription.", "item_", "proration_", "Immediate", "At next period", "qty "} {
			if strings.Contains(text, raw) {
				t.Errorf("%s: shows %q: %q", c.Case, raw, text)
			}
		}
		// The legacy detail/detail_timestamp pair can carry a time only from
		// the last segment.
		for i, seg := range l.Detail {
			for _, p := range seg {
				if p.Time != nil && i != len(l.Detail)-1 {
					t.Errorf("%s: a time part in segment %d of %d", c.Case, i+1, len(l.Detail))
				}
			}
		}
		// A warning or failure row says what it means, unless its title does.
		if (l.Tone == activity.ToneWarning || l.Tone == activity.ToneFailure) && l.Problem == "" && l.Title != "Collection paused" {
			t.Errorf("%s: %s row %q has no problem line", c.Case, l.Tone, l.Title)
		}
		// Green only for money (ADR-128 decision 3): no subscription line
		// moves money itself.
		if l.Tone == activity.ToneSuccess {
			t.Errorf("%s: %q is green", c.Case, l.Title)
		}
	}
}

// TestLegacyDetail pins how a line's meta reaches the older detail and
// detail_timestamp fields the card reads today.
func TestLegacyDetail(t *testing.T) {
	at := time.Date(2026, 11, 19, 5, 3, 0, 0, time.UTC)
	for _, c := range []struct {
		name         string
		detail       []activity.Segment
		want, wantTS string
	}{
		{"no meta", nil, "", ""},
		{"text and money", []activity.Segment{activity.Text("Starter → Pro"), {activity.T("Credit "), activity.M(1980, "EUR")}}, "Starter → Pro · Credit €19.80", ""},
		{"trailing time", []activity.Segment{{activity.T("Trial now ends "), activity.At(at)}}, "Trial now ends", at.Format(time.RFC3339)},
		{"time in brackets", []activity.Segment{{activity.T("At the end of the current period ("), activity.At(at), activity.T(")")}}, "At the end of the current period", at.Format(time.RFC3339)},
		{"text then time", []activity.Segment{activity.Text("Seat"), {activity.T("Resumes "), activity.At(at)}}, "Seat · Resumes", at.Format(time.RFC3339)},
		{"a link", []activity.Segment{{activity.T("Invoice "), activity.L(activity.LinkInvoice, "vlx_inv_1", "VLX-000123"), activity.T(" issued early")}}, "Invoice VLX-000123 issued early", ""},
	} {
		got, ts := legacyDetail(activity.Line{Detail: c.detail})
		if got != c.want || ts != c.wantTS {
			t.Errorf("%s: got %q, %q; want %q, %q", c.name, got, ts, c.want, c.wantTS)
		}
	}
}
