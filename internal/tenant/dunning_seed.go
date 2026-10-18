package tenant

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
)

// DunningSeedFunc writes the dunning policies a new account starts with:
// one default per mode, on the account-creation tx (ADR-123). The tenant
// package may not import its peer dunning, so the composition root wires
// dunning.Service.SeedDefaultPoliciesTx in. onSeeded runs after each
// insert, with app.livemode still set to that insert's mode.
type DunningSeedFunc func(ctx context.Context, tx *sql.Tx, tenantID string, onSeeded func(p domain.DunningPolicy, livemode bool) error) error

// seedDunningPolicies runs seed on tx and, when an audit emitter is wired,
// records one create/dunning_policy row per mode in that mode's log. A
// failure from either aborts account creation: an account without its
// policies would not retry a failed payment, which is the gap #1152 closes.
func seedDunningPolicies(ctx context.Context, tx *sql.Tx, seed DunningSeedFunc, a AuditEmitter, tenantID, auditAction string) error {
	if seed == nil {
		return fmt.Errorf("dunning seed not wired")
	}
	var onSeeded func(domain.DunningPolicy, bool) error
	if a != nil {
		onSeeded = func(p domain.DunningPolicy, livemode bool) error {
			if err := a.LogInTx(ctx, tx, audit.Entry{
				Action:        domain.AuditActionCreate,
				ResourceType:  "dunning_policy",
				ResourceID:    p.ID,
				ResourceLabel: p.Name,
				Metadata: map[string]any{
					"action":   auditAction,
					"livemode": livemode,
				},
			}); err != nil {
				return fmt.Errorf("audit seeded dunning policy: %w", err)
			}
			return nil
		}
	}
	if err := seed(ctx, tx, tenantID, onSeeded); err != nil {
		return fmt.Errorf("seed dunning policies: %w", err)
	}
	return nil
}
