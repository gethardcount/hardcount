package dunning

import (
	"context"
	"database/sql"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// CreateRunTx exposes createRunTx to the race tests in dunning_test: a test
// holds a run insert open on its own transaction to stand in for the
// payment-retry start racing another write.
func (s *PostgresStore) CreateRunTx(ctx context.Context, tx *sql.Tx, tenantID string, run domain.InvoiceDunningRun) (domain.InvoiceDunningRun, error) {
	return s.createRunTx(ctx, tx, tenantID, run)
}
