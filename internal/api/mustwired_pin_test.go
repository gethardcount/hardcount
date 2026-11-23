package api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestMustWired_CoversEveryAuditedComponent pins the audit.MustWired list in
// wire.go against the SetAuditLogger wiring cluster: every component that
// gets an in-tx audit emitter via <name>.SetAuditLogger(auditLogger) must
// also appear in the audit.MustWired(...) call, so a future audited domain
// can't silently skip the boot-time nil check. Source-text based on purpose:
// the wiring lives in ONE composition-root file, and a text pin is the
// simplest gate that fails when a new SetAuditLogger line lands without the
// matching MustWired entry.
//
// THERE ARE NO EXEMPTIONS. There used to be 14 — every handler-level receiver —
// on the written grounds that "handlers keep the *audit.Logger concrete type and
// fail loudly at compile time instead." BOTH HALVES OF THAT WERE FALSE. MOST of
// those handlers take an INTERFACE (pricing.AuditWriter, subscription.auditRecorder,
// invoice.auditWriter, dunning.AuditWriter, …), so a nil emitter does not even
// fail to type-check — and every one of them, INCLUDING the two that do hold the
// concrete *audit.Logger (customerH, creditNoteH), nil-GUARDS its emission and
// silently skips it. A concrete type buys nothing when the field is still nil-able
// and the call site tolerates nil: a missing emitter compiles, boots, serves
// traffic, and writes nothing.
//
// So a forgotten SetAuditLogger line un-audited an entire domain in silence, with
// the route registry still declaring those routes `explicit` and every gate in
// this package green. The API-key lifecycle — mint, revoke, rotate — sat behind
// exactly that hole. If you are about to add an exemption here, you are about to
// reopen it: MustWired is generic (it reflects the audit field out of any struct),
// so there is no kind of component it cannot check.
func TestMustWired_CoversEveryAuditedComponent(t *testing.T) {
	src, err := os.ReadFile("wire.go")
	if err != nil {
		t.Fatalf("read wire.go: %v", err)
	}
	text := string(src)

	mws := regexp.MustCompile(`audit\.MustWired\(([^)]*)\)`).FindAllStringSubmatch(text, -1)
	if len(mws) == 0 {
		t.Fatal("audit.MustWired call not found in wire.go")
	}
	wired := map[string]bool{}
	for _, mw := range mws {
		for _, name := range strings.Split(mw[1], ",") {
			// audit.MustWired(&bootstrapDeps) — the arg carries an ampersand the
			// declaration site does not. Strip it, or the gate reports a component
			// as un-gated when it is right there in the call.
			wired[strings.TrimPrefix(strings.TrimSpace(name), "&")] = true
		}
	}

	// EVERY receiver of SetAuditLogger(auditLogger) — service, adapter or handler.
	for _, m := range regexp.MustCompile(`(\w+)\.SetAuditLogger\(auditLogger\)`).FindAllStringSubmatch(text, -1) {
		recv := m[1]
		if !wired[recv] {
			t.Errorf("%s.SetAuditLogger is wired in wire.go but %s is missing from audit.MustWired(...) — add it, so that DROPPING that wiring line fails loudly at boot instead of silently un-auditing %s's routes", recv, recv, recv)
		}
	}

	// A struct literal can hand over the logger too (hostedInvoiceStripe,
	// noPMNotifier, emailSettingsH). The grep above cannot see it, so each
	// literal's variable must be listed as well.
	literals := structLiteralAuditWirings(text)
	if len(literals) == 0 {
		t.Fatal("found ZERO struct-literal audit wirings — either the style is gone (delete this gate) or the pattern stopped matching (fix it). A gate that matches nothing is not a gate.")
	}
	for _, m := range literals {
		recv := enclosingVar(text, m[0])
		if recv == "" {
			t.Errorf("a struct literal at offset %d hands over auditLogger but this test cannot name the variable it is assigned to — teach enclosingVar the new shape rather than letting an un-gated emitter through", m[0])
			continue
		}
		if !wired[recv] {
			t.Errorf(`%s is handed auditLogger by STRUCT LITERAL but is missing from audit.MustWired(...).

Its emitter is almost certainly nil-guarded (they all are), so DROPPING that wiring
line would not fail to compile and would not fail a test — it would silently
un-audit %s's routes. Pass it to audit.MustWired.`, recv, recv)
		}
	}
}

// structLiteralAuditWirings finds every place a struct literal hands the logger
// over — on its own line (`Audit: auditLogger,`) OR inline in a one-line literal
// (`&adapter{clients: x, audit: auditLogger}`). The first version only matched the
// line-anchored form and so could not see hostedInvoiceStripe at all: a gate that
// sees one FORM has a hole shaped like the other form, which is the same mistake
// one level down.
func structLiteralAuditWirings(text string) [][]int {
	return regexp.MustCompile(`[Aa]udit\w*:\s+auditLogger[,}]`).FindAllStringIndex(text, -1)
}

// enclosingVar walks BACKWARD from a struct-literal field to the variable the
// literal is assigned to: `noPMNotifier := &noPaymentMethodNotifierAdapter{` →
// "noPMNotifier". A literal passed as a constructor's last argument names the
// constructed variable: `creditNoteH := creditnote.NewHandler(creditNoteSvc, creditnote.HandlerDeps{`
// → "creditNoteH". Nil when it cannot tell, which the caller treats as a failure —
// a gate that silently gives up is the thing being gated against.
func enclosingVar(text string, at int) string {
	// Same line first: `x := &T{a: 1, audit: auditLogger}`.
	lineStart := strings.LastIndex(text[:at], "\n") + 1
	if m := regexp.MustCompile(`^\s*(\w+)\s*:?=\s*&?[\w.]+\{`).FindStringSubmatch(text[lineStart:at]); m != nil {
		return m[1]
	}
	// Otherwise the nearest multi-line literal opened above.
	assign := regexp.MustCompile(`(?m)^\s*(\w+)\s*:?=\s*(?:[\w.]+\([^()\n]*)?&?[\w.]+\{\s*$`)
	best := ""
	for _, m := range assign.FindAllStringSubmatchIndex(text, -1) {
		if m[0] < at {
			best = text[m[2]:m[3]]
			continue
		}
		break
	}
	return best
}
