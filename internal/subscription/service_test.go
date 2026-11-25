package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// memStore is an in-memory Store implementation used across the subscription
// package's unit tests. It does not enforce every invariant the postgres store
// does — it covers just the behaviour the service layer depends on (item CRUD,
// atomic transitions, and pending-item bookkeeping).
type memStore struct {
	// clearPauseOverride, when set, is what ResumeLiftedPause returns —
	// simulates a sibling leader having cleared the pause between the list
	// and the clear (HA hazard 5).
	clearPauseOverride error
	subs               map[string]domain.Subscription
	items              map[string]domain.SubscriptionItem
	itemChanges        []domain.SubscriptionItemChange
	// events records each EnqueueEventTx call: the item events the service
	// queues on the change's tx. The store's own writers queue the
	// lifecycle events, which the real-Postgres tests check.
	events []capturedEvent
}

// EnqueueEventTx records the event with the subscription as the memStore
// holds it. WithTenantTx runs no real tx, so a failed change keeps it; the
// rollback is tested on Postgres.
func (m *memStore) EnqueueEventTx(ctx context.Context, _ *sql.Tx, tenantID, subscriptionID, eventType string, extra map[string]any) error {
	sub, err := m.Get(ctx, tenantID, subscriptionID)
	if err != nil {
		return err
	}
	m.events = append(m.events, capturedEvent{tenantID: tenantID, eventType: eventType, payload: domain.SubscriptionWebhookData(sub, extra)})
	return nil
}

// firstEvent returns the first recorded event of eventType.
func (m *memStore) firstEvent(eventType string) (capturedEvent, bool) {
	for _, e := range m.events {
		if e.eventType == eventType {
			return e, true
		}
	}
	return capturedEvent{}, false
}

func newMemStore() *memStore {
	return &memStore{
		subs:  make(map[string]domain.Subscription),
		items: make(map[string]domain.SubscriptionItem),
	}
}

func (m *memStore) create(ctx context.Context, tenantID string, s domain.Subscription) (domain.Subscription, error) {
	for _, existing := range m.subs {
		if existing.TenantID == tenantID && existing.Code == s.Code {
			return domain.Subscription{}, fmt.Errorf("%w: subscription code %q", errs.ErrAlreadyExists, s.Code)
		}
	}
	s.ID = fmt.Sprintf("hc_sub_%d", len(m.subs)+1)
	s.TenantID = tenantID
	now := clock.Now(ctx)
	s.CreatedAt = now
	s.UpdatedAt = now

	hydrated := make([]domain.SubscriptionItem, 0, len(s.Items))
	for i, it := range s.Items {
		it.ID = fmt.Sprintf("%s_item_%d", s.ID, i+1)
		it.SubscriptionID = s.ID
		it.TenantID = tenantID
		it.CreatedAt = now
		it.UpdatedAt = now
		m.items[it.ID] = it
		hydrated = append(hydrated, it)
		m.recordChange(it, "add", "", it.PlanID, 0, it.Quantity, now)
	}
	s.Items = hydrated
	m.subs[s.ID] = s
	return s, nil
}

func (m *memStore) CreateWithBill(ctx context.Context, tenantID string, s domain.Subscription, billFn func(tx *sql.Tx, created domain.Subscription) error) (domain.Subscription, error) {
	created, err := m.create(ctx, tenantID, s)
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(nil, created); err != nil {
			// Simulate the tx rollback: the real store does not persist the
			// subscription (or its items) when billFn fails.
			delete(m.subs, created.ID)
			for _, it := range created.Items {
				delete(m.items, it.ID)
			}
			return domain.Subscription{}, err
		}
	}
	return created, nil
}

// recordChange mirrors the DB trigger that fills
// subscription_item_changes (migration 0029). Each mutation through
// the mem store appends a change row so tests that exercise
// segment-aware billing see the same audit log shape as production.
func (m *memStore) recordChange(it domain.SubscriptionItem, changeType, fromPlanID, toPlanID string, fromQty, toQty int64, at time.Time) {
	m.itemChanges = append(m.itemChanges, domain.SubscriptionItemChange{
		ID:                 fmt.Sprintf("hc_sic_%d", len(m.itemChanges)+1),
		TenantID:           it.TenantID,
		SubscriptionID:     it.SubscriptionID,
		SubscriptionItemID: it.ID,
		ChangeType:         changeType,
		FromPlanID:         fromPlanID,
		ToPlanID:           toPlanID,
		FromQuantity:       fromQty,
		ToQuantity:         toQty,
		ChangedAt:          at,
		CreatedAt:          at,
	})
}

func (m *memStore) Get(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	s.Items = m.hydrateItems(s.ID)
	return s, nil
}

// ClearBillingFailure mirrors the store's contract on the in-memory row: a
// sub with no record is InvalidState(billing_not_failing) and inTx is not run.
func (m *memStore) ClearBillingFailure(_ context.Context, tenantID, id string, inTx func(tx *sql.Tx, cleared domain.BillingFailure) error) (domain.BillingFailure, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.BillingFailure{}, errs.ErrNotFound
	}
	if s.BillingFailure == nil {
		return domain.BillingFailure{}, errs.InvalidState("billing is not failing on this subscription; there is nothing to retry").WithCode("billing_not_failing")
	}
	cleared := *s.BillingFailure
	if inTx != nil {
		if err := inTx(nil, cleared); err != nil {
			return domain.BillingFailure{}, err
		}
	}
	s.BillingFailure = nil
	m.subs[id] = s
	return cleared, nil
}

func (m *memStore) List(ctx context.Context, filter ListFilter) ([]domain.Subscription, int, error) {
	var result []domain.Subscription
	for _, s := range m.subs {
		if s.TenantID != filter.TenantID {
			continue
		}
		if filter.Status != "" && string(s.Status) != filter.Status {
			continue
		}
		s.Items = m.hydrateItems(s.ID)
		result = append(result, s)
	}
	return result, len(result), nil
}

func (m *memStore) Update(ctx context.Context, tenantID string, s domain.Subscription) (domain.Subscription, error) {
	cur, ok := m.subs[s.ID]
	if !ok || cur.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	s.Items = cur.Items
	s.UpdatedAt = clock.Now(ctx)
	m.subs[s.ID] = s
	return s, nil
}

// ClosePeriodTx mirrors the store's ADR-115 CAS exactly: the row must still
// match `expected` (status, period start, watermark) or nothing changes.
func (m *memStore) ClosePeriodTx(_ context.Context, _ *sql.Tx, tenantID, id string, expected PeriodSnapshot, periodStart, periodEnd, nextBillingAt time.Time, anchorDay int) error {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return errs.ErrNotFound
	}
	if !snapshotMatches(s, expected) {
		return ErrWatermarkMoved
	}
	s.CurrentBillingPeriodStart = &periodStart
	s.CurrentBillingPeriodEnd = &periodEnd
	s.NextBillingAt = &nextBillingAt
	s.BillingAnchorDay = anchorDay
	m.subs[id] = s
	return nil
}

func snapshotMatches(s domain.Subscription, expected PeriodSnapshot) bool {
	sameTime := func(a, b *time.Time) bool {
		if a == nil || b == nil {
			return a == nil && b == nil
		}
		return a.Equal(*b)
	}
	return s.Status == expected.Status && sameTime(s.CurrentBillingPeriodStart, expected.Start) && sameTime(s.NextBillingAt, expected.Next)
}

// cancelAtomic mirrors the store's compare-and-set: a canceled row is
// already terminated, and any other row must still hold expected.
func (m *memStore) cancelAtomic(tenantID, id string, expected PeriodSnapshot, at time.Time) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status == domain.SubscriptionCanceled {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf("cannot cancel %s subscription (already terminated)", s.Status))
	}
	if !snapshotMatches(s, expected) {
		return domain.Subscription{}, ErrWatermarkMoved
	}
	s.Status = domain.SubscriptionCanceled
	s.CanceledAt = &at
	s.UpdatedAt = at
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

// The memory store keeps no billing intervals: both reads find none, so a
// cancel's interval check always passes here.
func (m *memStore) ListItemIntervals(context.Context, string, string) ([]domain.ItemInterval, error) {
	return nil, nil
}

func (m *memStore) ListItemIntervalsTx(context.Context, *sql.Tx, string) ([]domain.ItemInterval, error) {
	return nil, nil
}

// retireLiveOnlyState mirrors the store's retireLiveOnlyStateTx: the last
// write of every cancel writer clears the schedule and the items' pending
// plan changes, and keeps the pause.
func (m *memStore) retireLiveOnlyState(id string) domain.Subscription {
	s := m.subs[id]
	s.CancelAtPeriodEnd = false
	m.subs[id] = s
	for itemID, it := range m.items {
		if it.SubscriptionID == id && it.PendingPlanID != "" {
			it.PendingPlanID = ""
			it.PendingPlanEffectiveAt = nil
			m.items[itemID] = it
		}
	}
	s.Items = m.hydrateItems(id)
	return s
}

func (m *memStore) CancelAtomicWithBill(_ context.Context, tenantID, id string, expected PeriodSnapshot, at time.Time, billFn func(tx *sql.Tx, canceled domain.Subscription) error) (domain.Subscription, error) {
	prev, existed := m.subs[id]
	canceled, err := m.cancelAtomic(tenantID, id, expected, at)
	if err != nil {
		return domain.Subscription{}, err
	}
	if billFn != nil {
		if err := billFn(nil, canceled); err != nil {
			// Simulate the tx rollback: restore the pre-cancel row.
			if existed {
				m.subs[id] = prev
			}
			return domain.Subscription{}, err
		}
	}
	return m.retireLiveOnlyState(id), nil
}

func (m *memStore) ScheduleCancellation(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	// Mirror the store's status CAS.
	if s.Status != domain.SubscriptionActive {
		return domain.Subscription{}, errs.InvalidState(fmt.Sprintf(
			"This subscription just changed from %s to %s. Refresh the page and try again.",
			domain.SubscriptionActive, s.Status))
	}
	s.CancelAtPeriodEnd = true
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ClearScheduledCancellation(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if !s.CancelAtPeriodEnd {
		return domain.Subscription{}, ErrNoScheduledCancel // mirrors the real store's CAS
	}
	s.CancelAtPeriodEnd = false
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

// resumeLifted mirrors PostgresStore.clearLiftedPauseTx: a CAS on listed's
// pause (its resumes_at and setter), then the resume's audit hook.
func (m *memStore) resumeLifted(ctx context.Context, listed domain.Subscription, onResume ResumeAudit) (domain.Subscription, bool, error) {
	s, ok := m.subs[listed.ID]
	pc := listed.PauseCollection
	if !ok || pc == nil || pc.ResumesAt == nil || s.PauseCollection == nil || s.PauseCollection.ResumesAt == nil ||
		!s.PauseCollection.ResumesAt.Equal(*pc.ResumesAt) || s.PauseCollection.SetBy != pc.SetBy {
		return domain.Subscription{}, false, nil
	}
	r := liftedPauseResume(*pc)
	if pc.ResumesAt.After(clock.Now(ctx)) {
		return domain.Subscription{}, false, nil
	}
	s.PauseCollection = nil
	s.UpdatedAt = clock.Now(ctx)
	out := s
	out.Items = m.hydrateItems(s.ID)
	if onResume != nil {
		if err := onResume(ctx, nil, out, r); err != nil {
			return domain.Subscription{}, false, err
		}
	}
	m.subs[s.ID] = s
	return out, true, nil
}

// resumeIfLifted mirrors PostgresStore.resumeIfLiftedTx.
func (m *memStore) resumeIfLifted(ctx context.Context, id string, onResume ResumeAudit) error {
	s := m.subs[id]
	if s.PauseCollection == nil || s.PauseCollection.InForceAt(clock.Now(ctx)) {
		return nil
	}
	_, _, err := m.resumeLifted(ctx, s, onResume)
	return err
}

func (m *memStore) ResumeLiftedPause(ctx context.Context, tenantID string, listed domain.Subscription, onResume ResumeAudit) (domain.Subscription, error) {
	if m.clearPauseOverride != nil {
		return domain.Subscription{}, m.clearPauseOverride
	}
	if s, ok := m.subs[listed.ID]; !ok || s.TenantID != tenantID {
		return domain.Subscription{}, ErrNotPaused
	}
	out, ok, err := m.resumeLifted(ctx, listed, onResume)
	if err != nil {
		return domain.Subscription{}, err
	}
	if !ok {
		return domain.Subscription{}, ErrNotPaused
	}
	return out, nil
}

func (m *memStore) SetPauseCollection(ctx context.Context, tenantID, id string, pc domain.PauseCollection, onResume ResumeAudit) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status == domain.SubscriptionCanceled {
		return domain.Subscription{}, fmt.Errorf("Collection can't be paused on a %s subscription.", s.Status)
	}
	if err := m.resumeIfLifted(ctx, id, onResume); err != nil {
		return domain.Subscription{}, err
	}
	s = m.subs[id]
	pcCopy := pc
	if pc.ResumesAt != nil {
		t := *pc.ResumesAt
		pcCopy.ResumesAt = &t
	}
	pcCopy.SetBy = domain.PauseSetByOperator
	s.PauseCollection = &pcCopy
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

// SetDunningPause mirrors the real store's write rule (the invoice gate is
// the real store's alone: memStore has no invoices).
func (m *memStore) SetDunningPause(ctx context.Context, tenantID, subID, _ string, inTx func(tx *sql.Tx, sub domain.Subscription) error, onResume ResumeAudit) (domain.Subscription, DunningPauseOutcome, error) {
	s, ok := m.subs[subID]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, 0, errs.ErrNotFound
	}
	if s.Status == domain.SubscriptionCanceled {
		return domain.Subscription{}, 0, errs.InvalidState("terminal")
	}
	if s.PauseCollection.InForceAt(clock.Now(ctx)) {
		return domain.Subscription{}, DunningPauseAlreadyHeld, nil
	}
	if err := m.resumeIfLifted(ctx, subID, onResume); err != nil {
		return domain.Subscription{}, 0, err
	}
	s = m.subs[subID]
	s.PauseCollection = &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft, SetBy: domain.PauseSetByDunning}
	s.UpdatedAt = clock.Now(ctx)
	out := s
	out.Items = m.hydrateItems(subID)
	if inTx != nil {
		if err := inTx(nil, out); err != nil {
			return domain.Subscription{}, 0, err
		}
	}
	m.subs[subID] = s
	return out, DunningPauseWritten, nil
}

