package invoice

import (
	"context"
	"errors"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// Failing-first skeleton for #979: the signatures the tests call, with no
// behavior. The next commit implements them.
var errDisputesNotImplemented = errors.New("disputes: not implemented")

func (s *PostgresStore) RecordStripeDispute(context.Context, string, domain.DisputeSnapshot) (domain.DisputeRecordResult, error) {
	return domain.DisputeRecordResult{}, errDisputesNotImplemented
}

func (s *PostgresStore) ListDisputesByInvoice(context.Context, string, string) ([]domain.InvoiceDispute, error) {
	return nil, errDisputesNotImplemented
}

func (s *PostgresStore) LostDisputesByInvoices(context.Context, string, []string) (map[string]domain.InvoiceDispute, error) {
	return nil, errDisputesNotImplemented
}
