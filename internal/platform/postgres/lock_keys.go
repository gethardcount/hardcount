package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// Transaction-scoped advisory locks. Each is taken with pg_advisory_xact_lock
// and held until its transaction commits or rolls back, so a transaction-mode
// pooler cannot leak one. Singleton-role leadership is a leader_leases row
// (ADR-114), not an advisory lock.
//
// Two kinds of key are in use:
//
//   - LockKeyBootstrap, a fixed number, serializes first-boot tenant
//     bootstrap.
//   - Per-row keys are text hashed into the 64-bit key space with
//     hashtextextended(key, 0). The payment-method attach lock
//     ("payment_methods:tenant:customer", internal/paymentmethods) and the
//     customer-rules lock ("customer_rules:tenant:customer",
//     LockCustomerRulesTx, below) name their lock in a prefix. The credit
//     ledger ("tenant:customer", internal/credit) and the credit-note invoice
//     lock ("tenant:invoice", internal/creditnote) are bare "tenant:id"
//     pairs. Those two cannot be equal, because customer and invoice ids
//     differ (hc_cus_ and hc_inv_). A new key takes its own prefix, so it
//     cannot equal any of these. Two different keys share a lock only
//     through a hash collision, which adds a wait and nothing else.
//
// Migrations take goose's own session lock (id 4097083626, ADR-125) on a
// dedicated one-connection pool. That key is goose's, so it is not listed
// here.
//
// The retired keys are the per-role session keys 76540001-76540005, the
// topology-probe key 76540008 and the hybrid migration loop key 76540007.
// Do not reuse their numbers. An old binary in a mixed fleet still takes the
// session and topology-probe keys for the duration of its tick, and v0.8.x
// migration runs take 76540007.
const (
	LockKeyBootstrap int64 = 76540006
)

// LockCustomerRulesTx takes the customer-rules lock for one customer on tx and
// holds it until tx ends. Every customer-scoped writer of a rule that spans
// a customer's subscriptions takes it: one live subscription item per meter,
// one currency, no new business on an archived customer, and a customer
// update read and written as one step. Those are subscription creates, item
// changes, draft activations, customer updates and billing-profile writes.
// Each one reads the state its rule checks after it gets the lock, so it sees
// every earlier holder's commit. A price write is plan-scoped and takes no
// customer lock, so a price added to a plan the customer already uses is not
// ordered with these checks (ADR-130 Risks).
//
// A taker takes it before plans, invoices, credit notes, the tenant's number
// row, the credit ledger lock and the payment-method lock, and holds no
// subscription row but its own (ADR-130 §8). It conflicts only with itself:
// a bounce mark, usage ingest and a foreign-key check on the customer row do
// not wait for it.
func LockCustomerRulesTx(ctx context.Context, tx *sql.Tx, tenantID, customerID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"customer_rules:"+tenantID+":"+customerID); err != nil {
		return fmt.Errorf("acquire customer rules lock: %w", err)
	}
	return nil
}