func (m *memStore) ClearPauseCollection(ctx context.Context, tenantID, id string, onResume ResumeAudit) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.PauseCollection == nil {
		return domain.Subscription{}, ErrNotPaused // mirrors the real store's CAS
	}
	if !s.PauseCollection.InForceAt(clock.Now(ctx)) {
		if err := m.resumeIfLifted(ctx, id, onResume); err != nil {
			return domain.Subscription{}, err
		}
		return domain.Subscription{}, ErrPauseLifted
	}
	s.PauseCollection = nil
	s.UpdatedAt = clock.Now(ctx)
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ListExpiredPauseCollections(_ context.Context, before time.Time, limit int) ([]domain.Subscription, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []domain.Subscription
	for _, s := range m.subs {
		if s.PauseCollection == nil || s.PauseCollection.ResumesAt == nil {
			continue
		}
		if s.PauseCollection.ResumesAt.After(before) {
			continue
		}
		s.Items = m.hydrateItems(s.ID)
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// LockForItemChangeTx: the in-memory store has no locks or transactions, so
// it returns the subscription as Get does. The lock is proven against real
// Postgres in item_change_race_integration_test.go.
func (m *memStore) LockForItemChangeTx(ctx context.Context, _ *sql.Tx, tenantID, subscriptionID string) (domain.Subscription, error) {
	return m.Get(ctx, tenantID, subscriptionID)
}

// LockPlansTx: no locks in memory. The order is proven against real Postgres
// in plan_lock_order_integration_test.go.
func (m *memStore) LockPlansTx(context.Context, *sql.Tx, []string) error { return nil }

// WithTenantTx: no transactions in memory; the *Tx methods ignore the tx.
// When fn fails, the rows it wrote are restored, as the real store's
// rollback does, so a test can see that a failed change left nothing behind.
func (m *memStore) WithTenantTx(_ context.Context, _ string, fn func(tx *sql.Tx) error) error {
	subs, items, changes := maps.Clone(m.subs), maps.Clone(m.items), len(m.itemChanges)
	if err := fn(nil); err != nil {
		m.subs, m.items, m.itemChanges = subs, items, m.itemChanges[:changes]
		return err
	}
	return nil
}

func (m *memStore) GetItem(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	return it, nil
}

// AddItemTx and the other item *Tx methods ignore the tx: the fake is in
// memory, and WithTenantTx restores what a failed transaction wrote.
func (m *memStore) AddItemTx(ctx context.Context, _ *sql.Tx, tenantID string, item domain.SubscriptionItem) (domain.SubscriptionItem, error) {
	for _, existing := range m.items {
		if existing.SubscriptionID == item.SubscriptionID && existing.PlanID == item.PlanID {
			return domain.SubscriptionItem{}, errs.ErrAlreadyExists
		}
	}
	item.ID = fmt.Sprintf("%s_item_%d", item.SubscriptionID, len(m.items)+1)
	item.TenantID = tenantID
	now := clock.Now(ctx)
	item.CreatedAt = now
	item.UpdatedAt = now
	m.items[item.ID] = item
	m.recordChange(item, "add", "", item.PlanID, 0, item.Quantity, now)
	return item, nil
}

func (m *memStore) UpdateItemQuantityTx(ctx context.Context, _ *sql.Tx, tenantID, itemID string, quantity int64) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	prevQty := it.Quantity
	it.Quantity = quantity
	it.UpdatedAt = clock.Now(ctx)
	m.items[itemID] = it
	m.recordChange(it, "quantity", it.PlanID, it.PlanID, prevQty, quantity, it.UpdatedAt)
	return it, nil
}

func (m *memStore) ApplyItemPlanImmediatelyTx(ctx context.Context, _ *sql.Tx, tenantID, itemID, newPlanID string, changedAt time.Time) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	oldPlan := it.PlanID
	it.PlanID = newPlanID
	it.PlanChangedAt = &changedAt
	it.PendingPlanID = ""
	it.PendingPlanEffectiveAt = nil
	it.UpdatedAt = changedAt
	m.items[itemID] = it
	m.recordChange(it, "plan", oldPlan, newPlanID, it.Quantity, it.Quantity, changedAt)
	return it, nil
}

func (m *memStore) RemoveItemTx(ctx context.Context, _ *sql.Tx, tenantID, itemID string) error {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return errs.ErrNotFound
	}
	delete(m.items, itemID)
	m.recordChange(it, "remove", it.PlanID, "", it.Quantity, 0, clock.Now(ctx))
	return nil
}

// FindMeterConflicts: the in-memory fake reports no conflicts — the
// double-billing guard's SQL (live-sub join + meter_ids unnest) is proven
// against real Postgres in meter_overlap_guard_integration_test.go.
func (m *memStore) FindMeterConflicts(context.Context, string, string, string, []string) ([]MeterConflict, error) {
	return nil, nil
}

func (m *memStore) SetItemPendingPlanTx(ctx context.Context, _ *sql.Tx, tenantID, itemID, pendingPlanID string, effectiveAt time.Time) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	it.PendingPlanID = pendingPlanID
	it.PendingPlanEffectiveAt = &effectiveAt
	it.UpdatedAt = clock.Now(ctx)
	m.items[itemID] = it
	return it, nil
}

func (m *memStore) ClearItemPendingPlan(ctx context.Context, tenantID, itemID string) (domain.SubscriptionItem, error) {
	it, ok := m.items[itemID]
	if !ok || it.TenantID != tenantID {
		return domain.SubscriptionItem{}, errs.ErrNotFound
	}
	if it.PendingPlanID == "" {
		return domain.SubscriptionItem{}, ErrNoPendingChange
	}
	it.PendingPlanID = ""
	it.PendingPlanEffectiveAt = nil
	it.UpdatedAt = clock.Now(ctx)
	m.items[itemID] = it
	return it, nil
}

// SetBillingThresholds is a minimal in-memory implementation matching the
// store contract: stores the BillingThresholds struct on the row and rejects
// terminal subs. The handler tests don't exercise this path; integration
// tests against real Postgres cover the full behaviour.
func (m *memStore) SetBillingThresholds(ctx context.Context, tenantID, id string, t domain.BillingThresholds) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	if s.Status == domain.SubscriptionCanceled {
		return domain.Subscription{}, errs.InvalidState("cannot configure billing thresholds on terminated subscription")
	}
	bt := t
	s.BillingThresholds = &bt
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ClearBillingThresholds(ctx context.Context, tenantID, id string) (domain.Subscription, error) {
	s, ok := m.subs[id]
	if !ok || s.TenantID != tenantID {
		return domain.Subscription{}, errs.ErrNotFound
	}
	s.BillingThresholds = nil
	m.subs[id] = s
	s.Items = m.hydrateItems(id)
	return s, nil
}

