package credit

import (
	"context"
	"database/sql"
)

// SetBeforeRestoreHook installs the concurrent-resolver seam of
// ReturnCreditTx (ADR-132): f runs on the return's tx after the source blocks
// are read under the ledger locks and before the first restore write. Tests
// only.
func SetBeforeRestoreHook(s *PostgresStore, f func(ctx context.Context, tx *sql.Tx)) {
	s.beforeRestore = f
}
