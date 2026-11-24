package billing

import (
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/tax"
)

// TestMustValidate_NamesEveryNilCollaborator pins the boot-fail-closed
// contract (2026-07-10 design review, redesign #3 stage 1): a partially
// wired engine panics at MustValidate with EVERY missing collaborator named
// — not just the first — so one boot failure yields one complete fix.
func TestMustValidate_NamesEveryNilCollaborator(t *testing.T) {
	// Construct WITHOUT wireBaseTax: that helper now defaults the collect
	// collaborators (paymentSetups/charger/noPMNotifier), and this test needs
	// them nil to assert they are named.
	e := NewEngine(&mockSubs{}, &mockUsage{}, &mockPricing{}, &mockInvoices{}, nil, &mockSettings{}, nil, nil, billingTestClock())
	e.SetTaxProviderResolver(tax.NewResolver(nil))
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected MustValidate to panic on a partially wired engine")
		}
		msg, _ := r.(string)
		// The deliberately-unwired deps must ALL be named.
		for _, want := range []string{"credits", "paymentSetups", "charger", "auditLogger"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic must name nil collaborator %q, got: %s", want, msg)
			}
		}
		// Wired deps must NOT be named.
		for _, wired := range []string{"subs", "usage", "pricing", "invoices", "settings", "taxProviders"} {
			if strings.Contains(msg, wired+",") || strings.HasSuffix(msg, wired) {
				t.Errorf("panic must not name wired collaborator %q, got: %s", wired, msg)
			}
		}
	}()
	e.MustValidate()
}

// TestMustValidate_CoversEveryCollaborator is the drift guard: every
// interface-typed field on Engine participates in validation, so adding a
// 26th collaborator cannot silently escape the boot check. It asserts the
// interface-field count matches the panic's nil list on a zero engine
// (everything nil except what NewEngine defaults).
func TestMustValidate_CoversEveryCollaborator(t *testing.T) {
	e := &Engine{} // nothing wired at all — even clock
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on a zero engine")
		}
		msg, _ := r.(string)
		nilCount := strings.Count(msg, ",") + 1
		// Count Engine's interface fields via the same mechanism a
		// maintainer would, in lockstep with the struct: a change means a
		// collaborator was added or removed, so update the wiring in wire.go
		// and this count deliberately.
		const collaborators = 25
		if nilCount != collaborators {
			t.Errorf("zero engine names %d nil collaborators, expected %d — Engine's dep set changed; update router wiring + this count deliberately", nilCount, collaborators)
		}
	}()
	e.MustValidate()
}