func (m *memStore) ListItemChangesInPeriod(_ context.Context, tenantID, subscriptionID string, periodStart, periodEnd time.Time) ([]domain.SubscriptionItemChange, error) {
	var out []domain.SubscriptionItemChange
	for _, c := range m.itemChanges {
		if c.TenantID != tenantID || c.SubscriptionID != subscriptionID {
			continue
		}
		if !c.ChangedAt.After(periodStart) || c.ChangedAt.After(periodEnd) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ChangedAt.Equal(out[j].ChangedAt) {
			return out[i].ChangedAt.Before(out[j].ChangedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// hydrateItems returns a subscription's items in the real store's order
// (created_at, then id), so a check that walks them answers the same way
// on every run.
func (m *memStore) hydrateItems(subID string) []domain.SubscriptionItem {
	var out []domain.SubscriptionItem
	for _, it := range m.items {
		if it.SubscriptionID == subID {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCreate(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	t.Run("multiple items", func(t *testing.T) {
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-multi", DisplayName: "Bundle",
			CustomerID: "cus_1",
			Items: []CreateItemInput{
				{PlanID: "pln_base", Quantity: 2},
				{PlanID: "pln_addon", Quantity: 10},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(sub.Items) != 2 {
			t.Fatalf("items count: got %d, want 2", len(sub.Items))
		}
	})

	t.Run("starts now by default", func(t *testing.T) {
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-002", DisplayName: "Active",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sub.Status != domain.SubscriptionActive {
			t.Errorf("got status %q, want active", sub.Status)
		}
		if sub.StartedAt == nil {
			t.Error("started_at should be set")
		}
		if sub.BillingTime != domain.BillingTimeCalendar {
			t.Errorf("got billing_time %q, want calendar", sub.BillingTime)
		}
		if len(sub.Items) != 1 || sub.Items[0].PlanID != "pln_1" || sub.Items[0].Quantity != 1 {
			t.Errorf("items: got %+v, want single pln_1 qty=1", sub.Items)
		}
	})

	t.Run("validation errors", func(t *testing.T) {
		cases := []CreateInput{
			{DisplayName: "x", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}},                                   // missing code
			{Code: "x", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}},                                          // missing display_name
			{Code: "x", DisplayName: "x", Items: []CreateItemInput{{PlanID: "p"}}},                                         // missing customer_id
			{Code: "x", DisplayName: "x", CustomerID: "c"},                                                                 // missing items
			{Code: "x", DisplayName: "x", CustomerID: "c", Items: []CreateItemInput{{}}},                                   // item missing plan_id
			{Code: "x", DisplayName: "x", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}, {PlanID: "p"}}},         // duplicate plan
			{Code: "x", DisplayName: "x", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}, BillingTime: "weekly"}, // invalid billing_time
		}
		for _, input := range cases {
			_, err := svc.Create(ctx, "t1", input)
			if err == nil {
				t.Errorf("expected error for input %+v", input)
			}
		}
	})

	t.Run("ADR-031 biller called for active sub", func(t *testing.T) {
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		_, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-bill-active", DisplayName: "Active",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
		})
		if err != nil {
			t.Fatalf("unexpected: %v", err)
		}
		// ADR-056: the day-1 invoice is now built in-tx via BillOnCreateTx.
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx called %d times, want 1", fb.createTxCalls)
		}
	})

	t.Run("PR-10: the final invoice is billed on Cancel (covers partial-period usage gap)", func(t *testing.T) {
		// Pre-fix: mid-period immediate cancel generated NO final
		// invoice. Customer's partial-period usage went uncaptured
		// (revenue leak). Fix: Service.Cancel now also calls
		// PlanCancelBill, which prices a usage + prorated
		// in_arrears base invoice for [period_start, canceled_at].
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-cancel-mid", DisplayName: "Mid-period cancel",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := svcWithBiller.Cancel(ctx, "t1", sub.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if fb.finalCalls != 1 {
			t.Errorf("final invoice billed %d times, want 1", fb.finalCalls)
		}
		// The proration relief also still runs — the two are independent
		// operations (invoice vs credit).
		if fb.finishCalls != 1 {
			t.Errorf("FinishRelief called %d times, want 1 (independent of the final invoice)", fb.finishCalls)
		}
	})

	t.Run("ADR-056 a final-on-cancel billing failure rolls the cancel back (atomic)", func(t *testing.T) {
		// Pre-ADR-056 a final-on-cancel invoice failure was swallowed: the sub
		// was canceled with the partial period UNINVOICED — a revenue leak (no
		// final-on-cancel reconciler exists; the "G3 deferred reconciler" the old
		// comment referenced never did). Now the final invoice is built IN the
		// cancel tx, so a failure rolls the cancel back rather than orphaning an
		// uninvoiced canceled sub.
		store := newMemStore()
		svcWithBiller := NewService(store, nil)
		fb := &fakeBiller{finalCancelErr: fmt.Errorf("invoice insert failed")}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-cancel-final-err", DisplayName: "Final errpath",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := svcWithBiller.Cancel(ctx, "t1", sub.ID); err == nil {
			t.Fatal("a final-on-cancel billing failure must fail Cancel (atomic), not leave an uninvoiced canceled sub")
		}
		// The cancel must have rolled back — the sub is still active, not canceled.
		after, gerr := store.Get(ctx, "t1", sub.ID)
		if gerr != nil {
			t.Fatalf("get after rollback: %v", gerr)
		}
		if after.Status != domain.SubscriptionActive {
			t.Errorf("cancel must roll back on a final-invoice failure; status=%q, want active", after.Status)
		}
		// The relief's post-commit legs must NOT run when the cancel didn't commit.
		if fb.finishCalls != 0 {
			t.Errorf("FinishRelief must not run when the cancel rolled back; got %d", fb.finishCalls)
		}
	})

	t.Run("ADR-031 cancel relief runs on Cancel", func(t *testing.T) {
		svcWithBiller := NewService(newMemStore(), nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-cancel-active", DisplayName: "Active to cancel",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := svcWithBiller.Cancel(ctx, "t1", sub.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if fb.finishCalls != 1 {
			t.Errorf("FinishRelief called %d times, want 1", fb.finishCalls)
		}
	})

	t.Run("ADR-118 a relief error fails the cancel and rolls it back", func(t *testing.T) {
		// It used to be logged while the cancel stood — the customer's relief
		// lost with no retry. Now the relief is written in the cancel tx, so its
		// failure is the cancel's failure; the caller retries.
		store := newMemStore()
		svcWithBiller := NewService(store, nil)
		fb := &fakeBiller{}
		svcWithBiller.SetBiller(fb)
		sub, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-cancel-errpath", DisplayName: "Error on cancel",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		fb.draftTxErr = fmt.Errorf("credit-note draft insert failed")
		if _, _, err := svcWithBiller.Cancel(ctx, "t1", sub.ID); err == nil {
			t.Fatal("a relief error must fail the cancel")
		}
		after, gerr := store.Get(ctx, "t1", sub.ID)
		if gerr != nil {
			t.Fatalf("get after rollback: %v", gerr)
		}
		if after.Status != domain.SubscriptionActive {
			t.Errorf("status %q after a failed relief, want active (rolled back)", after.Status)
		}
		if fb.finishCalls != 0 {
			t.Errorf("FinishRelief ran %d time(s) on a rolled-back cancel", fb.finishCalls)
		}
	})

	t.Run("ADR-056 a hard day-1 billing failure rolls back the create (atomic)", func(t *testing.T) {
		// Pre-ADR-056 a BillOnCreate failure was swallowed: the sub stayed
		// active with NO first-period invoice — a permanent revenue leak (the
		// cycle scheduler bills the UPCOMING period and skips the just-elapsed
		// in_advance segment, so it is NOT "deferred to next cycle close"). Now
		// the day-1 invoice is built IN the create tx, so a hard failure rolls
		// the whole create back rather than orphaning a sub.
		store := newMemStore()
		svcWithBiller := NewService(store, nil)
		fb := &fakeBiller{createTxErr: fmt.Errorf("invoice insert failed")}
		svcWithBiller.SetBiller(fb)
		_, err := svcWithBiller.Create(ctx, "t1", CreateInput{
			Code: "sub-bill-err", DisplayName: "Error path",
			CustomerID: "cus_1",
			Items:      []CreateItemInput{{PlanID: "pln_1"}},
		})
		if err == nil {
			t.Fatal("a day-1 billing failure must fail Create (atomic), not orphan a sub with no first-period invoice")
		}
		if len(store.subs) != 0 {
			t.Fatalf("subscription must be rolled back on a billing failure; got %d persisted", len(store.subs))
		}
		if fb.createTxCalls != 1 {
			t.Errorf("BillOnCreateTx called %d times, want 1", fb.createTxCalls)
		}
		if fb.finalizeCalls != 0 {
			t.Errorf("FinalizeOnCreateInvoice must NOT run when the create rolled back; got %d", fb.finalizeCalls)
		}
	})
}

// fakeBiller captures the billing calls a subscription change makes, for the
// ADR-031 / ADR-118 tests.
type fakeBiller struct {
	// finishErr is what FinishRelief returns: a relief note that was not issued.
	finishErr error
	// thresholdBilledThrough feeds ThresholdBilledThroughTx — the swap's
	// fire-vs-swap guard (ADR-115). nil = no threshold fire this period.
	thresholdBilledThrough    *time.Time
	thresholdBilledThroughErr error
	finalCalls                int
	finalCancelErr            error
	finalCancelInv            domain.Invoice
	createTxCalls             int
	finalizedAt               []time.Time // the instant each FinalizeOnCreateInvoice ran on
	createTxOK                bool
	createTxInv               domain.Invoice
	createTxErr               error
	// BillOnPlanSwapTx: what it was called with. It answers with the
	// createTx* fields, the swap's in-tx bill as BillOnCreateTx was before.
	swapBillCalls        int
	swapBillBefore       domain.Subscription
	swapBillAfter        domain.Subscription
	swapBillAt           time.Time
	swapAfterCommitCalls int
	finalizeCalls        int
	// Cancel relief (PlanCancelBill's writer): what it returns, and how often
	// it ran in the cancel tx.
	draftTxCalls int
	draftIDs     []string
	draftCredit  int64
	draftTxErr   error
	// planAts is the instant of each PlanCancelBill call and plannedFrom the
	// row it priced; writeMoved is how many writer calls answer
	// ErrWatermarkMoved first. onPlan, when set, runs inside each
	// PlanCancelBill call, as a change that commits while the cancel is
	// priced.
	planAts     []time.Time
	plannedFrom []domain.Subscription
	writeMoved  int
	onPlan      func()
	// Plan-swap relief (BillOnPlanSwapDraftsTx).
	swapDraftTxCalls int
	swapDraftIDs     []string
	swapDraftTxErr   error
	planSwapAt       time.Time
	// FinishRelief: post-commit runs and the draft ids they issued (cancel
	// and swap share it).
	finishCalls    int
	finishedDrafts []string
	// afterCommitCalls counts runs of the cancel writer's
	// afterCommit (the unpriced-usage count, #981): post-commit only.
	afterCommitCalls int
	// postCommit is the order of the post-commit legs: "relief" per
	// FinishRelief, "finalize:<invoice id>" per FinalizeOnCreateInvoice.
	postCommit []string
	// onAfterCommit runs inside the cancel writer's afterCommit,
	// right after the cancel commits; a test ends the caller's context here.
	onAfterCommit func()
	// postCommitCtxErr is ctx.Err() as each post-commit leg saw it, in
	// postCommit's order.
	postCommitCtxErr []error
}

// PlanCancelBill counts the plan in finalCalls, records its instant in
// planAts and its row in plannedFrom, and runs onPlan; it fails with
// finalCancelErr. Its writer counts the relief write in draftTxCalls and fails
// with draftTxErr. While writeMoved is above zero, the writer answers
// ErrWatermarkMoved instead, once per call, as a change that committed while
// the cancel was priced would.
func (f *fakeBiller) PlanCancelBill(_ context.Context, sub domain.Subscription, at time.Time) (BillWriter, error) {
	f.finalCalls++
	f.planAts = append(f.planAts, at)
	f.plannedFrom = append(f.plannedFrom, sub)
	if f.onPlan != nil {
		f.onPlan()
	}
	if f.finalCancelErr != nil {
		return nil, f.finalCancelErr
	}
	return func(context.Context, *sql.Tx, domain.Subscription) (domain.Invoice, domain.Relief, func(), error) {
		if f.writeMoved > 0 {
			f.writeMoved--
			return domain.Invoice{}, domain.Relief{}, nil, ErrWatermarkMoved
		}
		f.draftTxCalls++
		if f.draftTxErr != nil {
			return domain.Invoice{}, domain.Relief{}, nil, f.draftTxErr
		}
		afterCommit := func() {
			f.afterCommitCalls++
			if f.onAfterCommit != nil {
				f.onAfterCommit()
			}
		}
		return f.finalCancelInv, domain.Relief{DraftIDs: f.draftIDs, CreditedCents: f.draftCredit}, afterCommit, nil
	}, nil
}

func (f *fakeBiller) BillOnPlanSwapDraftsTx(_ context.Context, _ *sql.Tx, _ domain.Subscription, at time.Time) (domain.Relief, error) {
	f.swapDraftTxCalls++
	f.planSwapAt = at
	if f.swapDraftTxErr != nil {
		return domain.Relief{}, f.swapDraftTxErr
	}
	return domain.Relief{DraftIDs: f.swapDraftIDs}, nil
}

func (f *fakeBiller) FinishRelief(ctx context.Context, _ domain.Subscription, r domain.Relief) error {
	f.finishCalls++
	f.finishedDrafts = append(f.finishedDrafts, r.DraftIDs...)
	f.postCommit = append(f.postCommit, "relief")
	f.postCommitCtxErr = append(f.postCommitCtxErr, ctx.Err())
	return f.finishErr
}

func (f *fakeBiller) BillOnCreateTx(_ context.Context, _ *sql.Tx, _ domain.Subscription, _ time.Time) (domain.Invoice, bool, error) {
	f.createTxCalls++
	return f.createTxInv, f.createTxOK, f.createTxErr
}

func (f *fakeBiller) BillOnPlanSwapTx(_ context.Context, _ *sql.Tx, before, after domain.Subscription, at time.Time) (domain.Invoice, bool, func(), error) {
	f.swapBillCalls++
	f.swapBillBefore, f.swapBillAfter, f.swapBillAt = before, after, at
	return f.createTxInv, f.createTxOK, func() { f.swapAfterCommitCalls++ }, f.createTxErr
}

func (f *fakeBiller) ThresholdBilledThroughTx(_ context.Context, _ *sql.Tx, _ domain.Subscription) (*time.Time, error) {
	return f.thresholdBilledThrough, f.thresholdBilledThroughErr
}

func (f *fakeBiller) FinalizeOnCreateInvoice(ctx context.Context, _ domain.Subscription, inv domain.Invoice) {
	f.finalizeCalls++
	f.finalizedAt = append(f.finalizedAt, clock.Now(ctx))
	f.postCommit = append(f.postCommit, "finalize:"+inv.ID)
	f.postCommitCtxErr = append(f.postCommitCtxErr, ctx.Err())
}

// fakePlanReader returns plans by id — lets tests stub plan intervals
// for the yearly-vs-monthly period-anchoring branches without standing
// up the full pricing package.
type fakePlanReader struct {
	plans map[string]domain.Plan
}

func (f *fakePlanReader) GetPlan(_ context.Context, _, planID string) (domain.Plan, error) {
	if p, ok := f.plans[planID]; ok {
		return p, nil
	}
	return domain.Plan{}, fmt.Errorf("no stub for plan %s", planID)
}

// fakeSettings is a SettingsReader that returns a fixed timezone — used
// to assert the day-grade snap honours the tenant's configured TZ.
type fakeSettings struct{ tz string }

func (f fakeSettings) Get(_ context.Context, _ string) (domain.TenantSettings, error) {
	return domain.TenantSettings{Timezone: f.tz}, nil
}

