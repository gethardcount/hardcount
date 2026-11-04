package recipe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// PricingWriter is the narrow, transaction-aware part of pricing.Service
// that recipe.Service needs. It is defined here, so the recipe package owns
// no cross-domain state and unit tests can drive it with fakes.
type PricingWriter interface {
	CreateMeterTx(ctx context.Context, tx *sql.Tx, tenantID string, m domain.Meter) (domain.Meter, error)
	// GetMeterByEventNameTx reads a meter the install would adopt, on the
	// install's own transaction and FOR KEY SHARE. So an aggregation PATCH
	// cannot land between the conformance check and the prices that use the
	// meter (ADR-130 §15).
	GetMeterByEventNameTx(ctx context.Context, tx *sql.Tx, eventName string) (domain.Meter, error)
	// CreatePlanWithPricesTx creates a plan and its prices on tx, with the
	// checks POST /v1/plans runs.
	CreatePlanWithPricesTx(ctx context.Context, tx *sql.Tx, tenantID string, p domain.Plan, prices []domain.PlanPrice) (domain.Plan, []domain.PlanPrice, error)
	ListPlans(ctx context.Context, tenantID string) ([]domain.Plan, error)
}

// AuditEmitter is the narrow in-transaction audit seam (ADR-090).
// Instantiate owns the coordinator transaction that spans every
// cross-domain write, so it writes the audit row on that same transaction.
// The installed objects and the record of what was installed then commit
// or roll back together.
type AuditEmitter interface {
	LogInTx(ctx context.Context, tx *sql.Tx, e audit.Entry) error
}

// Service is the orchestrator for the recipes feature. It answers "list
// recipes", "preview" and "instantiate" (idempotent apply). The entities a
// recipe creates (meters, plans and their prices) live in the pricing
// domain's store. This service carries one transaction across those writes
// and the install record, so a recipe is either fully applied or not
// applied at all. A recipe touches only the catalog (ADR-130 §15).
type Service struct {
	db          *postgres.DB
	store       Store
	registry    *Registry
	pricing     PricingWriter
	auditLogger AuditEmitter
}

// NewService wires the recipe service. registry must already be loaded
// with Load(). The constructor does no I/O.
func NewService(
	db *postgres.DB,
	store Store,
	registry *Registry,
	pricing PricingWriter,
) *Service {
	return &Service{
		db:       db,
		store:    store,
		registry: registry,
		pricing:  pricing,
	}
}

// SetAuditLogger wires the in-transaction audit row for recipe apply
// (ADR-090). A nil emitter skips the row, so unit tests can drive the
// service with fakes. The composition root's audit.MustWired check turns a
// forgotten wiring line into a panic at boot, instead of installs that
// write no audit row.
func (s *Service) SetAuditLogger(a AuditEmitter) { s.auditLogger = a }

// RecipeListItem is one entry in the GET /v1/recipes response. It holds
// the canonical recipe metadata, the tenant's install state, and a summary
// of what the recipe creates. With the summary, the picker UI can show "1
// meter · 9 prices · monthly billing" without an extra preview call. The
// dashboard uses Instantiated to show an "Installed" badge in place of the
// "Install" button, because re-apply is an idempotent no-op and there is
// no uninstall (ADR-085).
type RecipeListItem struct {
	domain.Recipe
	Creates      RecipeCreates          `json:"creates"`
	Instantiated *domain.RecipeInstance `json:"instantiated,omitempty"`
}

// RecipeCreates counts, per role, the objects a recipe creates when it is
// installed. List and detail responses carry it, so the dashboard can show
// summary chips without calling preview. The counts follow the shape of
// domain.CreatedObjects, which lists IDs, reduced to integers for display.
type RecipeCreates struct {
	Meters int `json:"meters"`
	Plans  int `json:"plans"`
	Prices int `json:"prices"`
}

