package domain

import (
	"strings"
	"time"
)

// MaxBillingAttempts is how many counted failures one piece of due work gets
// before the billing run stops attempting it. The eighth failure parks the
// subscription. The number lives only in Go: no SQL contains it, so changing
// it never re-classifies rows already saved (ADR-119).
const MaxBillingAttempts = 8

// BillingFailure is the saved record of a subscription whose billing keeps
// failing (ADR-119). It is present on a read only while it describes the work
// that is due right now; see SubscriptionBillingFailingSQL.
type BillingFailure struct {
	// Attempts counts the failed attempts on the current due work. A record
	// that exists always has at least 1.
	Attempts    int `json:"attempts"`
	MaxAttempts int `json:"max_attempts"`
	// LastError is a curated sentence or a generic reference to the
	// subscription id. It is never raw error text.
	LastError string    `json:"last_error"`
	FailedAt  time.Time `json:"failed_at"`
	// NextAttemptAt is the earliest instant the billing run attempts the
	// subscription again. Nil means parked.
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	// Parked: nothing retries the subscription until an operator calls
	// POST /v1/subscriptions/{id}/retry-billing or the due work changes.
	Parked bool `json:"parked"`
}

// DueInstant is the instant the billing run next owes work for sub: the close
// at next_billing_at, or a mid-period cancel at cancel_at, whichever is first.
// It is the Go form of the expression the due scans test,
//
//	LEAST(next_billing_at, CASE WHEN status = 'active' THEN cancel_at END)
//
// and, like LEAST, it ignores a missing side. Nil means no work can come due.
//
// A billing-failure record is keyed to this value. Any statement that makes
// the failing work stop being due moves the due instant in that same
// statement, which voids the record with no clear.
func DueInstant(sub Subscription) *time.Time {
	var due *time.Time
	if sub.NextBillingAt != nil {
		t := *sub.NextBillingAt
		due = &t
	}
	if sub.Status == SubscriptionActive && sub.CancelAt != nil && (due == nil || sub.CancelAt.Before(*due)) {
		t := *sub.CancelAt
		due = &t
	}
	return due
}

// BillingDueAt reports whether sub owes work to the billing run at now: it can
// still bill (active or trialing) and its due instant has been reached. It is
// the Go form of what the due scans admit, before the billing-failure gate.
func BillingDueAt(sub Subscription, now time.Time) bool {
	if sub.Status != SubscriptionActive && sub.Status != SubscriptionTrialing {
		return false
	}
	due := DueInstant(sub)
	return due != nil && !due.After(now)
}

// SubscriptionDueInstantSQL is the SQL form of DueInstant for the
// subscriptions row aliased as alias.
func SubscriptionDueInstantSQL(alias string) string {
	return strings.ReplaceAll(`LEAST(a.next_billing_at, CASE WHEN a.status = 'active' THEN a.cancel_at END)`, "a.", alias+".")
}

// SubscriptionBillingFailingSQL is the one predicate that says a
// subscriptions row (aliased as alias) carries a live billing-failure record:
// a wall-clock subscription that can still bill, whose record was saved for
// the due instant the row has now. The five billing_* columns are not cleared
// when the due instant moves, so reading them without this predicate reads
// stale data. Every reader goes through it: the due scans, the list filter,
// both writers, and the gauges (a hand copy, pinned by a test).
//
// Every term is two-valued (never NULL), so the predicate is safe under NOT.
func SubscriptionBillingFailingSQL(alias string) string {
	return strings.ReplaceAll(
		`a.status IN ('active','trialing') AND a.test_clock_id IS NULL AND a.billing_failed_for IS NOT NULL AND a.billing_failed_for IS NOT DISTINCT FROM `,
		"a.", alias+".") + SubscriptionDueInstantSQL(alias)
}

// SubscriptionBillingGateSQL is the clause the wall-clock due scans add: skip
// a row whose live record is parked (no next attempt) or still waiting.
// nowParam is the placeholder of the run's anchored instant, e.g. "$2".
func SubscriptionBillingGateSQL(alias, nowParam string) string {
	return "NOT (" + SubscriptionBillingFailingSQL(alias) +
		" AND (" + alias + ".billing_next_attempt_at IS NULL OR " + alias + ".billing_next_attempt_at > " + nowParam + "))"
}