// TestPeriod_DayGradeSnap pins the day-grade calendar billing
// behaviour: a subscription created at any wall-clock time on a given
// day produces a period whose start is 00:00 in tenant TZ, NOT the
// raw timestamp. Without this, a 14:00 signup gets billed for 30/31
// days (truncating int division) even though the UI shows the period
// as a whole month — Chargebee / Lago default. ADR follow-up.
func TestPeriod_DayGradeSnap(t *testing.T) {
	ctx := context.Background()

	// Asia/Kolkata: UTC+05:30, no DST. Picked because the screenshot
	// that surfaced this bug used IST and because no-DST keeps the
	// expected values trivially predictable.
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatalf("load Asia/Kolkata: %v", err)
	}
	// Fix the clock at May 1, 2026 14:00 IST. Pre-snap, periodStart
	// would be this exact instant; post-snap it should be May 1 00:00
	// IST.
	creation := time.Date(2026, 5, 1, 14, 0, 0, 0, ist)
	expectedStart := time.Date(2026, 5, 1, 0, 0, 0, 0, ist).UTC()
	expectedEnd := time.Date(2026, 6, 1, 0, 0, 0, 0, ist).UTC()

	svc := NewService(newMemStore(), clock.NewFake(creation))
	svc.SetSettingsReader(fakeSettings{tz: "Asia/Kolkata"})

	sub, err := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-snap-calendar", DisplayName: "Snap Calendar",
		CustomerID: "cus_1",
		Items:      []CreateItemInput{{PlanID: "pln_1"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(expectedStart) {
		t.Errorf("calendar periodStart: got %v, want %v", sub.CurrentBillingPeriodStart, expectedStart)
	}
	if sub.CurrentBillingPeriodEnd == nil || !sub.CurrentBillingPeriodEnd.Equal(expectedEnd) {
		t.Errorf("calendar periodEnd: got %v, want %v", sub.CurrentBillingPeriodEnd, expectedEnd)
	}

	// Anniversary billing: same snap rule — 14:00 signup → 00:00
	// start in tenant TZ. For a May 1 IST start the next anniversary is
	// June 1 IST (May has 31 days), identical to the calendar boundary —
	// computed in the TENANT zone (ADR-058), NOT a UTC AddDate on the
	// UTC-located start (which lands a day early on May 31 IST).
	sub, err = svc.Create(ctx, "t1", CreateInput{
		Code: "sub-snap-anniversary", DisplayName: "Snap Anniversary",
		CustomerID:  "cus_1",
		Items:       []CreateItemInput{{PlanID: "pln_1"}},
		BillingTime: domain.BillingTimeAnniversary,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.CurrentBillingPeriodStart == nil || !sub.CurrentBillingPeriodStart.Equal(expectedStart) {
		t.Errorf("anniversary periodStart: got %v, want %v", sub.CurrentBillingPeriodStart, expectedStart)
	}
	annivExpectedEnd := expectedEnd // June 1 IST — same as the calendar boundary for a 1st-of-month start
	if sub.CurrentBillingPeriodEnd == nil || !sub.CurrentBillingPeriodEnd.Equal(annivExpectedEnd) {
		t.Errorf("anniversary periodEnd: got %v, want %v", sub.CurrentBillingPeriodEnd, annivExpectedEnd)
	}

	// SettingsReader nil → UTC fallback, behaviour stays correct.
	// Operators who haven't configured a TZ get the same shape, just
	// against the UTC clock instead.
	svcUTC := NewService(newMemStore(), clock.NewFake(creation))
	subUTC, err := svcUTC.Create(ctx, "t1", CreateInput{
		Code: "sub-snap-utc", DisplayName: "Snap UTC",
		CustomerID: "cus_1",
		Items:      []CreateItemInput{{PlanID: "pln_1"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 14:00 IST = 08:30 UTC, so the UTC start-of-day is May 1 00:00
	// UTC = April 30 18:30 IST. The snap targets UTC midnight here.
	expectedUTCStart := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if !subUTC.CurrentBillingPeriodStart.Equal(expectedUTCStart) {
		t.Errorf("UTC fallback periodStart: got %v, want %v", subUTC.CurrentBillingPeriodStart, expectedUTCStart)
	}
}

func TestCancel_ActiveThenCanceled(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	sub, _ := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-act", DisplayName: "Test", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "p"}},
	})

	t.Run("cancel active", func(t *testing.T) {
		canceled, _, err := svc.Cancel(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if canceled.Status != domain.SubscriptionCanceled {
			t.Errorf("got status %q, want canceled", canceled.Status)
		}
	})

	t.Run("cannot cancel canceled", func(t *testing.T) {
		_, _, err := svc.Cancel(ctx, "t1", sub.ID)
		if err == nil {
			t.Fatal("expected error canceling already canceled")
		}
	})
}

// TestUpdateItem_PlanChange exercises immediate and scheduled per-item plan
// changes. Mirrors the invariants the old TestChangePlan covered, scoped to
// a single item: immediate stamps plan_changed_at and overwrites plan_id;
// scheduled leaves plan_id alone and records pending fields keyed to the
// current billing period end.
func TestUpdateItem_PlanChange(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	sub, _ := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-change", DisplayName: "Test", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "plan_old"}},
	})

	// Set billing period so scheduled-change effective_at lines up with it.
	store := svc.store.(*memStore)
	s := store.subs[sub.ID]
	start := clock.Now(ctx).AddDate(0, 0, -15)
	end := clock.Now(ctx).AddDate(0, 0, 15)
	s.CurrentBillingPeriodStart = &start
	s.CurrentBillingPeriodEnd = &end
	store.subs[sub.ID] = s

	itemID := sub.Items[0].ID

	t.Run("immediate plan swap", func(t *testing.T) {
		result, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{
			NewPlanID: "plan_new",
			Immediate: true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Item.PlanID != "plan_new" {
			t.Errorf("plan_id: got %q, want plan_new", result.Item.PlanID)
		}
		if result.Item.PlanChangedAt == nil {
			t.Error("plan_changed_at should be set")
		}
	})

	t.Run("same plan rejected", func(t *testing.T) {
		_, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{NewPlanID: "plan_new", Immediate: true})
		if err == nil {
			t.Fatal("expected error for same plan")
		}
	})

	t.Run("missing new_plan_id and quantity", func(t *testing.T) {
		_, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{})
		if err == nil {
			t.Fatal("expected error for missing fields")
		}
	})

	t.Run("both quantity and new_plan_id rejected", func(t *testing.T) {
		q := int64(2)
		_, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{Quantity: &q, NewPlanID: "other"})
		if err == nil {
			t.Fatal("expected error when both fields set")
		}
	})
}

// TestUpdateItem_Scheduled locks in that a non-immediate plan change records
// pending fields at the billing period boundary and leaves the active plan_id
// untouched. Cancel-pending and immediate-supersede-pending are exercised too.
func TestUpdateItem_Scheduled(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	sub, _ := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-sched", DisplayName: "Test", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "plan_old"}},
	})

	store := svc.store.(*memStore)
	s := store.subs[sub.ID]
	start := clock.Now(ctx).AddDate(0, 0, -5)
	end := clock.Now(ctx).AddDate(0, 0, 25)
	s.CurrentBillingPeriodStart = &start
	s.CurrentBillingPeriodEnd = &end
	store.subs[sub.ID] = s

	itemID := sub.Items[0].ID

	t.Run("records pending, leaves plan_id untouched", func(t *testing.T) {
		result, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{
			NewPlanID: "plan_new",
			Immediate: false,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result.Item.PlanID != "plan_old" {
			t.Errorf("plan_id must not change on scheduled: got %q, want plan_old", result.Item.PlanID)
		}
		if result.Item.PendingPlanID != "plan_new" {
			t.Errorf("pending_plan_id: got %q, want plan_new", result.Item.PendingPlanID)
		}
		if result.Item.PendingPlanEffectiveAt == nil {
			t.Fatal("pending_plan_effective_at must be set")
		}
		if !result.Item.PendingPlanEffectiveAt.Equal(end) {
			t.Errorf("pending_plan_effective_at: got %v, want period end %v", *result.Item.PendingPlanEffectiveAt, end)
		}
		if !result.EffectiveAt.Equal(end) {
			t.Errorf("response effective_at: got %v, want %v", result.EffectiveAt, end)
		}
	})

	t.Run("cancel pending clears scheduled change", func(t *testing.T) {
		updated, canceled, err := svc.CancelPendingItemChange(ctx, "t1", sub.ID, itemID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !canceled {
			t.Error("canceled = false, want true: a change was scheduled")
		}
		if updated.PendingPlanID != "" {
			t.Errorf("pending_plan_id should be cleared: got %q", updated.PendingPlanID)
		}
		if updated.PendingPlanEffectiveAt != nil {
			t.Errorf("pending_plan_effective_at should be nil: got %v", updated.PendingPlanEffectiveAt)
		}
		if updated.PlanID != "plan_old" {
			t.Errorf("plan_id should remain unchanged: got %q", updated.PlanID)
		}
	})

	t.Run("cancel with no pending is no-op", func(t *testing.T) {
		updated, canceled, err := svc.CancelPendingItemChange(ctx, "t1", sub.ID, itemID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if canceled {
			t.Error("canceled = true, want false: nothing was scheduled")
		}
		if updated.PendingPlanID != "" {
			t.Errorf("expected no pending: got %q", updated.PendingPlanID)
		}
	})

	t.Run("immediate supersedes pending", func(t *testing.T) {
		_, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{NewPlanID: "plan_scheduled", Immediate: false})
		if err != nil {
			t.Fatalf("schedule: %v", err)
		}
		result, err := svc.UpdateItem(ctx, "t1", sub.ID, itemID, UpdateItemInput{NewPlanID: "plan_immediate", Immediate: true})
		if err != nil {
			t.Fatalf("immediate: %v", err)
		}
		if result.Item.PlanID != "plan_immediate" {
			t.Errorf("plan_id: got %q, want plan_immediate", result.Item.PlanID)
		}
		if result.Item.PendingPlanID != "" {
			t.Errorf("pending_plan_id should be cleared: got %q", result.Item.PendingPlanID)
		}
	})
}

// TestAddRemoveItem covers item add/remove rules: duplicate plans rejected,
// removing a subscription's last item is blocked (the caller should Cancel
// instead), a canceled subscription's items do not change, removing a
// non-last item succeeds.
func TestAddRemoveItem(t *testing.T) {
	svc := NewService(newMemStore(), nil)
	ctx := context.Background()

	sub, _ := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-items", DisplayName: "Test", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "plan_base"}},
	})

	t.Run("add second item", func(t *testing.T) {
		added, err := svc.AddItem(ctx, "t1", sub.ID, AddItemInput{PlanID: "plan_addon", Quantity: 5})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if added.PlanID != "plan_addon" || added.Quantity != 5 {
			t.Errorf("added item: got %+v", added)
		}
	})

	t.Run("duplicate plan rejected", func(t *testing.T) {
		_, err := svc.AddItem(ctx, "t1", sub.ID, AddItemInput{PlanID: "plan_base"})
		if err == nil {
			t.Fatal("expected ErrAlreadyExists for duplicate plan on subscription")
		}
	})

	t.Run("remove non-last item", func(t *testing.T) {
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		var addonID string
		for _, it := range fresh.Items {
			if it.PlanID == "plan_addon" {
				addonID = it.ID
			}
		}
		if addonID == "" {
			t.Fatal("addon item not found")
		}
		if err := svc.RemoveItem(ctx, "t1", sub.ID, addonID); err != nil {
			t.Fatalf("remove item: %v", err)
		}
	})

	t.Run("cannot remove last item on active subscription", func(t *testing.T) {
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		if len(fresh.Items) != 1 {
			t.Fatalf("expected 1 item remaining, got %d", len(fresh.Items))
		}
		if err := svc.RemoveItem(ctx, "t1", sub.ID, fresh.Items[0].ID); !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("removing the last item: got %v, want ErrInvalidState", err)
		}
		if after, _ := svc.Get(ctx, "t1", sub.ID); len(after.Items) != 1 {
			t.Fatalf("the subscription has %d items after the refused removal, want 1", len(after.Items))
		}
	})

	// As for adding an item: a canceled subscription's items do not change.
	t.Run("cannot remove an item from a canceled subscription", func(t *testing.T) {
		two, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-canceled-items", DisplayName: "Canceled", CustomerID: "c3",
			Items: []CreateItemInput{{PlanID: "plan_base"}, {PlanID: "plan_addon"}},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if _, _, err := svc.Cancel(ctx, "t1", two.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if err := svc.RemoveItem(ctx, "t1", two.ID, two.Items[0].ID); !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("removing an item from a canceled subscription: got %v, want ErrInvalidState", err)
		}
	})
}

