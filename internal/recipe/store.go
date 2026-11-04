package recipe

import (
	"context"
	"database/sql"

	"github.com/gethardcount/hardcount/internal/domain"
)

// Store is the persistence boundary for recipe_instances. The pricing
// domain's store owns the cross-domain entities a recipe creates (meters,
// plans and their prices). This Store only tracks the thin index of
// installs.
//
// Methods come in two families:
//
//   - Plain methods (GetByKey, List) run their own short transaction.
//   - The *Tx variants take an existing *sql.Tx, so Service.Instantiate can
//     carry one transaction across several cross-domain calls. All the
//     objects then commit together, or none do.
type Store interface {
	// GetByKey returns the existing instance for (tenant, recipe_key), or
	// errs.ErrNotFound. GET /v1/recipes uses it to fill the `instantiated`
	// field of each list entry.
	GetByKey(ctx context.Context, tenantID, recipeKey string) (domain.RecipeInstance, error)

	// List returns all instances for the tenant, newest first. It backs the
	// dashboard's "recipes installed" view.
	List(ctx context.Context, tenantID string) ([]domain.RecipeInstance, error)

	// GetByKeyTx is GetByKey on an existing transaction. It returns (zero,
	// errs.ErrNotFound) if no instance exists. Instantiate uses it as the
	// idempotency gate: when the badge (the install record) is present,
	// apply is a no-op.
	GetByKeyTx(ctx context.Context, tx *sql.Tx, tenantID, recipeKey string) (domain.RecipeInstance, error)

	// CreateTx inserts a new instance row inside the caller's transaction.
	// The caller commits or rolls back.
	CreateTx(ctx context.Context, tx *sql.Tx, inst domain.RecipeInstance) (domain.RecipeInstance, error)
}
