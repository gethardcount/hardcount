package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/subscription"
	"github.com/gethardcount/hardcount/internal/tax"
)

// changingTaxResolver serves the tenant's tax provider, wrapped so that each
// Calculate first runs change, while change is set.
type changingTaxResolver struct {
	inner *tax.Resolver
	mu    sync.Mutex
	// change runs inside each Calculate with the call's context; nil runs
	// nothing.
	change func(ctx context.Context)
	calls  int
}

func (c *changingTaxResolver) Resolve(ctx context.Context, ts domain.TenantSettings) (tax.Provider, error) {
	prov, err := c.inner.Resolve(ctx, ts)
	if err != nil {
		return nil, err
	}
	return changingProvider{Provider: prov, c: c}, nil
}

func (c *changingTaxResolver) set(change func(ctx context.Context)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.change, c.calls = change, 0
}

type changingProvider struct {
	tax.Provider
	c *changingTaxResolver
}

func (p changingProvider) Calculate(ctx context.Context, req tax.Request) (*tax.Result, error) {
	p.c.mu.Lock()
	p.c.calls++
	change := p.c.change
	p.c.mu.Unlock()
	if change != nil {
		change(ctx)
	}
	return p.Provider.Calculate(ctx, req)
}

// TestCancel_SnapshotMovedTwice_Answers409: an immediate cancel through the
// API whose subscription changes while each of its two plans is priced
// (ADR-139). Each change commits while the cancel waits on the tax call, so
// each transaction finds the items moved and writes nothing. After the second
// the cancel answers 409 concurrent_change: the subscription is not canceled,
// and the cancel wrote no invoice, credit note, subscription.canceled or
// cancel audit row. The idempotency layer does not store the 409, and the same
// request with the same Idempotency-Key then cancels.
//
// Before, the tax call ran inside the cancel's transaction: each change waited
// for the cancel's row lock and timed out, and the cancel committed.
//
// Mutation: drop the ErrWatermarkMoved mapping in the cancel handler → a 500,
// which the idempotency layer stores and replays.
func TestCancel_SnapshotMovedTwice_Answers409(t *testing.T) {
	r := newLagRig(t, "in_arrears", false)
	cancelAt := lagP0.Add(15 * 24 * time.Hour)
	const key = "idem-e2e-cancel-moved-twice"
	path := "/v1/subscriptions/" + r.subID + "/cancel"

	resolver := &changingTaxResolver{inner: tax.NewResolver(nil)}
	r.srv.BillingEngine.SetTaxProviderResolver(resolver)
	var mu sync.Mutex
	var changeErrs []error
	qty := int64(1)
	resolver.set(func(ctx context.Context) {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()
		qty++
		q := qty
		if _, err := r.srv.SubscriptionSvc.UpdateItem(cctx, r.tenantID, r.subID, r.itemA, subscription.UpdateItemInput{Quantity: &q, Immediate: true}); err != nil {
			mu.Lock()
			changeErrs = append(changeErrs, fmt.Errorf("quantity to %d while the cancel was priced: %w (it waited on the cancel's lock)", q, err))
			mu.Unlock()
		}
	})

	first := r.doKey(http.MethodPost, path, key, cancelAt, nil)
	mu.Lock()
	for _, err := range changeErrs {
		t.Error(err)
	}
	mu.Unlock()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(first.Body.Bytes(), &env)
	if first.Code != http.StatusConflict || env.Error.Code != "concurrent_change" || first.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("cancel whose subscription moved twice: %d %s, want 409 concurrent_change from the handler", first.Code, first.Body.String())
	}
	if resolver.calls != 2 {
		t.Errorf("tax calculations = %d, want 2: one per plan", resolver.calls)
	}
	if s := r.sub(); s.Status != domain.SubscriptionActive {
		t.Fatalf("after the 409: subscription %s, want active", s.Status)
	}
	wrote := map[string]int{
		"final invoices": flowCount(t, r.db, `SELECT count(*) FROM invoices
			WHERE subscription_id = $1 AND billing_reason = $2`, r.subID, domain.BillingReasonSubscriptionCancel),
		"credit notes": flowCount(t, r.db, `SELECT count(*) FROM credit_notes WHERE tenant_id = $1`, r.tenantID),
		"subscription.canceled events": flowCount(t, r.db, `SELECT count(*) FROM webhook_outbox
			WHERE event_type = $1 AND payload->'subscription'->>'id' = $2`, domain.EventSubscriptionCanceled, r.subID),
		"cancel audit rows": flowCount(t, r.db, `SELECT count(*) FROM audit_log
			WHERE action = $1 AND resource_id = $2`, domain.AuditActionCancel, r.subID),
	}
	for what, n := range wrote {
		if n != 0 {
			t.Errorf("after the 409: %d %s, want 0", n, what)
		}
	}

	resolver.set(nil)
	again := r.doKey(http.MethodPost, path, key, cancelAt, nil)
	if again.Code != http.StatusOK || again.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("resend with the same key: %d (replayed=%q) %s, want 200 from the handler",
			again.Code, again.Header().Get("Idempotent-Replayed"), again.Body.String())
	}
	if s := r.sub(); s.Status != domain.SubscriptionCanceled {
		t.Fatalf("after the resend: subscription %s, want canceled", s.Status)
	}
	if n := flowCount(t, r.db, `SELECT count(*) FROM invoices WHERE subscription_id = $1 AND billing_reason = $2`,
		r.subID, domain.BillingReasonSubscriptionCancel); n != 1 {
		t.Errorf("final invoices after the resend = %d, want 1", n)
	}
}