// TestScheduleCancel covers the service layer of the scheduled cancel: it
// sets the at-period-end flag, a canceled subscription is refused before
// anything is written, and the clear undoes the schedule.
func TestScheduleCancel(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	periodStart := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	periodEnd := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	mkSvc := func() (*Service, domain.Subscription) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(context.Background(), "t1", CreateInput{
			Code: "sub-sched", DisplayName: "Test", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}},
		})
		stored, _ := svc.Get(context.Background(), "t1", sub.ID)
		stored.CurrentBillingPeriodStart = &periodStart
		stored.CurrentBillingPeriodEnd = &periodEnd
		_, _ = svc.store.(*memStore).Update(context.Background(), "t1", stored)
		return svc, stored
	}

	t.Run("at_period_end sets the flag", func(t *testing.T) {
		svc, sub := mkSvc()
		out, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !out.CancelAtPeriodEnd {
			t.Error("cancel_at_period_end should be true")
		}
	})

	t.Run("refuses a canceled subscription", func(t *testing.T) {
		svc, sub := mkSvc()
		store := svc.store.(*memStore)
		canceled := store.subs[sub.ID]
		canceled.Status = domain.SubscriptionCanceled
		canceled.CanceledAt = &now
		store.subs[sub.ID] = canceled
		_, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID)
		if !errors.Is(err, errs.ErrInvalidState) {
			t.Fatalf("err = %v, want invalid state", err)
		}
		if store.subs[sub.ID].CancelAtPeriodEnd {
			t.Error("the refused schedule set cancel_at_period_end")
		}
	})

	t.Run("clear undoes any prior schedule", func(t *testing.T) {
		svc, sub := mkSvc()
		_, err := svc.ScheduleCancel(context.Background(), "t1", sub.ID)
		if err != nil {
			t.Fatalf("schedule: %v", err)
		}
		out, cleared, err := svc.ClearScheduledCancel(context.Background(), "t1", sub.ID)
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if !cleared {
			t.Error("cleared = false, want true: a schedule was removed")
		}
		if out.CancelAtPeriodEnd {
			t.Error("cancel_at_period_end should be false after clear")
		}
	})
}

// TestPauseCollection covers the v1 service-layer validation for the
// pause_collection surface: behavior whitelist (only keep_as_draft for now),
// resumes_at must be future or nil, and the round-trip pause → resume path.
func TestPauseCollection(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	mkSvc := func() (*Service, domain.Subscription) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, _ := svc.Create(context.Background(), "t1", CreateInput{
			Code: "sub-pause", DisplayName: "Test", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p"}},
		})
		stored, _ := svc.Get(context.Background(), "t1", sub.ID)
		return svc, stored
	}

	t.Run("behavior keep_as_draft accepted, no resumes_at", func(t *testing.T) {
		svc, sub := mkSvc()
		out, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior: domain.PauseCollectionKeepAsDraft,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.PauseCollection == nil {
			t.Fatal("PauseCollection must be set")
		}
		if out.PauseCollection.Behavior != domain.PauseCollectionKeepAsDraft {
			t.Errorf("behavior: got %q, want %q", out.PauseCollection.Behavior, domain.PauseCollectionKeepAsDraft)
		}
		if out.PauseCollection.ResumesAt != nil {
			t.Error("ResumesAt must be nil when not provided")
		}
	})

	t.Run("future resumes_at accepted", func(t *testing.T) {
		svc, sub := mkSvc()
		future := now.Add(7 * 24 * time.Hour)
		out, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &future,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.PauseCollection == nil || out.PauseCollection.ResumesAt == nil {
			t.Fatal("ResumesAt must be set")
		}
		if !out.PauseCollection.ResumesAt.Equal(future) {
			t.Errorf("ResumesAt: got %v, want %v", out.PauseCollection.ResumesAt, future)
		}
	})

	t.Run("rejects empty behavior", func(t *testing.T) {
		svc, sub := mkSvc()
		_, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{})
		if err == nil {
			t.Fatal("expected error for empty behavior")
		}
	})

	t.Run("rejects unsupported behavior", func(t *testing.T) {
		svc, sub := mkSvc()
		_, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior: domain.PauseCollectionBehavior("mark_uncollectible"),
		})
		if err == nil {
			t.Fatal("expected error for unsupported behavior")
		}
	})

	t.Run("rejects past resumes_at", func(t *testing.T) {
		svc, sub := mkSvc()
		past := now.Add(-time.Hour)
		_, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &past,
		})
		if err == nil {
			t.Fatal("expected error for past resumes_at")
		}
	})

	t.Run("resume clears the pause", func(t *testing.T) {
		svc, sub := mkSvc()
		// In force on the store's clock too (the ctx clock, wall time here):
		// a pause lifted by then resumes with its own trigger, not the
		// operator's (TestLiftedPause_AResumeCallAnnouncesTheLift).
		future := time.Now().Add(time.Hour)
		if _, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &future,
		}); err != nil {
			t.Fatalf("pause: %v", err)
		}
		out, resumed, err := svc.ResumeCollection(context.Background(), "t1", sub.ID)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if !resumed {
			t.Error("resumed = false, want true: a pause was cleared")
		}
		if out.PauseCollection != nil {
			t.Errorf("PauseCollection should be nil after resume, got %+v", out.PauseCollection)
		}
	})

	t.Run("re-pause replaces resumes_at", func(t *testing.T) {
		svc, sub := mkSvc()
		first := now.Add(time.Hour)
		if _, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &first,
		}); err != nil {
			t.Fatalf("first: %v", err)
		}
		second := now.Add(48 * time.Hour)
		out, err := svc.PauseCollection(context.Background(), "t1", sub.ID, PauseCollectionInput{
			Behavior:  domain.PauseCollectionKeepAsDraft,
			ResumesAt: &second,
		})
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if !out.PauseCollection.ResumesAt.Equal(second) {
			t.Errorf("ResumesAt: got %v, want %v (second call must replace first)",
				out.PauseCollection.ResumesAt, second)
		}
	})
}

// steppingClock reads one second later each time and keeps every read.
type steppingClock struct {
	at     time.Time
	issued []time.Time
}

func (c *steppingClock) Now(context.Context) time.Time {
	c.at = c.at.Add(time.Second)
	c.issued = append(c.issued, c.at)
	return c.at
}

