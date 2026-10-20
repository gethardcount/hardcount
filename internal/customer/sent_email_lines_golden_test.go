package customer

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/platform/activity"
)

// update regenerates testdata/sent_email_lines.golden.json:
//
//	go test ./internal/customer -run TestSentEmailLinesGolden -update
//
// The reviewer reads the JSON diff: every Sent emails line shape, in the
// words an operator sees. The outcome words themselves (every status and
// delivery state) are in the invoice package's golden file, built by the
// same activity.EmailLine.
var update = flag.Bool("update", false, "rewrite the Sent emails line golden file")

const sentEmailGoldenPath = "testdata/sent_email_lines.golden.json"

// outboxAllowlists reads the email types the customer's Sent emails list
// and the invoice timeline return, from their SQL in email/outbox.go, and
// the skip reasons the dispatcher records.
func outboxAllowlists(t *testing.T) (customer, invoice, skipReasons []string) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "email", "outbox.go"))
	if err != nil {
		t.Fatal(err)
	}
	list := func(re string) []string {
		m := regexp.MustCompile(re).FindSubmatch(src)
		if m == nil {
			t.Fatalf("allowlist %s not found in email/outbox.go", re)
		}
		var out []string
		for _, v := range regexp.MustCompile(`'(\w+)'`).FindAllSubmatch(m[1], -1) {
			out = append(out, string(v[1]))
		}
		sort.Strings(out)
		return out
	}
	customer = list(`(?s)WHERE eo\.email_type IN \(([^)]*)\)`)
	invoice = list(`(?s)FROM email_outbox\s+WHERE email_type IN \(([^)]*)\)`)
	for _, m := range regexp.MustCompile(`SkipReason\w+\s*=\s*"(\w+)"`).FindAllSubmatch(src, -1) {
		skipReasons = append(skipReasons, string(m[1]))
	}
	sort.Strings(skipReasons)
	if len(skipReasons) == 0 {
		t.Fatal("no skip reasons found in email/outbox.go")
	}
	return customer, invoice, skipReasons
}

type sentEmailGoldenCase struct {
	Case string        `json:"case"`
	Line activity.Line `json:"line"`
	Text []string      `json:"text"`
}

func sentEmailLineText(l activity.Line) []string {
	out := []string{l.Title}
	if l.Problem != "" {
		out = append(out, "problem: "+l.Problem)
	}
	if m := activity.MetaText(l); m != "" {
		out = append(out, "meta: "+m)
	}
	return append(out, "tone: "+string(l.Tone))
}

func TestSentEmailLinesGolden(t *testing.T) {
	types, _, skipReasons := outboxAllowlists(t)
	created := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	dispatched := created.Add(2 * time.Minute)
	sim := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	base := SentEmailOutboxRow{
		ID: "vlx_emob_1", Recipient: "billing@acme.test", Status: "dispatched", DeliveryState: "delivered",
		CreatedAt: created, DispatchedAt: &dispatched, InvoiceNumber: "VLX-000123", InvoiceID: "vlx_inv_1",
	}

	var cases []sentEmailGoldenCase
	add := func(name string, row SentEmailOutboxRow) {
		l, ok := sentEmailLine(row)
		if !ok {
			t.Fatalf("%s: no line", name)
		}
		cases = append(cases, sentEmailGoldenCase{Case: name, Line: l, Text: sentEmailLineText(l)})
	}
	// Each email type, naming its invoice.
	for _, et := range types {
		r := base
		r.EmailType = et
		add("type/"+et, r)
	}
	// Why one was not sent: each recorded state, and a row skipped before the
	// state was recorded.
	for _, sr := range append(skipReasons, "") {
		r := base
		r.EmailType, r.Status, r.DeliveryState, r.DispatchedAt, r.SkipReason = "dunning_warning", "skipped", "unknown", nil, sr
		name := sr
		if name == "" {
			name = "(not recorded)"
		}
		add("skipped because/"+name, r)
	}
	// Why one could not be sent.
	for _, f := range []string{activity.SendFailureSMTPNotConfigured, activity.SendFailureRecipientSuppressed, activity.SendFailureInvalidRecipient, ""} {
		r := base
		r.EmailType, r.Status, r.DeliveryState, r.DispatchedAt, r.SendFailure = "invoice", "failed", "unknown", nil, f
		name := f
		if name == "" {
			name = "(no plain-words name)"
		}
		add("couldn't be sent/"+name, r)
	}
	// A payment setup email says why it went out.
	for _, tr := range []string{"finalize_no_pm", "auto_charge_retry_no_pm", "operator_resend"} {
		r := base
		r.EmailType, r.Trigger = "payment_setup_request", tr
		add("setup email/"+tr, r)
	}
	// A credit-note email names its credit note: a link while it exists.
	cn := base
	cn.EmailType, cn.CreditNoteNumber, cn.CreditNoteID = "credit_note", "CN-000012", "vlx_cn_1"
	add("credit note email/note exists", cn)
	cn.CreditNoteID = ""
	add("credit note email/note gone", cn)
	// An invoice whose id is unknown is named, not linked.
	noID := base
	noID.EmailType, noID.InvoiceID = "invoice", ""
	add("invoice not linked without its id", noID)
	// On a test clock: dated at the instant that caused it, with the real
	// send time as its Recorded time.
	onClock := base
	onClock.EmailType, onClock.SimEffectiveAt = "payment_receipt", &sim
	add("test clock", onClock)

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cases); err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.MkdirAll(filepath.Dir(sentEmailGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sentEmailGoldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(sentEmailGoldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update): %v", err)
	}
	if !bytes.Equal(want, buf.Bytes()) {
		var old []sentEmailGoldenCase
		_ = json.Unmarshal(want, &old)
		byName := map[string]sentEmailGoldenCase{}
		for _, c := range old {
			byName[c.Case] = c
		}
		for _, c := range cases {
			if o, ok := byName[c.Case]; !ok || strings.Join(o.Text, "\n") != strings.Join(c.Text, "\n") {
				t.Errorf("%s:\n  golden: %q\n  now:    %q", c.Case, o.Text, c.Text)
			}
		}
		t.Fatalf("Sent emails lines differ from %s; review the diff and regenerate with -update", sentEmailGoldenPath)
	}
}

// TestSentEmailLines_NoFallback: every email type the Sent emails list can
// return has words, and the list and the invoice timeline show the same
// types, so one email never has a line on one card and none on the other.
func TestSentEmailLines_NoFallback(t *testing.T) {
	customer, invoice, skipReasons := outboxAllowlists(t)
	for _, et := range customer {
		if _, ok := activity.EmailName(et); !ok {
			t.Errorf("the Sent emails list returns email type %q, which has no name", et)
		}
	}
	if strings.Join(customer, ",") != strings.Join(invoice, ",") {
		t.Errorf("the Sent emails list shows %v, the invoice timeline %v; one email vocabulary means one list", customer, invoice)
	}
	for _, sr := range skipReasons {
		r := SentEmailOutboxRow{EmailType: "payment_failed", Status: "skipped", SkipReason: sr}
		l, _ := sentEmailLine(r)
		if strings.Contains(activity.MetaText(l), "already closed") {
			t.Errorf("skip reason %q reads as the generic %q", sr, activity.MetaText(l))
		}
	}
}