// countCreates derives a RecipeCreates summary from a parsed recipe.
func countCreates(r domain.Recipe) RecipeCreates {
	c := RecipeCreates{Meters: len(r.Meters), Plans: len(r.Plans)}
	for _, p := range r.Plans {
		c.Prices += len(p.Prices)
	}
	return c
}

// ListRecipes returns every recipe in the registry, tagged with this
// tenant's install state. Each one is rendered with its declared defaults,
// so no raw template syntax ("{{ .currency }}") leaks into the response. It
// makes one indexed read per recipe. That is fine at the v1 catalog size
// (3 recipes). Revisit it if the catalog grows past ~50.
func (s *Service) ListRecipes(ctx context.Context, tenantID string) ([]RecipeListItem, error) {
	recipes := s.registry.List()
	out := make([]RecipeListItem, 0, len(recipes))
	for _, r := range recipes {
		display, err := displayRecipe(r)
		if err != nil {
			return nil, fmt.Errorf("recipe %q: render defaults: %w", r.Key, err)
		}
		item := RecipeListItem{Recipe: display, Creates: countCreates(r)}
		inst, err := s.store.GetByKey(ctx, tenantID, r.Key)
		switch {
		case err == nil:
			instCopy := inst
			item.Instantiated = &instCopy
		case errors.Is(err, errs.ErrNotFound):
			// Not installed for this tenant: leave Instantiated nil.
		default:
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// RecipeDetail is the GET /v1/recipes/{key} response. It has the same
// fields as a list item, plus the long description and the full
// overridable_schema, which are already on the embedded domain.Recipe. It
// wraps domain.Recipe instead of returning it bare, so it can carry the
// creates summary. The dashboard's picker drawer then shows chips without
// a preview call.
type RecipeDetail struct {
	domain.Recipe
	Creates RecipeCreates `json:"creates"`
}

// GetRecipe returns a recipe rendered with its defaults, so the dashboard
// can fill the override form. It reads only the registry, with no
// database I/O.
func (s *Service) GetRecipe(key string) (RecipeDetail, error) {
	r, ok := s.registry.Get(key)
	if !ok {
		return RecipeDetail{}, errs.ErrNotFound
	}
	display, err := displayRecipe(r)
	if err != nil {
		return RecipeDetail{}, fmt.Errorf("recipe %q: render defaults: %w", key, err)
	}
	return RecipeDetail{Recipe: display, Creates: countCreates(r)}, nil
}

// displayRecipe renders a recipe with its declared defaults, for the
// read-only views (list and detail). It keeps the overridable schema, which
// renderRecipe strips because overrides are an input, so the dashboard's
// override form still has it. Instantiate and Preview never use it: they
// render with the caller's actual overrides.
func displayRecipe(r domain.Recipe) (domain.Recipe, error) {
	rendered, err := renderRecipe(r, nil)
	if err != nil {
		return domain.Recipe{}, err
	}
	rendered.Overridable = r.Overridable
	return rendered, nil
}

// ListInstances returns the recipe_instances rows for the tenant.
func (s *Service) ListInstances(ctx context.Context, tenantID string) ([]domain.RecipeInstance, error) {
	return s.store.List(ctx, tenantID)
}

// PreviewResult is the wire shape of POST /v1/recipes/{key}/preview.
// `objects` lists what an install creates: the meters, and each plan with
// its prices. `warnings` is a slot for non-fatal conditions. It is always
// empty today, and it stays so the contract is stable when a warning is
// added.
type PreviewResult struct {
	Key      string         `json:"key"`
	Version  string         `json:"version"`
	Objects  PreviewObjects `json:"objects"`
	Warnings []string       `json:"warnings"`
}

// PreviewObjects is what an install creates. Empty arrays are written (no
// `omitempty`), so the picker UI loops over them without null checks. An
// install reuses a meter that already exists with the same event_name and
// aggregation, and the preview lists it either way.
type PreviewObjects struct {
	Meters []domain.RecipeMeter `json:"meters"`
	Plans  []domain.RecipePlan  `json:"plans"`
}

// Preview renders a recipe with the caller's overrides, and returns the
// objects an install would create. It runs in memory, with no database
// writes and no transactions. It is cheap enough to call on every
// keystroke in the override form.
func (s *Service) Preview(_ context.Context, recipeKey string, overrides map[string]any) (PreviewResult, error) {
	r, ok := s.registry.Get(recipeKey)
	if !ok {
		return PreviewResult{}, errs.ErrNotFound
	}
	rendered, err := renderRecipe(r, overrides)
	if err != nil {
		return PreviewResult{}, errs.Invalid("overrides", err.Error())
	}
	return previewResultFrom(rendered), nil
}

// previewResultFrom converts a rendered domain.Recipe into the public
// preview wire shape. Slices default to non-nil, so the JSON encoder writes
// `[]` rather than `null`, and the picker UI maps them without checks.
func previewResultFrom(r domain.Recipe) PreviewResult {
	out := PreviewResult{
		Key:      r.Key,
		Version:  r.Version,
		Objects:  PreviewObjects{Meters: r.Meters, Plans: make([]domain.RecipePlan, 0, len(r.Plans))},
		Warnings: []string{},
	}
	if out.Objects.Meters == nil {
		out.Objects.Meters = []domain.RecipeMeter{}
	}
	for _, p := range r.Plans {
		if p.Prices == nil {
			p.Prices = []domain.RecipePrice{}
		}
		out.Objects.Plans = append(out.Objects.Plans, p)
	}
	return out
}

// InstantiateOptions holds the one-off options for Instantiate. CreatedBy
// is the actor (operator email or API key ID) recorded on
// recipe_instances.created_by for audit.
type InstantiateOptions struct {
	CreatedBy string
}

// Instantiate applies a recipe for tenantID in one transaction, in this
// order: meters, each plan with its prices, the instance row, then the
// audit row. Any failure rolls back all of it, so partial state never
// reaches the tenant. It writes only the catalog (ADR-130 §15). Apply is an
// idempotent event (ADR-085). A second call on an installed recipe is a
// no-op that returns the existing instance. It never answers 409, and
// never creates a second plan.
func (s *Service) Instantiate(
	ctx context.Context,
	tenantID, recipeKey string,
	overrides map[string]any,
	opts InstantiateOptions,
) (domain.RecipeInstance, error) {
	r, ok := s.registry.Get(recipeKey)
	if !ok {
		return domain.RecipeInstance{}, errs.ErrNotFound
	}

	rendered, err := renderRecipe(r, overrides)
	if err != nil {
		return domain.RecipeInstance{}, errs.Invalid("overrides", err.Error())
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.RecipeInstance{}, err
	}
	defer postgres.Rollback(tx)

	// Idempotency: a recipe is an instantiation event, applied once. If the
	// badge (the install record) exists, apply is a no-op that returns the
	// existing instance unchanged. It writes no audit row, on purpose.
	// Nothing was installed on this call, so a row would record a change
	// that never happened, the fabricated record ADR-090 rules out. The
	// first apply's row is the true record.
	//
	// Everything the badge recorded is still present. Plans are ON DELETE
	// RESTRICT with no hard delete, and meters are shared reference data.
	// So there is nothing to create again, and a double submit never makes
	// a second plan: the badge is the idempotency gate. The badge is scoped
	// to the mode, like the objects it records. RLS partitions this read by
	// app.livemode, so a test-mode install never gates a live-mode one.
	// Additive re-apply of a newer template version is deferred (ADR-085).
	// v1 ships one version per recipe, so a re-apply at the same version is
	// a pure no-op.
	if existing, err := s.store.GetByKeyTx(ctx, tx, tenantID, recipeKey); err == nil {
		// Declare the no-op to the audit-coverage detector. This request is a
		// mutating POST that answers 201 having changed nothing. Without the
		// declaration, an observer at the transport cannot tell it from an
		// install that forgot its audit row. Every re-apply would then be
		// reported as an uncovered mutation.
		audit.MarkSkip(ctx)
		return existing, nil
	} else if !errors.Is(err, errs.ErrNotFound) {
		return domain.RecipeInstance{}, err
	}

	objs := domain.CreatedObjects{}

	// Meters: adopt an existing event name only if its aggregation matches,
	// and refuse loudly otherwise. Aggregation is the only field billing
	// reads, and sum against max, count or last would roll usage up wrongly
	// without a word. The read runs on the install's transaction FOR KEY
	// SHARE, so an aggregation PATCH waits for the install. Adoption never
	// changes the meter. A meter only counts, and the prices go on the new
	// plan (ADR-130 §15), so adopting a meter that other plans price changes
	// none of their prices.
	meterIDByKey := make(map[string]string, len(rendered.Meters))
	for _, m := range rendered.Meters {
		if existing, err := s.pricing.GetMeterByEventNameTx(ctx, tx, m.EventName); err == nil {
			if diffs := meterConformanceDiff(existing, m); len(diffs) > 0 {
				return domain.RecipeInstance{}, errs.AlreadyExists("meter", meterConflictMessage(m.EventName, diffs))
			}
			meterIDByKey[m.EventName] = existing.ID
			objs.MeterIDs = append(objs.MeterIDs, existing.ID)
			continue
		} else if !errors.Is(err, errs.ErrNotFound) {
			return domain.RecipeInstance{}, fmt.Errorf("meter %q: adoption check: %w", m.EventName, err)
		}
		created, err := s.pricing.CreateMeterTx(ctx, tx, tenantID, domain.Meter{
			EventName:   m.EventName,
			Name:        m.Name,
			Unit:        m.Unit,
			Aggregation: m.Aggregation,
		})
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("meter %q: %w", m.EventName, err)
		}
		meterIDByKey[m.EventName] = created.ID
		objs.MeterIDs = append(objs.MeterIDs, created.ID)
	}

	// Plan: create a fresh plan, and never adopt an existing plan by code
	// (ADR-085). The plan is created with its prices, so it cannot bill $0
	// because a price was left unwired. There is also no question of
	// whether an existing plan matches. The operator owns the plan once it
	// exists, and the recipe never changes or reuses one. On a code
	// collision, with an earlier recipe's plan or another plan, the new plan
	// gets a unique code (`_2`, `_3`, …). The existing plan is never
	// renamed. The plan's currency is the recipe's, and its prices take it.
	existingPlans, err := s.pricing.ListPlans(ctx, tenantID)
	if err != nil {
		return domain.RecipeInstance{}, fmt.Errorf("list plans: %w", err)
	}
	takenCodes := make(map[string]bool, len(existingPlans))
	for _, ep := range existingPlans {
		takenCodes[ep.Code] = true
	}
	for _, p := range rendered.Plans {
		planPrices := make([]domain.PlanPrice, 0, len(p.Prices))
		for _, rp := range p.Prices {
			planPrices = append(planPrices, domain.PlanPrice{
				MeterID:        meterIDByKey[rp.Meter],
				DimensionMatch: rp.DimensionMatch,
				PriceShape:     rp.PriceShape,
			})
		}
		code := freePlanCode(p.Code, takenCodes)
		takenCodes[code] = true
		created, createdPrices, err := s.pricing.CreatePlanWithPricesTx(ctx, tx, tenantID, domain.Plan{
			Code:            code,
			Name:            p.Name,
			Currency:        p.Currency,
			BillingInterval: p.BillingInterval,
			Status:          domain.PlanActive,
			BaseAmountCents: p.BaseAmountCents,
			BaseBillTiming:  p.BaseBillTiming,
		}, planPrices)
		if err != nil {
			return domain.RecipeInstance{}, fmt.Errorf("plan %q: %w", code, err)
		}
		objs.PlanIDs = append(objs.PlanIDs, created.ID)
		for _, cp := range createdPrices {
			objs.PriceIDs = append(objs.PriceIDs, cp.ID)
		}
	}

	inst, err := s.store.CreateTx(ctx, tx, domain.RecipeInstance{
		TenantID:       tenantID,
		RecipeKey:      recipeKey,
		RecipeVersion:  rendered.Version,
		Overrides:      copyOverrides(overrides),
		CreatedObjects: objs,
		CreatedBy:      opts.CreatedBy,
	})
	if err != nil {
		return domain.RecipeInstance{}, err
	}

	// The audit row goes on the coordinator transaction (ADR-090), so the
	// installed objects and the record of what was installed commit or roll
	// back together. Only the path that installed something writes it. The
	// idempotent no-op above returns before here.
	if err := s.emitInstantiated(ctx, tx, rendered, inst); err != nil {
		return domain.RecipeInstance{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.RecipeInstance{}, err
	}
	return inst, nil
}

// emitInstantiated writes the recipe-apply audit row on the coordinator
// transaction.
//
// The wire strings are frozen vocabulary: action "create" (an apply creates
// an install) and resource_type "recipe". resource_id is the recipe key,
// not the instance id. The operator surface addresses the key (GET
// /v1/recipes/{key}, the dashboard's install badge), so it is the id that
// leads to something a person can open. The instance id goes in metadata.
//
// Metadata answers "what did this recipe install?", which nothing else can
// answer once the operator starts editing the objects. It mirrors the
// instance row's created-object ids. Those ids include adopted meters
// (ADR-085), because apply reconnects to an existing meter instead of
// making a copy. So metadata holds the objects this apply wired, which is
// the question an auditor asks.
func (s *Service) emitInstantiated(ctx context.Context, tx *sql.Tx, rendered domain.Recipe, inst domain.RecipeInstance) error {
	if s.auditLogger == nil {
		return nil
	}
	objs := inst.CreatedObjects
	meta := map[string]any{
		"instance_id":    inst.ID,
		"recipe_key":     inst.RecipeKey,
		"recipe_version": inst.RecipeVersion,
	}
	if len(objs.PlanIDs) > 0 {
		meta["plan_ids"] = objs.PlanIDs
	}
	if len(objs.MeterIDs) > 0 {
		meta["meter_ids"] = objs.MeterIDs
	}
	if len(objs.PriceIDs) > 0 {
		meta["price_ids"] = objs.PriceIDs
	}
	if err := s.auditLogger.LogInTx(ctx, tx, audit.Entry{
		Action:        domain.AuditActionCreate,
		ResourceType:  "recipe",
		ResourceID:    inst.RecipeKey,
		ResourceLabel: rendered.Name,
		Metadata:      meta,
	}); err != nil {
		return fmt.Errorf("audit emission: %w", err)
	}
	return nil
}

// copyOverrides returns a defensive shallow copy of the caller's overrides
// map, so the recipe_instances row cannot be changed through the caller's
// reference after it is saved.
func copyOverrides(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

// freePlanCode returns `desired` if no plan holds it, else the first free
// `<desired>_N` (N≥2). A recipe never adopts an existing plan by code. So a
// collision, with an earlier recipe's plan or another plan, is avoided by
// giving the new plan a unique code, never by renaming the existing plan
// (the Kubernetes generateName pattern). The plan's immutable id is its
// identity. The code is a display slug that no logic reads: subscriptions
// bind by plan id. A concurrent writer that takes the chosen code between
// the ListPlans read and the INSERT trips the UNIQUE(tenant_id, code)
// constraint, and the whole transaction rolls back. That is a loud retry,
// never a silent duplicate.
func freePlanCode(desired string, taken map[string]bool) string {
	if !taken[desired] {
		return desired
	}
	for n := 2; ; n++ {
		c := fmt.Sprintf("%s_%d", desired, n)
		if !taken[c] {
			return c
		}
	}
}