// TestPeriodAnchoring: a created subscription's first billing period starts
// on the create day. Calendar billing gives a stub period that ends at the
// next calendar month; anniversary gives a full cycle from the create day.
func TestPeriodAnchoring(t *testing.T) {
	// Pick a mid-month "now" so the calendar-billing branch produces a
	// non-trivial stub. UTC for all snaps — no tenant TZ surprises.
	now := time.Date(2025, 11, 29, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("Create + calendar on a month boundary → full cycle, no stub", func(t *testing.T) {
		// Created on Dec 1: the period starts on a calendar boundary, so
		// it is a full cycle Dec 1 → Jan 1, not an empty stub.
		onBoundary := time.Date(2025, 12, 1, 9, 0, 0, 0, time.UTC)
		svc := NewService(newMemStore(), clock.NewFake(onBoundary))
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p"}},
			BillingTime: domain.BillingTimeCalendar,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		wantPeriodStart := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (full cycle, no stub)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Create + calendar → period starts on the create day, NOT month start", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p"}},
			BillingTime: domain.BillingTimeCalendar,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// period_start = the create instant snapped to its day (Nov 29
		// 00:00), NOT the start of the month (Nov 1): the customer is not
		// billed for days before the subscription existed.
		wantPeriodStart := time.Date(2025, 11, 29, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v (NOT backdated to Nov 1)", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Create + anniversary → period starts on the create day, ends +1 month", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p"}},
			BillingTime: domain.BillingTimeAnniversary,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		wantPeriodStart := time.Date(2025, 11, 29, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2025, 12, 29, 0, 0, 0, 0, time.UTC)
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (anniversary cycle, NOT calendar)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Create + anniversary + yearly plan → first period is 1 YEAR, not 1 month (Bug #10)", func(t *testing.T) {
		// The period helpers read the plan's interval. With monthly math a
		// yearly plan's first cycle would bill 1/12 of the yearly fee.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p": {ID: "p", BillingInterval: domain.BillingYearly},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p"}},
			BillingTime: domain.BillingTimeAnniversary,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		wantPeriodStart := time.Date(2025, 11, 29, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC) // +1 YEAR
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (full year, NOT 1 month)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})

	t.Run("Create rejects mixed monthly + yearly items (Stripe parity)", func(t *testing.T) {
		// Stripe / Lago / Chargebee all reject mixed intervals on the
		// same sub because the period anchor is per-sub and a monthly +
		// yearly mix has no coherent cycle. Hardcount should too.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_monthly": {ID: "p_monthly", BillingInterval: domain.BillingMonthly},
			"p_yearly":  {ID: "p_yearly", BillingInterval: domain.BillingYearly},
		}})
		_, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{
				{PlanID: "p_monthly"},
				{PlanID: "p_yearly"},
			},
		})
		if err == nil {
			t.Fatal("expected error for mixed monthly + yearly items")
		}
	})

	t.Run("Create allows multiple items with same interval", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p1": {ID: "p1", BillingInterval: domain.BillingMonthly},
			"p2": {ID: "p2", BillingInterval: domain.BillingMonthly},
		}})
		_, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p1"}, {PlanID: "p2"}},
		})
		if err != nil {
			t.Errorf("expected success for matching-interval items: %v", err)
		}
	})

	t.Run("UpdateItem rejects plan-change that would mix intervals on a multi-item sub", func(t *testing.T) {
		// On a multi-item sub, swapping ONE item's plan to a different
		// interval would create a mix. Reject. Single-item plan-change
		// (e.g. only item swaps monthly → yearly) is NOT covered by
		// this guard — that's a clean interval swap, not a mix, and
		// other platforms permit it.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_monthly_a": {ID: "p_monthly_a", BillingInterval: domain.BillingMonthly},
			"p_monthly_b": {ID: "p_monthly_b", BillingInterval: domain.BillingMonthly},
			"p_yearly":    {ID: "p_yearly", BillingInterval: domain.BillingYearly},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p_monthly_a"}, {PlanID: "p_monthly_b"}},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Immediate plan-change to yearly while the OTHER item stays monthly → mix.
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_yearly",
			Immediate: true,
		})
		if err == nil {
			t.Error("expected error on immediate plan-change that would mix intervals")
		}
		// Scheduled plan-change should also be rejected at request time
		// (Stripe parity — you can't queue a state that would be
		// invalid when it lands).
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_yearly",
			Immediate: false,
		})
		if err == nil {
			t.Error("expected error on scheduled plan-change that would mix intervals")
		}
	})

	t.Run("AddItem rejects mismatched interval against existing sub", func(t *testing.T) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_monthly": {ID: "p_monthly", BillingInterval: domain.BillingMonthly},
			"p_yearly":  {ID: "p_yearly", BillingInterval: domain.BillingYearly},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p_monthly"}},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = svc.AddItem(ctx, "t1", sub.ID, AddItemInput{PlanID: "p_yearly"})
		if err == nil {
			t.Fatal("expected error when adding yearly item to monthly sub")
		}
	})

	t.Run("Create rejects mixed in_arrears + in_advance items", func(t *testing.T) {
		// Bill-timing mix on the same sub would emit inconsistent
		// invoice lines (arrears-close + advance-open under different
		// rules). Hardcount's hybrid invoice shape assumes a uniform
		// bill_timing across items, so reject at request time.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_arrears": {ID: "p_arrears", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
			"p_advance": {ID: "p_advance", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance},
		}})
		_, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{
				{PlanID: "p_arrears"},
				{PlanID: "p_advance"},
			},
		})
		if err == nil {
			t.Fatal("expected error for mixed in_arrears + in_advance items")
		}
	})

	t.Run("UpdateItem rejects plan-swap that changes bill_timing on single-item sub (immediate=false)", func(t *testing.T) {
		// User's exact scenario: active sub on Plan A
		// (in_arrears $29/mo). Schedule a swap to Plan B
		// (in_advance $49/mo). The cross-bill-timing boundary swap
		// path is not exercised end-to-end, so reject at request
		// time and steer the operator to cancel + recreate.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_arrears": {ID: "p_arrears", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
			"p_advance": {ID: "p_advance", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p_arrears"}},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_advance",
			Immediate: false,
		})
		if err == nil {
			t.Error("expected error on scheduled plan-swap that changes bill_timing")
		}
	})

	t.Run("UpdateItem rejects plan-swap that changes bill_timing on single-item sub (immediate=true)", func(t *testing.T) {
		// Cross-cadence (in_advance ↔ in_arrears) plan-swaps are
		// rejected — 2026-05-21 industry verification across Stripe /
		// Lago / Orb / Chargebee / Recurly / Metronome found no
		// documented support for swapping a customer between a prepaid
		// plan and a postpaid plan as an in-place operation. Lago — the
		// closest model to Hardcount — documents same-cadence transitions
		// only. Operator path: cancel + recreate.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_arrears": {ID: "p_arrears", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
			"p_advance": {ID: "p_advance", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p_arrears"}},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_advance",
			Immediate: true,
		})
		if err == nil {
			t.Error("expected error on immediate plan-swap that changes bill_timing")
		}
	})

	t.Run("UpdateItem immediate cross-interval in_arrears truncates period to now", func(t *testing.T) {
		// Same-cadence cross-interval (monthly→yearly, both in_arrears)
		// truncates the current period to `now`. Scheduler picks up
		// next_billing_at=now, closes (oldPS, now) under OLD plan via
		// segment-aware billing, then opens a new period under NEW
		// plan's yearly interval. No synchronous bill.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_monthly": {ID: "p_monthly", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears, BaseAmountCents: 2900},
			"p_yearly":  {ID: "p_yearly", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInArrears, BaseAmountCents: 28800},
		}})
		fb := &fakeBiller{}
		svc.SetBiller(fb)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_monthly"}},
			BillingTime: domain.BillingTimeAnniversary,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Reset the day-1 bill counter: Create calls BillOnCreateTx even for
		// in_arrears (the engine no-ops internally). The assertion below is
		// about the swap path, not Create's call.
		fb.createTxCalls = 0
		oldPS := *sub.CurrentBillingPeriodStart
		// The non-atomic UpdateItem refuses a cycle-restructuring swap
		// (ADR-118); the handler runs it through UpdateItemTx.
		if _, err := svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_yearly", Immediate: true,
		}); errs.Code(err) != "swap_requires_atomic_path" {
			t.Fatalf("non-atomic cross-interval swap: err = %v, want swap_requires_atomic_path", err)
		}
		res, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_yearly",
			Immediate: true,
		})
		if err != nil {
			t.Fatalf("immediate cross-interval in_arrears swap: %v", err)
		}
		if res.Item.PlanID != "p_yearly" {
			t.Errorf("item plan after swap: got %q want p_yearly", res.Item.PlanID)
		}
		// in_arrears path: the refund is still written in the swap tx (the
		// engine finds nothing prebilled and writes nothing) — uniform shape.
		if fb.swapDraftTxCalls != 1 {
			t.Errorf("expected 1 BillOnPlanSwapDraftsTx call, got %d", fb.swapDraftTxCalls)
		}
		// in_arrears NEW plan: no new-period bill.
		if fb.createTxCalls != 0 || fb.swapBillCalls != 0 {
			t.Errorf("expected no new-period bill (in_arrears NEW), got BillOnCreateTx=%d BillOnPlanSwapTx=%d", fb.createTxCalls, fb.swapBillCalls)
		}
		updated, err := svc.store.Get(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("get updated sub: %v", err)
		}
		if updated.CurrentBillingPeriodStart == nil || !updated.CurrentBillingPeriodStart.Equal(oldPS) {
			t.Errorf("period_start should be preserved: got %v want %v", updated.CurrentBillingPeriodStart, oldPS)
		}
		if updated.CurrentBillingPeriodEnd == nil || !updated.CurrentBillingPeriodEnd.Equal(now) {
			t.Errorf("period_end should be truncated to now: got %v want %v", updated.CurrentBillingPeriodEnd, now)
		}
		if updated.NextBillingAt == nil || !updated.NextBillingAt.Equal(now) {
			t.Errorf("next_billing_at should be now: got %v want %v", updated.NextBillingAt, now)
		}
	})

	t.Run("UpdateItem immediate cross-interval in_advance jumps period and bills synchronously", func(t *testing.T) {
		// Same-cadence cross-interval (yearly→monthly, both in_advance):
		// orchestrator refunds OLD unused, plan swaps, period jumps to
		// (now, NextBillingPeriodEnd for NEW monthly interval), and
		// BillOnCreate fires synchronously for the new in_advance bill.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_yearly_adv":  {ID: "p_yearly_adv", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 120000},
			"p_monthly_adv": {ID: "p_monthly_adv", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 12000},
		}})
		fb := &fakeBiller{}
		svc.SetBiller(fb)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_yearly_adv"}},
			BillingTime: domain.BillingTimeAnniversary,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Reset the bill counter — Create's day-1 invoice fires it once.
		fb.createTxCalls = 0
		fb.createTxOK = true
		res, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_monthly_adv",
			Immediate: true,
		})
		if err != nil {
			t.Fatalf("immediate cross-interval in_advance swap: %v", err)
		}
		if res.Item.PlanID != "p_monthly_adv" {
			t.Errorf("item plan after swap: got %q want p_monthly_adv", res.Item.PlanID)
		}
		if fb.swapDraftTxCalls != 1 {
			t.Errorf("expected 1 BillOnPlanSwapDraftsTx call, got %d", fb.swapDraftTxCalls)
		}
		if !fb.planSwapAt.Equal(now) {
			t.Errorf("refund at: got %v want %v", fb.planSwapAt, now)
		}
		if fb.swapBillCalls != 1 || fb.createTxCalls != 0 {
			t.Errorf("expected 1 in-tx BillOnPlanSwapTx call for the new in_advance period (and no BillOnCreateTx), got %d and %d", fb.swapBillCalls, fb.createTxCalls)
		}
		updated, err := svc.store.Get(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("get updated sub: %v", err)
		}
		if updated.CurrentBillingPeriodStart == nil || !updated.CurrentBillingPeriodStart.Equal(now) {
			t.Errorf("period_start should jump to now: got %v want %v", updated.CurrentBillingPeriodStart, now)
		}
		// NextBillingPeriodEnd for monthly anniversary is now+1 month.
		expectedEnd := now.AddDate(0, 1, 0)
		if updated.CurrentBillingPeriodEnd == nil || !updated.CurrentBillingPeriodEnd.Equal(expectedEnd) {
			t.Errorf("period_end should be 1 month from now: got %v want %v", updated.CurrentBillingPeriodEnd, expectedEnd)
		}
	})

	t.Run("UpdateItem allows plan-swap when bill_timing matches", func(t *testing.T) {
		// Same-timing swap (in_arrears $29 → in_arrears $49) is the
		// vanilla supported path. Must not regress.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_arrears_a": {ID: "p_arrears_a", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
			"p_arrears_b": {ID: "p_arrears_b", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears},
		}})
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items: []CreateItemInput{{PlanID: "p_arrears_a"}},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = svc.UpdateItem(ctx, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_arrears_b",
			Immediate: false,
		})
		if err != nil {
			t.Errorf("same-timing swap should succeed, got: %v", err)
		}
	})

	t.Run("Create (starts now) + yearly plan → first period is 1 year from now (no calendar stub)", func(t *testing.T) {
		// Yearly billing ignores billing_time — Stripe doesn't ship
		// calendar yearly either. Even with billing_time=calendar, a
		// yearly plan anchors anniversary-style.
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p": {ID: "p", BillingInterval: domain.BillingYearly},
		}})
		sub, _ := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p"}},
			BillingTime: domain.BillingTimeCalendar, // intentionally calendar
		})
		wantPeriodStart := time.Date(2025, 11, 29, 0, 0, 0, 0, time.UTC)
		wantPeriodEnd := time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC) // +1 YEAR (billing_time ignored)
		if !sub.CurrentBillingPeriodStart.Equal(wantPeriodStart) {
			t.Errorf("period_start: got %v, want %v", sub.CurrentBillingPeriodStart, wantPeriodStart)
		}
		if !sub.CurrentBillingPeriodEnd.Equal(wantPeriodEnd) {
			t.Errorf("period_end: got %v, want %v (yearly forces anniversary, ignores calendar)", sub.CurrentBillingPeriodEnd, wantPeriodEnd)
		}
	})
}

