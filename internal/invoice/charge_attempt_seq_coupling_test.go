package invoice

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestChargeAttemptSeqBumpedByEveryPIStamp mechanises the invariant that makes
// the Stripe charge idempotency key correct: charge_attempt_seq must move
// exactly when a charge attempt outcome is recorded.
//
// "An outcome was recorded" has one syntactic tell in this store — the UPDATE
// stamps stripe_payment_intent_id. So the rule is checkable by construction:
// every UPDATE on invoices that sets stripe_payment_intent_id must bump
// charge_attempt_seq in the SAME statement.
//
// Why a source-scanning test and not a convention: this is a class-C2 hazard
// (playbook §1) — the invariant is spread across THREE writers today
// (StampChargeOutcome, MarkPaymentFailedReportingTransition,
// markPaidReportingTransition), and a fourth added later would fail silently in
// the worst direction. Missing the bump means a RECORDED decline reuses its
// key, Stripe hands back the declined PaymentIntent forever, and the invoice
// can never be paid — a liveness sink (class G) with no error anywhere.
//
// The reverse direction is checked too: a bump without a PI stamp means some
// unrelated write is re-seeding the key, which is the exact drift this column
// was introduced to end (#677 → #678).
//
// One write moves the seq with no PaymentIntent: the interrupted-charge
// check's release (ADR-127; ADR-105 amendment 2026-10-19). Stripe was shown to
// hold nothing for the charge record's key, so "no payment was created" is the
// attempt's recorded outcome, and the next charge needs a fresh key: Stripe
// may have saved a 500 under the old one. The release is the statement that
// also clears the record (charge_started_at = NULL); the scanner admits that
// statement and requires there to be exactly one.
//
// The scan covers every non-test .go file of this package, not one file: a
// writer added in a new file would otherwise escape it.
func TestChargeAttemptSeqBumpedByEveryPIStamp(t *testing.T) {
	const (
		piStamp      = "stripe_payment_intent_id ="
		seqBump      = "charge_attempt_seq = charge_attempt_seq + 1"
		recordClears = "charge_started_at = NULL"
	)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var src strings.Builder
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		src.Write(b)
	}

	// Each UPDATE ... on the invoices table, up to its terminating clause.
	updateStmt := regexp.MustCompile(`(?s)UPDATE invoices.*?(?:WHERE|RETURNING)`)
	stmts := updateStmt.FindAllString(src.String(), -1)
	if len(stmts) == 0 {
		t.Fatal("found no UPDATE invoices statements — the scanner is broken, not the code")
	}

	var stamps, bumps, releases int
	for _, stmt := range stmts {
		// A predicate reference (WHERE stripe_payment_intent_id = $1) is a read,
		// not a stamp; the regex stops before WHERE, so anything left is a SET.
		hasStamp := strings.Contains(stmt, piStamp)
		hasBump := strings.Contains(stmt, seqBump)
		isRelease := hasBump && !hasStamp && strings.Contains(stmt, recordClears)
		switch {
		case hasStamp && !hasBump:
			t.Errorf("an UPDATE stamps stripe_payment_intent_id without bumping charge_attempt_seq — "+
				"a recorded decline would reuse its idempotency key and Stripe would return the DECLINED "+
				"PaymentIntent forever, so the invoice could never be paid:\n%s", stmt)
		case hasBump && !hasStamp && !isRelease:
			t.Errorf("an UPDATE bumps charge_attempt_seq without stamping stripe_payment_intent_id — "+
				"only a recorded attempt outcome may re-seed the charge idempotency key; re-seeding it "+
				"from an unrelated write is the drift #678 removed:\n%s", stmt)
		}
		if hasStamp {
			stamps++
		}
		if hasBump {
			bumps++
		}
		if isRelease {
			releases++
		}
	}

	// Guard against the scanner silently matching nothing after a refactor:
	// the known outcome writers must all be seen.
	if stamps < 3 {
		t.Errorf("expected at least 3 PI-stamping UPDATEs (StampChargeOutcome, MarkPaymentFailedReportingTransition, "+
			"markPaidReportingTransition), found %d — did the scanner stop matching?", stamps)
	}
	if releases != 1 {
		t.Errorf("found %d interrupted-charge releases (an UPDATE that moves the seq and clears the charge record), want exactly 1: "+
			"ReleaseInterruptedCharge is the one write ADR-105's amendment lets move the seq without a PaymentIntent", releases)
	}
	if bumps != stamps+releases {
		t.Errorf("seq bumps (%d) are not PI stamps (%d) plus the release (%d)", bumps, stamps, releases)
	}
}
