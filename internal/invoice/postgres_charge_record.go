package invoice

import (
	"context"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// The charge record (ADR-127). Scaffolding: the bodies land with the fix.

func (s *PostgresStore) BeginChargeAttempt(ctx context.Context, tenantID, id string, seq int64, key, stripeCustomerID string) (bool, error) {
	return false, nil
}

func (s *PostgresStore) EndChargeAttempt(ctx context.Context, tenantID, id string, seq int64) (bool, error) {
	return false, nil
}

func (s *PostgresStore) ListInterruptedCharges(ctx context.Context, minAge, giveUpAge time.Duration, limit int) ([]domain.InterruptedCharge, error) {
	return nil, nil
}

func (s *PostgresStore) ReleaseInterruptedCharge(ctx context.Context, tenantID, id string, seq int64, startedAt time.Time) (released, seqMoved bool, err error) {
	return false, false, nil
}

func (s *PostgresStore) ParkInterruptedCharge(ctx context.Context, tenantID, id string, seq int64, startedAt time.Time, message string) (parked, cleared bool, err error) {
	return false, false, nil
}