// TestUpdateItemTx_CrossIntervalAtomic covers the atomic cross-interval swap
// (ADR-056): UpdateItemTx restructures the cycle on the caller's tx (plan write
// + watermark advance + swap invoice via BillOnPlanSwapTx), defers the
// OLD-period refund + the new invoice's finalize to post-commit, and fails loud
// (so the tx rolls back) when the in-tx bill fails — closing the silent
// revenue-drop. Control flow is verified here with the in-memory store + fake
// biller (which ignore the nil tx); true rollback against real tx semantics is
// exercised by TestUpdateItemTx_CrossIntervalSwap_RealTxRollsBackOnBillFailure
// (real-Postgres, -short=false).
func TestUpdateItemTx_CrossIntervalAtomic(t *testing.T) {
	now := time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	newSvc := func(fb *fakeBiller) (*Service, domain.Subscription) {
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_yearly_adv":  {ID: "p_yearly_adv", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 120000},
			"p_monthly_adv": {ID: "p_monthly_adv", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 12000},
			"p_monthly_arr": {ID: "p_monthly_arr", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInArrears, BaseAmountCents: 2900},
		}})
		svc.SetBiller(fb)
		// The seed Create now bills the day-1 invoice IN-tx via BillOnCreateTx
		// (ADR-056). Temporarily neutralize any failure the subtest configured
		// for the SWAP so the seed create itself succeeds, then restore it.
		savedErr, savedOK := fb.createTxErr, fb.createTxOK
		fb.createTxErr, fb.createTxOK = nil, true
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_yearly_adv"}},
			BillingTime: domain.BillingTimeAnniversary,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		fb.createTxErr, fb.createTxOK = savedErr, savedOK
		fb.createTxCalls, fb.finalizeCalls, fb.swapDraftTxCalls, fb.finishCalls, fb.swapBillCalls = 0, 0, 0, 0, 0
		return svc, sub
	}

	t.Run("in_advance swap bills new period in-tx, defers refund + finalize", func(t *testing.T) {
		fb := &fakeBiller{createTxOK: true, createTxInv: domain.Invoice{ID: "hc_inv_swap"}}
		svc, sub := newSvc(fb)

		res, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_monthly_adv", Immediate: true,
		})
		if err != nil {
			t.Fatalf("UpdateItemTx cross-interval in_advance: %v", err)
		}
		if !res.OrchestratedCrossAxis {
			t.Error("expected OrchestratedCrossAxis=true")
		}
		if res.Item.PlanID != "p_monthly_adv" {
			t.Errorf("item plan: got %q want p_monthly_adv", res.Item.PlanID)
		}
		if fb.swapBillCalls != 1 || fb.createTxCalls != 0 {
			t.Errorf("BillOnPlanSwapTx (swap invoice in-tx) calls: got %d want 1; BillOnCreateTx got %d want 0", fb.swapBillCalls, fb.createTxCalls)
		}
		// before is the pre-swap snapshot (old plan, old period start): the
		// window [old start, now) the swap closes is priced from it. after is
		// the prospective sub on the new plan and period.
		if b := fb.swapBillBefore; len(b.Items) != 1 || b.Items[0].PlanID != "p_yearly_adv" || b.CurrentBillingPeriodStart == nil || !b.CurrentBillingPeriodStart.Equal(*sub.CurrentBillingPeriodStart) {
			t.Errorf("BillOnPlanSwapTx before = items %+v period start %v, want the old plan p_yearly_adv from %v", b.Items, b.CurrentBillingPeriodStart, *sub.CurrentBillingPeriodStart)
		}
		if a := fb.swapBillAfter; len(a.Items) != 1 || a.Items[0].PlanID != "p_monthly_adv" || a.CurrentBillingPeriodStart == nil || !a.CurrentBillingPeriodStart.Equal(now) {
			t.Errorf("BillOnPlanSwapTx after = items %+v period start %v, want the new plan p_monthly_adv from %v", a.Items, a.CurrentBillingPeriodStart, now)
		}
		if !fb.swapBillAt.Equal(now) {
			t.Errorf("BillOnPlanSwapTx at = %v, want %v", fb.swapBillAt, now)
		}
		if fb.swapAfterCommitCalls != 0 {
			t.Errorf("the swap bill's afterCommit ran %d times before commit, want 0", fb.swapAfterCommitCalls)
		}
		if res.crossAxisNewInvoice == nil || res.crossAxisNewInvoice.ID != "hc_inv_swap" {
			t.Errorf("crossAxisNewInvoice should carry the new invoice for post-commit finalize; got %+v", res.crossAxisNewInvoice)
		}
		// The refund is WRITTEN in the tx; its legs and the finalize run
		// post-commit.
		if fb.swapDraftTxCalls != 1 {
			t.Errorf("refund must be written in the swap tx; BillOnPlanSwapDraftsTx ran %d times", fb.swapDraftTxCalls)
		}
		if fb.finishCalls != 0 {
			t.Errorf("relief legs must be deferred to post-commit; FinishRelief ran %d times in-tx", fb.finishCalls)
		}
		if fb.finalizeCalls != 0 {
			t.Errorf("finalize must be deferred to post-commit; FinalizeOnCreateInvoice ran %d times in-tx", fb.finalizeCalls)
		}
		updated, _ := svc.store.Get(ctx, "t1", sub.ID)
		if updated.CurrentBillingPeriodStart == nil || !updated.CurrentBillingPeriodStart.Equal(now) {
			t.Errorf("period_start should jump to now: got %v", updated.CurrentBillingPeriodStart)
		}
		if expEnd := now.AddDate(0, 1, 0); updated.CurrentBillingPeriodEnd == nil || !updated.CurrentBillingPeriodEnd.Equal(expEnd) {
			t.Errorf("period_end: got %v want %v", updated.CurrentBillingPeriodEnd, expEnd)
		}
		// The handler finalizes after commit; only then does afterCommit run.
		svc.FinalizeCrossIntervalSwap(ctx, "t1", sub, res)
		if fb.swapAfterCommitCalls != 1 {
			t.Errorf("FinalizeCrossIntervalSwap ran the swap bill's afterCommit %d times, want 1", fb.swapAfterCommitCalls)
		}
	})

	t.Run("in-tx bill failure returns error (drives the handler tx rollback)", func(t *testing.T) {
		fb := &fakeBiller{createTxErr: fmt.Errorf("tax provider down")}
		svc, sub := newSvc(fb)

		if _, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_monthly_adv", Immediate: true,
		}); err == nil {
			t.Fatal("expected error when the in-tx new-period bill fails (so the tx rolls back the whole swap)")
		}
		if fb.swapBillCalls != 1 {
			t.Errorf("BillOnPlanSwapTx calls: got %d want 1", fb.swapBillCalls)
		}
		if fb.finishCalls != 0 || fb.finalizeCalls != 0 || fb.swapAfterCommitCalls != 0 {
			t.Errorf("post-commit steps must not run on in-tx failure; finish=%d finalize=%d afterCommit=%d", fb.finishCalls, fb.finalizeCalls, fb.swapAfterCommitCalls)
		}
	})

	t.Run("cross-cadence swap rejected", func(t *testing.T) {
		fb := &fakeBiller{}
		svc, sub := newSvc(fb) // current item is in_advance
		if _, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_monthly_arr", Immediate: true, // in_arrears — cross-cadence
		}); err == nil {
			t.Fatal("expected cross-cadence (in_advance↔in_arrears) swap to be rejected")
		}
	})

	t.Run("multi-item swap that would mix intervals is rejected before any restructure", func(t *testing.T) {
		// Regression for the review's M1: the atomic path must keep the
		// mixed-interval guard the non-atomic path enforces. A uniform 2-item
		// monthly sub, swap one item monthly→yearly: the OTHER item stays
		// monthly, so the post-swap set mixes intervals and must be rejected —
		// otherwise the cross-interval re-anchor would bill the unchanged item a
		// full base over a yearly period (overcharge).
		fb := &fakeBiller{}
		svc := NewService(newMemStore(), clock.NewFake(now))
		svc.SetPlanReader(&fakePlanReader{plans: map[string]domain.Plan{
			"p_m1": {ID: "p_m1", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 1000},
			"p_m2": {ID: "p_m2", BillingInterval: domain.BillingMonthly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 2000},
			"p_y":  {ID: "p_y", BillingInterval: domain.BillingYearly, BaseBillTiming: domain.BillInAdvance, BaseAmountCents: 100000},
		}})
		svc.SetBiller(fb)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "s", DisplayName: "n", CustomerID: "c",
			Items:       []CreateItemInput{{PlanID: "p_m1"}, {PlanID: "p_m2"}},
			BillingTime: domain.BillingTimeAnniversary,
		})
		if err != nil {
			t.Fatalf("Create multi-item: %v", err)
		}
		if len(sub.Items) != 2 {
			t.Fatalf("expected 2 items, got %d", len(sub.Items))
		}
		fb.createTxCalls = 0 // ignore the seed create's in-tx day-1 bill; assert only the swap
		if _, err := svc.UpdateItemTx(ctx, nil, "t1", sub.ID, sub.Items[0].ID, UpdateItemInput{
			NewPlanID: "p_y", Immediate: true,
		}); err == nil {
			t.Fatal("expected mixed-interval swap to be rejected")
		}
		if fb.createTxCalls != 0 || fb.swapBillCalls != 0 {
			t.Errorf("rejected swap must not bill a new period; BillOnCreateTx called %d, BillOnPlanSwapTx %d", fb.createTxCalls, fb.swapBillCalls)
		}
		if it, _ := svc.store.GetItem(ctx, "t1", sub.Items[0].ID); it.PlanID != "p_m1" {
			t.Errorf("item must be unchanged after rejection; got %q", it.PlanID)
		}
	})

	t.Run("FinalizeCrossIntervalSwap runs refund + finalize post-commit", func(t *testing.T) {
		fb := &fakeBiller{}
		svc, sub := newSvc(fb)
		subBefore, _ := svc.store.Get(ctx, "t1", sub.ID)
		svc.FinalizeCrossIntervalSwap(ctx, "t1", subBefore, ItemChangeResult{
			EffectiveAt:         now,
			crossAxisNewInvoice: &domain.Invoice{ID: "hc_inv_swap"},
			swapRelief:          domain.Relief{DraftIDs: []string{"cn_swap"}},
		})
		if fb.finishCalls != 1 || len(fb.finishedDrafts) != 1 || fb.finishedDrafts[0] != "cn_swap" {
			t.Errorf("refund legs: FinishRelief calls %d drafts %v, want 1 with cn_swap", fb.finishCalls, fb.finishedDrafts)
		}
		if fb.finalizeCalls != 1 {
			t.Errorf("FinalizeOnCreateInvoice calls got %d want 1", fb.finalizeCalls)
		}
	})
}

// TestSetBillingThresholds covers the validation paths the service applies on
// PATCH. The Postgres store handles the actual write; integration tests
// exercise that path. These unit tests are the merge gate for the hot config
// rules — drop a check here and the API would happily accept e.g. duplicate
// item ids, which would later surface as a mid-tx integrity error.
func TestSetBillingThresholds(t *testing.T) {
	ctx := context.Background()

	newSubFixture := func(t *testing.T) (*Service, domain.Subscription) {
		t.Helper()
		svc := NewService(newMemStore(), nil)
		sub, err := svc.Create(ctx, "t1", CreateInput{
			Code: "sub-thresh", DisplayName: "Test", CustomerID: "c",
			Items: []CreateItemInput{
				{PlanID: "plan_base"},
				{PlanID: "plan_addon"},
			},
		})
		if err != nil {
			t.Fatalf("create sub: %v", err)
		}
		return svc, sub
	}

	t.Run("rejects empty body", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{})
		if err == nil {
			t.Fatal("expected error: empty body must be rejected, not silently no-op")
		}
	})

	t.Run("rejects negative amount_gte", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE: -1,
		})
		if err == nil {
			t.Fatal("expected error for negative amount_gte")
		}
	})

	t.Run("rejects unknown subscription", func(t *testing.T) {
		svc, _ := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", "hc_sub_does_not_exist", BillingThresholdsInput{
			AmountGTE: 500000,
		})
		if err == nil {
			t.Fatal("expected ErrNotFound for unknown subscription")
		}
	})

	t.Run("rejects on canceled sub", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		// Cancel the subscription so we can verify the terminal-state guard.
		if _, _, err := svc.Cancel(ctx, "t1", sub.ID); err != nil {
			t.Fatalf("setup cancel: %v", err)
		}
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE: 500000,
		})
		if err == nil {
			t.Fatal("expected error setting threshold on canceled sub")
		}
	})

	t.Run("rejects item_thresholds with empty subscription_item_id", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: "", UsageGTE: "1000"},
			},
		})
		if err == nil {
			t.Fatal("expected error for empty subscription_item_id")
		}
	})

	t.Run("rejects duplicate subscription_item_id", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		itemID := fresh.Items[0].ID
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: itemID, UsageGTE: "1000"},
				{SubscriptionItemID: itemID, UsageGTE: "2000"},
			},
		})
		if err == nil {
			t.Fatal("expected error for duplicate subscription_item_id")
		}
	})

	t.Run("rejects subscription_item_id not on this subscription", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: "hc_subitem_foreign", UsageGTE: "1000"},
			},
		})
		if err == nil {
			t.Fatal("expected error for foreign subscription_item_id")
		}
	})

	t.Run("rejects non-numeric usage_gte", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: fresh.Items[0].ID, UsageGTE: "not-a-number"},
			},
		})
		if err == nil {
			t.Fatal("expected error for non-numeric usage_gte")
		}
	})

	t.Run("rejects negative usage_gte", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		_, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: fresh.Items[0].ID, UsageGTE: "-1"},
			},
		})
		if err == nil {
			t.Fatal("expected error for negative usage_gte")
		}
	})

	t.Run("accepts amount-only configuration", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		updated, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE: 500000,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated.BillingThresholds == nil {
			t.Fatal("expected BillingThresholds set on updated sub")
		}
		if updated.BillingThresholds.AmountGTE != 500000 {
			t.Errorf("amount_gte: got %d, want 500000", updated.BillingThresholds.AmountGTE)
		}
	})

	t.Run("accepts item-only configuration", func(t *testing.T) {
		svc, sub := newSubFixture(t)
		fresh, _ := svc.Get(ctx, "t1", sub.ID)
		updated, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			ItemThresholds: []ItemThresholdInput{
				{SubscriptionItemID: fresh.Items[0].ID, UsageGTE: "1000.5"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if updated.BillingThresholds == nil {
			t.Fatal("expected BillingThresholds set on updated sub")
		}
		if len(updated.BillingThresholds.ItemThresholds) != 1 {
			t.Fatalf("expected 1 item threshold, got %d", len(updated.BillingThresholds.ItemThresholds))
		}
	})
}

// TestClearBillingThresholds is idempotent and unconditional — the service
// delegates straight to the store. We just verify the round-trip wires
// through correctly and removing a never-set threshold is not an error.
func TestClearBillingThresholds(t *testing.T) {
	ctx := context.Background()
	svc := NewService(newMemStore(), nil)

	sub, err := svc.Create(ctx, "t1", CreateInput{
		Code: "sub-clear", DisplayName: "Test", CustomerID: "c",
		Items: []CreateItemInput{{PlanID: "plan_base"}},
	})
	if err != nil {
		t.Fatalf("create sub: %v", err)
	}

	t.Run("idempotent on never-set", func(t *testing.T) {
		if _, err := svc.ClearBillingThresholds(ctx, "t1", sub.ID); err != nil {
			t.Errorf("clearing never-set threshold should be a no-op, got %v", err)
		}
	})

	t.Run("clears after set", func(t *testing.T) {
		if _, err := svc.SetBillingThresholds(ctx, "t1", sub.ID, BillingThresholdsInput{
			AmountGTE: 500000,
		}); err != nil {
			t.Fatalf("set: %v", err)
		}
		updated, err := svc.ClearBillingThresholds(ctx, "t1", sub.ID)
		if err != nil {
			t.Fatalf("clear: %v", err)
		}
		if updated.BillingThresholds != nil {
			t.Errorf("BillingThresholds should be nil after clear, got %+v", updated.BillingThresholds)
		}
	})
}

// TestCreate_WithNoInstantStampsWallClock: with no instant bound, Create
// stamps wall-clock time.
func TestCreate_WithNoInstantStampsWallClock(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil)

	before := time.Now().UTC()
	sub, err := svc.Create(context.Background(), "t1", CreateInput{
		Code:        "sub-fallback",
		DisplayName: "Fallback",
		CustomerID:  "cus_fallback",
		Items:       []CreateItemInput{{PlanID: "pln_1"}},
	})
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sub.StartedAt == nil {
		t.Fatal("started_at should be set")
	}
	if sub.StartedAt.Before(before.Add(-1*time.Second)) || sub.StartedAt.After(after.Add(1*time.Second)) {
		t.Errorf("started_at: got %v, want between %v and %v (wall-clock fallback)",
			*sub.StartedAt, before, after)
	}
}

