package audit

import (
	"os"
	"regexp"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
)

// TestAuditWriteErrors_IsNeverLabelledByTenant is the regression lock for an
// unbounded-cardinality metric.
//
// hardcount_audit_write_errors_total used to carry the TENANT ID as its label value.
// Prometheus client counters never age out: a label value that appears once is
// exported by that process forever. So a single shared-database blip during a busy
// hour mints a permanent time series per affected tenant — cardinality that grows
// with the customer base, is never reclaimed, and outlives the incident it
// described. It is also the wrong instrument for the question: an alert wants to
// know THAT evidence is at risk; a log wants to know WHICH tenant, and logs expire.
//
// The label is now `outcome`, whose domain is closed and has exactly two members.
func TestAuditWriteErrors_IsNeverLabelledByTenant(t *testing.T) {
	auditWriteErrors.WithLabelValues(OutcomeRowLost).Inc()

	var m dto.Metric
	if err := auditWriteErrors.WithLabelValues(OutcomeRowLost).Write(&m); err != nil {
		t.Fatalf("write metric: %v", err)
	}
	if len(m.Label) != 1 {
		t.Fatalf("label count = %d, want exactly 1 (outcome)", len(m.Label))
	}
	if got := m.Label[0].GetName(); got != "outcome" {
		t.Errorf("label name = %q, want %q — a per-tenant label is unbounded cardinality", got, "outcome")
	}
	if got := m.Label[0].GetValue(); got != OutcomeRowLost {
		t.Errorf("label value = %q, want %q", got, OutcomeRowLost)
	}
}

// TestOutcomeLabel_DistinguishesTheTwoOPPOSITEIncidents pins the reason the label
// exists at all — and the reason the runbook can tell on-call what to do.
//
// The two failures look identical from a single "audit write failed" counter, and
// they demand OPPOSITE responses:
//
//   - row_lost:          the mutation COMMITTED, its row did not. Evidence is gone,
//     permanently, and nothing retries it. Compliance incident.
//   - mutation_refused:  the write failed INSIDE the business tx, so ADR-090's
//     shared fate rolled the change back with it. Nothing is
//     missing. The customer saw an error. Availability incident.
//
// Collapsing them would page someone to hunt for lost evidence that was never lost.
func TestOutcomeLabel_DistinguishesTheTwoOPPOSITEIncidents(t *testing.T) {
	if OutcomeRowLost == OutcomeMutationRefused {
		t.Fatal("the two outcomes must be distinguishable")
	}
	for _, o := range []string{OutcomeRowLost, OutcomeMutationRefused} {
		var m dto.Metric
		if err := auditWriteErrors.WithLabelValues(o).Write(&m); err != nil {
			t.Fatalf("outcome %q is not a usable label value: %v", o, err)
		}
	}
}

var withLabelValuesCall = regexp.MustCompile(`auditWriteErrors\.WithLabelValues\(([^)]*)\)`)

// TestEveryFailureSiteLabelsItsOutcome is a SOURCE gate, and it is the one that
// actually holds.
//
// The runtime tests above only see the sites a test happens to drive. The real
// regression is a new (or a missed) failure path passing something else — and this
// is not hypothetical: the tenant label survived at two of the three own-tx failure
// sites (insert, commit) after the first was fixed, silently re-labelling them
// outcome="<tenant-id>" and reintroducing the exact unbounded cardinality the fix
// was for. A per-site source check is what would have caught it.
func TestEveryFailureSiteLabelsItsOutcome(t *testing.T) {
	src, err := os.ReadFile("audit.go")
	if err != nil {
		t.Fatalf("read audit.go: %v", err)
	}

	allowed := map[string]bool{"OutcomeRowLost": true, "OutcomeMutationRefused": true}

	matches := withLabelValuesCall.FindAllStringSubmatch(stripComments(string(src)), -1)
	if len(matches) == 0 {
		t.Fatal("no auditWriteErrors.WithLabelValues call found — this gate is dead, and a dead gate is worse than none")
	}
	for _, m := range matches {
		arg := strings.TrimSpace(m[1])
		if !allowed[arg] {
			t.Errorf(`auditWriteErrors.WithLabelValues(%s): the label value must be one of the
closed Outcome* constants, not a runtime string.

Passing a tenant id (or any open-domain value) here mints a Prometheus time series
per distinct value, forever — client counters never age out. Use OutcomeRowLost (the
mutation committed, the row did not) or OutcomeMutationRefused (the in-tx write failed
and took the mutation down with it). The tenant belongs in the error log next to it.`, arg)
		}
	}
}

// stripComments keeps a call quoted in a comment — of which this file and audit.go
// have several — from being read as a real one.
func stripComments(src string) string {
	var b strings.Builder
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
