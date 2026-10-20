package invoice

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
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
//	go test ./internal/invoice -run TestActivityLinesGolden -update
//
// The reviewer reads the JSON diff: it is every invoice activity line the
// backend can build, in the words an operator sees.
var update = flag.Bool("update", false, "rewrite the activity-line golden file")

const goldenPath = "testdata/activity_lines.golden.json"

// ── Writer tokens, read from the code that writes them ──
//
// The golden cases and the no-fallback assertion range over these, so a
// token a writer starts emitting appears here without anyone remembering to
// add it. B1 and B2 were both a writer's token and a renderer's switch
// drifting apart.

// repoRoot is the module root, two levels above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// constsOfType returns name → value for the string constants of one named
// type in a package directory.
func constsOfType(t *testing.T, dir, typeName string) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", p, err)
		}
		files = append(files, f)
	}
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				id, ok := vs.Type.(*ast.Ident)
				if !ok || id.Name != typeName {
					continue
				}
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, _ := strconv.Unquote(lit.Value)
							out[name.Name] = v
						}
					}
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("no %s constants found in %s", typeName, dir)
	}
	return out
}

// grepSource returns the first submatch of re over every non-test Go file
// under dir, deduplicated and sorted.
func grepSource(t *testing.T, dir string, re *regexp.Regexp) []string {
	t.Helper()
	seen := map[string]bool{}
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func sortedValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// writerTokens are the tokens the invoice timeline's sources are written
// with, each read from its writer.
type writerTokens struct {
	dunningEventTypes []string // event types the dunning package writes
	allDunningTypes   []string // every DunningEventType constant
	resolutions       []string // resolutions a resolved run can hold
	refundStatuses    []string
	disputeStatuses   []string
	attemptOutcomes   []string
	attemptTriggers   []string
	billingReasons    []string
	emailTypes        []string // the invoice timeline's email allowlist
	outboxStatuses    []string
	deliveryStates    []string
	emailTriggers     []string
	cnReasons         []string // reason codes the engine and the refund form write
}

func readWriterTokens(t *testing.T) writerTokens {
	t.Helper()
	root := repoRoot(t)
	domainDir := filepath.Join(root, "internal", "domain")
	var w writerTokens

	eventConsts := constsOfType(t, domainDir, "DunningEventType")
	w.allDunningTypes = sortedValues(eventConsts)
	for _, name := range grepSource(t, filepath.Join(root, "internal", "dunning"), regexp.MustCompile(`EventType:\s+domain\.(DunningEvent\w+)`)) {
		v, ok := eventConsts[name]
		if !ok {
			t.Fatalf("dunning writes %s, which is not a DunningEventType constant", name)
		}
		w.dunningEventTypes = append(w.dunningEventTypes, v)
	}
	sort.Strings(w.dunningEventTypes)

	// A resolved run holds the resolution its resolver passed. Every
	// resolution named anywhere in the code is a candidate, except the two
	// the exhaust path stores on runs that are NOT resolved (escalated, or
	// still active with a failed final action). manually_resolved has no
	// writer since 0170 but its rows remain (legacy).
	resConsts := constsOfType(t, domainDir, "DunningResolution")
	for _, name := range grepSource(t, filepath.Join(root, "internal"), regexp.MustCompile(`domain\.(Resolution\w+)`)) {
		v, ok := resConsts[name]
		if !ok {
			continue
		}
		if v == string(domain.ResolutionRetriesExhausted) || v == string(domain.ResolutionActionFailed) {
			continue
		}
		w.resolutions = append(w.resolutions, v)
	}
	sort.Strings(w.resolutions)

	w.refundStatuses = sortedValues(constsOfType(t, domainDir, "RefundStatus"))
	w.disputeStatuses = sortedValues(constsOfType(t, domainDir, "DisputeStatus"))
	w.attemptOutcomes = sortedValues(constsOfType(t, domainDir, "ChargeAttemptOutcome"))
	w.attemptTriggers = sortedValues(constsOfType(t, domainDir, "ChargeAttemptTrigger"))
	w.billingReasons = sortedValues(constsOfType(t, domainDir, "InvoiceBillingReason"))

	outbox, err := os.ReadFile(filepath.Join(root, "internal", "email", "outbox.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(outbox)
	allow := regexp.MustCompile(`(?s)FROM email_outbox\s+WHERE email_type IN \(([^)]*)\)`).FindStringSubmatch(src)
	if allow == nil {
		t.Fatal("the invoice timeline's email_type allowlist was not found in email/outbox.go")
	}
	for _, m := range regexp.MustCompile(`'(\w+)'`).FindAllStringSubmatch(allow[1], -1) {
		w.emailTypes = append(w.emailTypes, m[1])
	}
	statuses := map[string]bool{}
	for _, m := range regexp.MustCompile(`Outbox\w+\s*=\s*"(\w+)"`).FindAllStringSubmatch(src, -1) {
		statuses[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`SET status = '(\w+)'`).FindAllStringSubmatch(src, -1) {
		statuses[m[1]] = true
	}
	for s := range statuses {
		w.outboxStatuses = append(w.outboxStatuses, s)
	}
	sort.Strings(w.outboxStatuses)
	for _, m := range regexp.MustCompile(`Delivery\w+\s*=\s*"(\w+)"`).FindAllStringSubmatch(src, -1) {
		w.deliveryStates = append(w.deliveryStates, m[1])
	}
	sort.Strings(w.deliveryStates)

	triggers := map[string]bool{}
	for _, v := range grepSource(t, filepath.Join(root, "internal"), regexp.MustCompile(`noPMTrigger\w+\s*=\s*"(\w+)"`)) {
		triggers[v] = true
	}
	for _, v := range grepSource(t, filepath.Join(root, "internal"), regexp.MustCompile(`NotifyNoPaymentMethod\([^)]*"(\w+)"\)`)) {
		triggers[v] = true
	}
	for v := range triggers {
		w.emailTriggers = append(w.emailTriggers, v)
	}
	sort.Strings(w.emailTriggers)

	reasons := map[string]bool{}
	for _, v := range grepSource(t, filepath.Join(root, "internal", "subscription"), regexp.MustCompile(`return "(subscription_\w+)"`)) {
		reasons[v] = true
	}
	for _, v := range grepSource(t, filepath.Join(root, "internal", "billing"), regexp.MustCompile(`reliefTx\([^)]*"(subscription_\w+)"`)) {
		reasons[v] = true
	}
	for _, v := range grepSource(t, filepath.Join(root, "internal", "creditnote"), regexp.MustCompile(`reason = "(\w+)"`)) {
		reasons[v] = true
	}
	for r := range validRefundReasons {
		reasons[r] = true
	}
	for r := range reasons {
		w.cnReasons = append(w.cnReasons, r)
	}
	sort.Strings(w.cnReasons)

	for name, list := range map[string][]string{
		"dunning event types": w.dunningEventTypes, "resolutions": w.resolutions, "email types": w.emailTypes,
		"outbox statuses": w.outboxStatuses, "delivery states": w.deliveryStates, "email triggers": w.emailTriggers,
		"credit-note reasons": w.cnReasons,
	} {
		if len(list) == 0 {
			t.Fatalf("read no %s from the code; a scan pattern no longer matches its writer", name)
		}
	}
	return w
}

// ── Golden cases ──

type goldenCase struct {
	Case string        `json:"case"`
	Line activity.Line `json:"line"`
	// Text is the line as the Activity card shows it today: title, amount,
	// problem and meta, one per row.
	Text []string `json:"text"`
}

func lineText(l activity.Line) []string {
	title := l.Title
	if l.Amount != nil {
		title += " · " + activity.FormatMoney(*l.Amount)
	}
	out := []string{title}
	if l.Problem != "" {
		out = append(out, "problem: "+l.Problem)
	}
	if m := activity.MetaText(l); m != "" {
		out = append(out, "meta: "+m)
	}
	return append(out, "tone: "+string(l.Tone))
}

var goldenAt = time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)

func buildGoldenCases(t *testing.T, w writerTokens) []goldenCase {
	t.Helper()
	var cases []goldenCase
	add := func(name string, l activity.Line) {
		l = activity.Stamp(l, goldenAt, time.Time{}, false)
		l.ID = name
		cases = append(cases, goldenCase{Case: name, Line: l, Text: lineText(l)})
	}
	person := activity.ActorFromAudit("user", "alice@example.com")
	key := activity.ActorFromAudit("api_key", "billing-sync")
	auto := activity.ActorFromAudit("system", "")
	sub := activity.L(activity.LinkSubscription, "vlx_sub_1", "Starter")
	inv := domain.Invoice{
		ID: "vlx_inv_1", InvoiceNumber: "VLX-000001", Currency: "USD", SubscriptionID: "vlx_sub_1",
		TotalAmountCents: 4900, AmountDueCents: 4900, BillingPeriodDisplay: "Feb 1, 2027 – Feb 28, 2027",
	}

	// I-1: every billing reason, and the plan-change kinds.
	for _, br := range w.billingReasons {
		i := inv
		i.BillingReason = domain.InvoiceBillingReason(br)
		add("created/"+br, createdLine(i, auto, &sub))
	}
	for _, ct := range []domain.ItemChangeType{domain.ItemChangeTypePlan, domain.ItemChangeTypeQuantity, domain.ItemChangeTypeAdd, ""} {
		i := inv
		i.BillingReason, i.SourceChangeType = domain.BillingReasonSubscriptionUpdate, ct
		add("created/subscription_update/"+string(ct), createdLine(i, person, &sub))
	}
	manual := inv
	manual.BillingReason, manual.SubscriptionID = domain.BillingReasonManual, ""
	add("created/manual by person", createdLine(manual, person, nil))
	legacy := inv
	legacy.BillingReason = ""
	add("created/legacy, no reason, no actor", createdLine(legacy, nil, nil))

	// I-2 to I-6.
	add("finalized/by API key", finalizedLine(inv, key))
	add("voided/by person", voidedLine(inv, person))
	due := int64(4900)
	add("uncollectible/by payment recovery", uncollectibleLine(inv, &due, true, auto))
	add("uncollectible/by person", uncollectibleLine(inv, &due, false, person))
	add("uncollectible/recovered later, no amount on record", uncollectibleLine(inv, nil, false, nil))
	paid := inv
	paid.AmountPaidCents, paid.StripePaymentIntentID, paid.PaymentCardBrand, paid.PaymentCardLast4 = 4900, "pi_1", "visa", "4242"
	add("paid/card", paidLine(paid, 0, "", nil, nil))
	add("paid/card after 1 retry", paidLine(paid, 1, "", nil, nil))
	add("paid/card after 3 retries", paidLine(paid, 3, "", nil, nil))
	credits := inv
	credits.AmountPaidCents, credits.CreditsAppliedCents = 0, 4900
	add("paid/with credits", paidLine(credits, 0, "", auto, nil))
	offline := inv
	offline.AmountPaidCents, offline.StripePaymentIntentID = 3900, "out_of_band:abc"
	add("paid/offline with reference", paidLine(offline, 0, "Wire 1042", person, nil))
	add("paid/offline without reference", paidLine(offline, 0, "", key, nil))
	cad := paid
	cad.Currency, cad.AmountPaidCents = "CAD", 123456789
	add("paid/CAD money format", paidLine(cad, 0, "", nil, nil))
	waived := inv
	waived.AmountPaidCents = 0
	note := activity.L(activity.LinkCreditNote, "vlx_cn_1", "CN-0001")
	add("paid/by credit note, after 3 failed retries", paidLine(waived, 3, "", key, &note))
	partly := credits
	partly.CreditsAppliedCents = 2000
	add("paid/by credit note, credits paid part", paidLine(partly, 0, "", person, &note))

	// I-7: every attempt outcome × trigger, every mapped decline code, one
	// unmapped code, and a wrapped provider text.
	for _, o := range w.attemptOutcomes {
		for _, tr := range w.attemptTriggers {
			add("attempt/"+o+"/"+tr, attemptLine(domain.InvoiceChargeAttempt{
				Outcome: domain.ChargeAttemptOutcome(o), Trigger: domain.ChargeAttemptTrigger(tr), AmountCents: 4900,
				ProviderReason: "Your card was declined.", DeclineCode: "generic_decline",
			}, inv))
		}
	}
	codes := activity.DeclineCodes()
	sort.Strings(codes)
	for _, c := range append(codes, "some_new_code") {
		add("decline/"+c, attemptLine(domain.InvoiceChargeAttempt{
			Outcome: domain.ChargeAttemptFailed, AmountCents: 4900, DeclineCode: c, ProviderReason: "Your card was declined for a reason we do not map.",
		}, inv))
	}
	add("decline/no code, wrapped provider text", attemptLine(domain.InvoiceChargeAttempt{
		Outcome: domain.ChargeAttemptFailed, AmountCents: 4900, ProviderReason: "payment failed: Your card was declined.",
	}, inv))

	// I-8: every refund status × every mix of the three parts, every reason
	// code, operator text, and a failed refund's provider reason.
	for _, rs := range w.refundStatuses {
		for mix := 0; mix < 8; mix++ {
			cn := domain.CreditNote{ID: "vlx_cn_1", CreditNoteNumber: "CN-000012", Currency: "USD", Reason: "Billing error", RefundStatus: domain.RefundStatus(rs)}
			var parts []string
			if mix&1 != 0 {
				cn.RefundAmountCents = 1500
				parts = append(parts, "card")
			}
			if mix&2 != 0 {
				cn.CreditAmountCents = 500
				parts = append(parts, "credit")
			}
			if mix&4 != 0 {
				cn.OutOfBandAmountCents = 300
				parts = append(parts, "outside")
			}
			cn.TotalCents = cn.RefundAmountCents + cn.CreditAmountCents + cn.OutOfBandAmountCents
			if cn.TotalCents == 0 {
				cn.TotalCents = 1000 // reduces the amount due of an unpaid invoice
				parts = append(parts, "amount due")
			}
			add(fmt.Sprintf("credit_note/%s/%s", rs, strings.Join(parts, "+")), creditNoteLine(cn, person, ""))
		}
	}
	for _, r := range append(w.cnReasons, "Goodwill credit for the outage") {
		add("credit_note/reason/"+r, creditNoteLine(domain.CreditNote{
			ID: "vlx_cn_1", CreditNoteNumber: "CN-000012", Currency: "USD", Reason: r, TotalCents: 1000, CreditAmountCents: 1000,
		}, auto, ""))
	}
	add("credit_note/failed refund with provider reason", creditNoteLine(domain.CreditNote{
		ID: "vlx_cn_1", CreditNoteNumber: "CN-000012", Currency: "USD", TotalCents: 1000, RefundAmountCents: 1000,
		RefundStatus: domain.RefundFailed,
	}, key, "expired_or_canceled_card"))

	// I-9 to I-13: every email type × every outbox status × every delivery
	// state; every setup-email trigger; a classified send failure; a credit
	// note email naming its note.
	for _, et := range w.emailTypes {
		for _, st := range w.outboxStatuses {
			for _, ds := range w.deliveryStates {
				l, ok := emailLine(EmailEventRow{EmailType: et, Status: st, DeliveryState: ds, To: "billing@acme.test"}, nil)
				if !ok {
					t.Fatalf("email type %q on the invoice timeline's allowlist has no name", et)
				}
				add(fmt.Sprintf("email/%s/%s/%s", et, st, ds), l)
			}
		}
	}
	for _, tr := range w.emailTriggers {
		l, _ := emailLine(EmailEventRow{EmailType: "payment_setup_request", Status: "dispatched", Trigger: tr, To: "billing@acme.test"}, nil)
		add("email/setup trigger/"+tr, l)
	}
	for _, f := range []string{activity.SendFailureSMTPNotConfigured, activity.SendFailureRecipientSuppressed, activity.SendFailureInvalidRecipient} {
		l, _ := emailLine(EmailEventRow{EmailType: "invoice", Status: "failed", SendFailure: f, To: "billing@acme.test"}, nil)
		add("email/send failure/"+f, l)
	}
	cnLink := activity.L(activity.LinkCreditNote, "vlx_cn_1", "CN-000012")
	l, _ := emailLine(EmailEventRow{EmailType: "credit_note", Status: "dispatched", DeliveryState: "delivered", To: "billing@acme.test"}, &cnLink)
	add("email/credit note email names its note", l)

	// I-14 to I-17: every dunning event type (writer or not), every start
	// cause, every retry reason, every escalation outcome on the invoice page
	// (write-off folded when the uncollectible row shows) and without the
	// fold, and every resolution.
	for _, et := range w.allDunningTypes {
		add("dunning/type/"+et, dunningLine(domain.InvoiceDunningEvent{EventType: domain.DunningEventType(et), AttemptCount: 1}, dunningContext{}))
	}
	for _, cause := range []string{string(domain.DunningCausePaymentFailed), string(domain.DunningCauseNoPaymentMethod), ""} {
		add("dunning/started/"+cause, dunningLine(domain.InvoiceDunningEvent{EventType: domain.DunningEventStarted, Reason: cause}, dunningContext{}))
	}
	reminder := EmailEventRow{EmailType: "dunning_warning", Status: "dispatched", DeliveryState: "delivered"}
	for _, reason := range []string{"payment failed: Your card has insufficient funds.", string(domain.DunningCauseNoPaymentMethod),
		domain.ErrNoPaymentMethodOnRetry.Error(), domain.DunningRetryReasonCardRefused, "succeeded"} {
		add("dunning/retry/"+reason, dunningLine(domain.InvoiceDunningEvent{EventType: domain.DunningEventRetryAttempted, AttemptCount: 2, Reason: reason}, dunningContext{email: &reminder}))
	}
	final := EmailEventRow{EmailType: "dunning_escalation", Status: "dispatched", DeliveryState: "bounced"}
	for mask := 0; mask < 8; mask++ {
		o := domain.DunningEscalationOutcome{SubscriptionPaused: mask&1 != 0, SubscriptionCanceled: mask&2 != 0, InvoiceWrittenOff: mask&4 != 0}
		reason := o.Reason()
		name := reason
		if name == "" {
			name = "(nothing changed)"
		}
		for _, page := range []struct {
			label string
			fold  bool
		}{{"invoice page", true}, {"no fold", false}} {
			add("dunning/escalated/"+name+"/"+page.label, dunningLine(
				domain.InvoiceDunningEvent{EventType: domain.DunningEventEscalated, AttemptCount: 3, Reason: reason},
				dunningContext{writeOffShown: page.fold && o.InvoiceWrittenOff, email: &final, sub: &sub}))
		}
	}
	for _, res := range w.resolutions {
		add("dunning/resolved/"+res, dunningLine(domain.InvoiceDunningEvent{EventType: domain.DunningEventResolved},
			dunningContext{run: domain.InvoiceDunningRun{Resolution: domain.DunningResolution(res)}}))
	}
	add("dunning/resolved/by person", dunningLine(domain.InvoiceDunningEvent{EventType: domain.DunningEventResolved},
		dunningContext{run: domain.InvoiceDunningRun{Resolution: domain.ResolutionInvoiceVoided}, resolvedBy: person}))

	// Disputes: every status, open and closed.
	dueBy := goldenAt.Add(14 * 24 * time.Hour)
	closed := goldenAt.Add(30 * 24 * time.Hour)
	for _, ds := range w.disputeStatuses {
		d := domain.InvoiceDispute{
			ID: "vlx_dsp_1", StripeDisputeID: "du_1", Status: domain.DisputeStatus(ds), Reason: "product_not_received",
			AmountCents: 4500, Currency: "USD", OpenedAt: goldenAt, EvidenceDueBy: &dueBy,
		}
		if d.Status.Closed() {
			d.ClosedAt = &closed
		}
		opened, c := disputeLines(d)
		add("dispute/"+ds+"/opened", opened)
		if c != nil {
			add("dispute/"+ds+"/closed", *c)
		}
	}
	return cases
}

func TestActivityLinesGolden(t *testing.T) {
	w := readWriterTokens(t)
	cases := buildGoldenCases(t, w)

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

// TestActivityLines_NoFallback is the test that would have caught B1 and
// B2: no token a writer emits may reach a fallback line or lose its words.
func TestActivityLines_NoFallback(t *testing.T) {
	w := readWriterTokens(t)

	for _, et := range w.dunningEventTypes {
		l := dunningLine(domain.InvoiceDunningEvent{EventType: domain.DunningEventType(et), AttemptCount: 1},
			dunningContext{run: domain.InvoiceDunningRun{Resolution: domain.ResolutionPaymentRecovered}})
		if l.Kind == "payment_recovery.updated" {
			t.Errorf("dunning writes event type %q, and the invoice timeline renders it as the fallback %q", et, l.Title)
		}
	}
	// Every outcome token the escalation writer can store has words.
	for mask := 0; mask < 8; mask++ {
		o := domain.DunningEscalationOutcome{SubscriptionPaused: mask&1 != 0, SubscriptionCanceled: mask&2 != 0, InvoiceWrittenOff: mask&4 != 0}
		for tok := range escalationTokens(o.Reason()) {
			if tok != tokenSubscriptionPaused && tok != tokenSubscriptionCanceled && tok != tokenInvoiceWrittenOff {
				t.Errorf("escalation writes outcome token %q, which escalationProblem has no words for", tok)
			}
		}
		if p := escalationProblem(o.Reason(), false); p == "" {
			t.Errorf("escalation outcome %+v renders no problem line on a page without the write-off fold", o)
		}
		if strings.Contains(escalationProblem(o.Reason(), false), "manual review") {
			t.Errorf("escalation outcome %+v still reads as manual review (B1)", o)
		}
	}
	for _, res := range w.resolutions {
		if _, _, ok := recoveryOutcome(domain.DunningResolution(res)); !ok {
			t.Errorf("a resolved run can hold %q, which has no outcome words", res)
		}
	}
	if o, _, _ := recoveryOutcome(domain.ResolutionManuallyResolved); o != "Closed unpaid" {
		t.Errorf("legacy manually_resolved reads %q, want Closed unpaid (B16)", o)
	}
	for _, et := range w.emailTypes {
		if _, ok := activity.EmailName(et); !ok {
			t.Errorf("the invoice timeline lists email type %q, which has no name", et)
		}
	}
	for _, st := range w.outboxStatuses {
		for _, ds := range w.deliveryStates {
			if out := activity.Outcome(st, ds, ""); out.Word == "" {
				t.Errorf("outbox status %q / delivery %q has no outcome word", st, ds)
			}
		}
	}
	for _, tr := range w.emailTriggers {
		if emailCause(tr) == "" {
			t.Errorf("setup emails are enqueued with trigger %q, which has no cause words", tr)
		}
	}
	for _, br := range w.billingReasons {
		if br == string(domain.BillingReasonManual) {
			continue
		}
		if whyInvoiceExists(domain.Invoice{BillingReason: domain.InvoiceBillingReason(br)}) == nil {
			t.Errorf("invoices are made with billing reason %q, which has no words", br)
		}
	}
	for _, r := range w.cnReasons {
		if activity.CreditNoteReason(r) == r {
			t.Errorf("credit notes are written with reason code %q, which shows raw (B8)", r)
		}
	}
	for _, rs := range w.refundStatuses {
		l := creditNoteLine(domain.CreditNote{CreditNoteNumber: "CN-1", TotalCents: 100, RefundAmountCents: 100, RefundStatus: domain.RefundStatus(rs)}, nil, "")
		if rs != string(domain.RefundSucceeded) && (l.Problem == "" || l.Tone == activity.ToneSuccess) {
			t.Errorf("refund status %q reads as a plain success", rs)
		}
	}
	for _, ds := range w.disputeStatuses {
		s := domain.DisputeStatus(ds)
		if !s.Closed() {
			continue
		}
		at := goldenAt
		_, c := disputeLines(domain.InvoiceDispute{Status: s, ClosedAt: &at, StripeDisputeID: "du_1"})
		if c == nil || c.Kind != "dispute.closed" {
			t.Errorf("closed dispute status %q has no closed row", ds)
			continue
		}
		if c.Problem == "" && (len(c.Detail) < 2 || activity.SegmentText(c.Detail[0]) == "") {
			t.Errorf("closed dispute status %q does not say its outcome", ds)
		}
	}
	// Three setup-email triggers, three distinct causes: two such rows on
	// one invoice must never read alike again.
	seen := map[string]string{}
	for _, tr := range w.emailTriggers {
		c := emailCause(tr)
		if prev, dup := seen[c]; dup {
			t.Errorf("triggers %q and %q read alike: %q", prev, tr, c)
		}
		seen[c] = tr
	}
}