// TestSubMutators_StampTheDecidedTime enumerates every public Service
// mutator that touches a subscription row, and asserts each stamps its
// writes at the request's instant: the API binds it, and the
// mutator's writes (updated_at, canceled_at) read it. Failure means a
// mutator writes on a time of its own. Add a new mutator to the table below.
func TestSubMutators_StampTheDecidedTime(t *testing.T) {
	frozen := time.Date(2024, 4, 15, 12, 0, 0, 0, time.UTC)

	// decided is the request's time, as the API binds it.
	decided := clock.WithEffectiveNow(postgres.WithLivemode(context.Background(), false), frozen)
	newPinned := func(t *testing.T, status domain.SubscriptionStatus) (*Service, domain.Subscription) {
		t.Helper()
		store := newMemStore()
		svc := NewService(store, nil)
		// Seed a sub directly via the store so we sidestep Create's
		// own (correctly-bound) path — the goal is to exercise the
		// post-create mutators in isolation.
		sub, err := store.CreateWithBill(context.Background(), "t1", domain.Subscription{
			Code:        "sub-pinned",
			DisplayName: "Pinned",
			CustomerID:  "cus_pinned",
			Status:      status,
			Items: []domain.SubscriptionItem{
				{PlanID: "pln_1", Quantity: 1},
			},
		}, nil)
		if err != nil {
			t.Fatalf("seed sub: %v", err)
		}
		return svc, sub
	}

	// Every contract case names the mutator, the precondition status,
	// and runs the action. The assertion is uniform: UpdatedAt (or
	// CanceledAt) on the returned domain object must equal frozen.
	t.Run("Cancel", func(t *testing.T) {
		svc, sub := newPinned(t, domain.SubscriptionActive)
		out, _, err := svc.Cancel(decided, "t1", sub.ID)
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if out.CanceledAt == nil || !out.CanceledAt.Equal(frozen) {
			t.Errorf("canceled_at: got %v, want %v (frozen)", out.CanceledAt, frozen)
		}
		if !out.UpdatedAt.Equal(frozen) {
			t.Errorf("updated_at: got %v, want %v (frozen)", out.UpdatedAt, frozen)
		}
	})

	t.Run("AddItem", func(t *testing.T) {
		svc, sub := newPinned(t, domain.SubscriptionActive)
		item, err := svc.AddItem(decided, "t1", sub.ID, AddItemInput{PlanID: "pln_2", Quantity: 1})
		if err != nil {
			t.Fatalf("AddItem: %v", err)
		}
		if !item.CreatedAt.Equal(frozen) {
			t.Errorf("created_at: got %v, want %v (frozen)", item.CreatedAt, frozen)
		}
	})

	t.Run("CancelPendingItemChange", func(t *testing.T) {
		svc, sub := newPinned(t, domain.SubscriptionActive)
		// Schedule a plan change so there's something to cancel.
		_, err := svc.store.SetItemPendingPlanTx(context.Background(), nil, "t1", sub.Items[0].ID, "pln_2", frozen.Add(72*time.Hour))
		if err != nil {
			t.Fatalf("seed pending plan: %v", err)
		}
		item, _, err := svc.CancelPendingItemChange(decided, "t1", sub.ID, sub.Items[0].ID)
		if err != nil {
			t.Fatalf("CancelPendingItemChange: %v", err)
		}
		if !item.UpdatedAt.Equal(frozen) {
			t.Errorf("updated_at: got %v, want %v (frozen)", item.UpdatedAt, frozen)
		}
	})
}

// captureAudit lets the test inspect what Service writes when pause /
// resume auto-clears via the new scan phases.
type captureAudit struct {
	entries []capturedAuditEntry
}

// LogInTx captures like Log (nil tx fine for content-level unit tests).
func (c *captureAudit) LogInTx(_ context.Context, _ *sql.Tx, e audit.Entry) error {
	c.entries = append(c.entries, capturedAuditEntry{
		action: e.Action, resourceID: e.ResourceID, resourceType: e.ResourceType, metadata: e.Metadata,
	})
	return nil
}

type capturedAuditEntry struct {
	action       string
	resourceID   string
	metadata     map[string]any
	resourceType string
}

// Log captures the row's content.
func (c *captureAudit) Log(_ context.Context, _, action, resourceType, resourceID, _ string, metadata map[string]any) error {
	c.entries = append(c.entries, capturedAuditEntry{
		action: action, resourceType: resourceType, resourceID: resourceID, metadata: metadata,
	})
	return nil
}

// TestProcessExpiredPauseCollections covers the new wall-clock scan
// that replaces the engine's in-cycle auto-resume gate. The gate
// only fired when a cycle was due; the scan runs every scheduler
// tick and is the Stripe-parity "resume AT resumes_at" mechanism.
func TestProcessExpiredPauseCollections(t *testing.T) {
	t.Run("resumes wall-clock subs whose resumes_at has passed", func(t *testing.T) {
		now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
		mem := newMemStore()
		svc := NewService(mem, clock.NewFake(now))
		audit := &captureAudit{}
		svc.SetAuditLogger(audit)

		// One sub paused with resumes_at in the past — should resume.
		// One sub paused with resumes_at in the future — should NOT.
		// One sub paused with no resumes_at — should NOT (indefinite).
		// Past-resumes_at is set by poking the store directly: the
		// Service.PauseCollection contract rejects past timestamps
		// (operator should never queue an already-elapsed resume), so
		// we simulate state that arose from time passing a
		// previously-future resumes_at.
		past := now.Add(-24 * time.Hour)
		future := now.Add(24 * time.Hour)
		ctx := context.Background()
		mkSub := func(code string, pc *domain.PauseCollection) string {
			s, _ := svc.Create(ctx, "t1", CreateInput{Code: code, DisplayName: code, CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}})
			if pc != nil {
				row := mem.subs[s.ID]
				row.PauseCollection = pc
				mem.subs[s.ID] = row
			}
			return s.ID
		}
		dueID := mkSub("due", &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &past})
		futureID := mkSub("future", &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &future})
		indefiniteID := mkSub("indef", &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft})

		audit.entries = nil // discard setup pauses
		processed, errs := svc.ProcessExpiredPauseCollections(ctx, 50)
		if len(errs) > 0 {
			t.Fatalf("unexpected errors: %v", errs)
		}
		if processed != 1 {
			t.Errorf("processed: got %d, want 1 (only the past-resumes_at sub)", processed)
		}

		got, _ := svc.Get(ctx, "t1", dueID)
		if got.PauseCollection != nil {
			t.Errorf("due sub should be resumed, got pause %+v", got.PauseCollection)
		}
		stillPaused, _ := svc.Get(ctx, "t1", futureID)
		if stillPaused.PauseCollection == nil {
			t.Error("future-resumes_at sub should still be paused")
		}
		indef, _ := svc.Get(ctx, "t1", indefiniteID)
		if indef.PauseCollection == nil {
			t.Error("indefinite-pause sub should still be paused")
		}

		// Audit row written with triggered_by=schedule.
		if len(audit.entries) != 1 {
			t.Fatalf("audit entries: got %d, want 1", len(audit.entries))
		}
		e := audit.entries[0]
		if e.metadata["action"] != "collection_resumed" {
			t.Errorf("audit action: got %v, want collection_resumed", e.metadata["action"])
		}
		if e.metadata["triggered_by"] != "schedule" {
			t.Errorf("audit triggered_by: got %v, want schedule", e.metadata["triggered_by"])
		}
		// Regression (FLOW TC9 live find): resumed_at is the CONTRACTED
		// resumes_at instant, not the tick time that happened to sweep
		// it — same contractual-instant split as ADR-097 cancels.
		if got, ok := e.metadata["resumed_at"].(time.Time); !ok || !got.Equal(past) {
			t.Errorf("resumed_at: got %v, want the contracted resumes_at %v (not scan-time now)", e.metadata["resumed_at"], past)
		}
	})
}

// CustomerSubPlanCurrencies (ADR-100 pin) — the fake has no plan table, so
// it reports no cross-sub currencies; the pin's cross-sub clause is covered
// by the real-Postgres integration tests.
func (m *memStore) CustomerSubPlanCurrencies(_ context.Context, _, _, _ string) ([]SubPlanCurrency, error) {
	return nil, nil
}

// The pause scan resumes each subscription on its own instant: the
// tick binds none, so a row reads the server's clock once.
func TestProcessExpiredPauseCollections_EachSubscriptionOnItsOwnInstant(t *testing.T) {
	createdAt := time.Date(2025, 11, 29, 12, 0, 0, 0, time.UTC)
	ctx := postgres.WithLivemode(context.Background(), true)
	svc := NewService(newMemStore(), clock.NewFake(createdAt))
	mem := svc.store.(*memStore)
	resumes := createdAt.Add(24 * time.Hour)
	var ids []string
	for _, code := range []string{"sub-pause-1", "sub-pause-2"} {
		sub, err := svc.Create(ctx, "t1", CreateInput{Code: code, DisplayName: "Paused", CustomerID: "c", Items: []CreateItemInput{{PlanID: "p"}}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		paused := mem.subs[sub.ID]
		paused.PauseCollection = &domain.PauseCollection{Behavior: domain.PauseCollectionKeepAsDraft, ResumesAt: &resumes, SetBy: domain.PauseSetByOperator}
		mem.subs[sub.ID] = paused
		ids = append(ids, sub.ID)
	}

	server := &steppingClock{at: resumes.Add(time.Hour)}
	if n, errs := NewService(mem, server).ProcessExpiredPauseCollections(ctx, 100); n != 2 || len(errs) != 0 {
		t.Fatalf("resumed %d, errors %v; want 2", n, errs)
	}
	first, second := mem.subs[ids[0]].UpdatedAt, mem.subs[ids[1]].UpdatedAt
	for _, at := range []time.Time{first, second} {
		if !slices.Contains(server.issued, at) || !at.After(server.issued[0]) {
			t.Errorf("resumed at %v: want a read of the server's clock after the list's %v", at, server.issued[0])
		}
	}
	if first.Equal(second) {
		t.Errorf("both resumed at %v: want each subscription on its own read", first)
	}
}

// TestProcessExpiredPauseCollections_AlreadyClearedAnnouncesNothing pins the
// hazard-5 root fix at the service seam: when the store reports the pause was
// already cleared (a sibling leader won the CAS), the cron counts nothing and
// reports no error. The event and the audit row are the store's, written only
// when its clear wins (TestClearPauseCollection_CASOneWinner). Mutation-verify:
// remove the ErrNotPaused branch in resumeListedPause → the batch reports an
// error.
func TestProcessExpiredPauseCollections_AlreadyClearedAnnouncesNothing(t *testing.T) {
	store := newMemStore()
	resumes := time.Now().Add(-time.Hour)
	store.subs["sub_paused"] = domain.Subscription{ID: "sub_paused", TenantID: "t1", CustomerID: "cus_1", Status: domain.SubscriptionActive,
		PauseCollection: &domain.PauseCollection{Behavior: "keep_as_draft", ResumesAt: &resumes}}
	store.clearPauseOverride = ErrNotPaused // the sibling got there first
	svc := NewService(store, nil)
	audit := &captureAudit{}
	svc.SetAuditLogger(audit)

	processed, errsOut := svc.ProcessExpiredPauseCollections(context.Background(), 10)
	if len(errsOut) != 0 {
		t.Fatalf("an already-cleared pause is not an error: %v", errsOut)
	}
	if processed != 0 {
		t.Fatalf("processed=%d, want 0 — nothing was resumed by this leader", processed)
	}
	if len(audit.entries) != 0 {
		t.Fatalf("wrote %d audit rows, want 0", len(audit.entries))
	}
}
