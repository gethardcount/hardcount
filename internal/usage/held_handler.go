package usage

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/subscription"
)

// PlanReader reads a plan by id. Implemented by *pricing.Service.
type PlanReader interface {
	GetPlan(ctx context.Context, tenantID, id string) (domain.Plan, error)
}

// SetHeldDeps wires what the held-usage routes read besides the usage
// store: a customer by id (to check a release target), the customer's
// subscriptions and their plans (to say which released rows an open billing
// period's invoice charges). Wired in router.go with the customer and
// subscription stores and the pricing service.
func (h *Handler) SetHeldDeps(customers CustomerLookup, subs SubscriptionLister, plans PlanReader) {
	h.heldCustomers = customers
	h.heldSubs = subs
	h.heldPlans = plans
}

// listHeld serves GET /v1/usage-events/held: the rows still held, grouped
// by (external customer id, meter key, reason), with whether a matching
// customer and meter exist now so the dashboard can offer Release.
func (h *Handler) listHeld(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	summary, err := h.svc.ListHeld(r.Context(), tenantID)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "list held usage", "error", err)
		return
	}
	groups := summary.Groups
	custIDs := map[string]string{}
	meterOK := map[string]bool{}
	for i := range groups {
		g := &groups[i]
		id, ok := custIDs[g.ExternalCustomerID]
		if !ok {
			if c, err := h.customers.GetByExternalID(r.Context(), tenantID, g.ExternalCustomerID); err == nil {
				id = c.ID
			} else if !errors.Is(err, errs.ErrNotFound) {
				respond.InternalError(w, r)
				slog.ErrorContext(r.Context(), "list held usage: customer lookup", "error", err)
				return
			}
			custIDs[g.ExternalCustomerID] = id
		}
		g.CustomerID = id
		exists, ok := meterOK[g.MeterKey]
		if !ok {
			if _, err := h.meters.GetMeterByKey(r.Context(), tenantID, g.MeterKey); err == nil {
				exists = true
			} else if !errors.Is(err, errs.ErrNotFound) {
				respond.InternalError(w, r)
				slog.ErrorContext(r.Context(), "list held usage: meter lookup", "error", err)
				return
			}
			meterOK[g.MeterKey] = exists
		}
		g.MeterExists = exists
	}
	// data lists at most HeldListCustomers external ids; the totals count
	// them all, so the dashboard can say when the list is cut.
	respond.JSON(w, r, http.StatusOK, map[string]any{
		"data":            groups,
		"total_customers": summary.TotalCustomers,
		"total_rows":      summary.TotalRows,
	})
}

type releaseHeldRequest struct {
	ExternalCustomerID string `json:"external_customer_id"`
	// CustomerID releases into a customer whose external_id differs (the
	// LiteLLM `user` value had a typo). Empty means the customer whose
	// external_id equals ExternalCustomerID.
	CustomerID string `json:"customer_id"`
}

// releaseHeld serves POST /v1/usage-events/held/release.
func (h *Handler) releaseHeld(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	var req releaseHeldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	ext := strings.TrimSpace(req.ExternalCustomerID)
	if ext == "" {
		respond.ValidationField(w, r, "external_customer_id", "external_customer_id is required")
		return
	}

	customerID := strings.TrimSpace(req.CustomerID)
	if customerID == "" {
		c, err := h.customers.GetByExternalID(r.Context(), tenantID, ext)
		if errors.Is(err, errs.ErrNotFound) {
			respond.ValidationField(w, r, "external_customer_id",
				"no customer has external ID \""+ext+"\" yet — create it, or pass customer_id to release into another customer")
			return
		}
		if err != nil {
			respond.InternalError(w, r)
			slog.ErrorContext(r.Context(), "release held usage: customer lookup", "error", err)
			return
		}
		customerID = c.ID
	} else if h.heldCustomers != nil {
		if _, err := h.heldCustomers.Get(r.Context(), tenantID, customerID); err != nil {
			if errors.Is(err, errs.ErrNotFound) {
				respond.ValidationField(w, r, "customer_id", "customer not found")
				return
			}
			respond.InternalError(w, r)
			slog.ErrorContext(r.Context(), "release held usage: customer get", "error", err)
			return
		}
	} else {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "release held usage: customer lookup not wired")
		return
	}

	keys, heldRows, err := h.svc.HeldFor(r.Context(), tenantID, ext)
	if err != nil {
		respond.InternalError(w, r)
		slog.ErrorContext(r.Context(), "release held usage: held keys", "error", err)
		return
	}
	meterIDs := map[string]string{}
	for _, k := range keys {
		m, err := h.meters.GetMeterByKey(r.Context(), tenantID, k)
		if errors.Is(err, errs.ErrNotFound) {
			continue // its rows stay held until the meter exists
		}
		if err != nil {
			respond.InternalError(w, r)
			slog.ErrorContext(r.Context(), "release held usage: meter lookup", "error", err)
			return
		}
		meterIDs[k] = m.ID
	}

	var subs []domain.Subscription
	plans := map[string]domain.Plan{}
	if h.heldSubs != nil {
		subs, _, err = h.heldSubs.List(r.Context(), subscription.ListFilter{TenantID: tenantID, CustomerID: customerID, Limit: 100})
		if err != nil {
			respond.InternalError(w, r)
			slog.ErrorContext(r.Context(), "release held usage: list subscriptions", "error", err)
			return
		}
	}
	// The plans say which meters each subscription prices: a released row
	// whose meter its subscription does not price is not invoiced.
	if h.heldPlans != nil {
		for _, sub := range subs {
			for _, it := range sub.Items {
				if _, ok := plans[it.PlanID]; ok || it.PlanID == "" {
					continue
				}
				p, err := h.heldPlans.GetPlan(r.Context(), tenantID, it.PlanID)
				if err != nil {
					respond.InternalError(w, r)
					slog.ErrorContext(r.Context(), "release held usage: get plan", "error", err)
					return
				}
				plans[it.PlanID] = p
			}
		}
	}

	// Nothing held, or every held meter still missing: nothing to release.
	res := ReleaseResult{CustomerID: customerID, StillHeld: heldRows}
	if len(meterIDs) > 0 {
		res, err = h.svc.Release(r.Context(), tenantID, ReleaseInput{
			ExternalCustomerID: ext,
			CustomerID:         customerID,
			MeterIDs:           meterIDs,
		}, subs, plans)
		if err != nil {
			respond.FromError(w, r, err, "usage_event")
			return
		}
	}
	if res.Released+res.Deduplicated == 0 {
		// No row changed state, so no audit row was written: say so to the
		// coverage detector.
		audit.MarkSkip(r.Context())
	}
	respond.JSON(w, r, http.StatusOK, res)
}

type discardHeldRequest struct {
	ExternalCustomerID string `json:"external_customer_id"`
	// MeterKey limits the discard to one meter. Empty discards every held
	// row for the external id.
	MeterKey string `json:"meter_key"`
}

// discardHeld serves POST /v1/usage-events/held/discard.
func (h *Handler) discardHeld(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	var req discardHeldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respond.BadRequest(w, r, "invalid JSON body")
		return
	}
	n, err := h.svc.Discard(r.Context(), tenantID, strings.TrimSpace(req.ExternalCustomerID), strings.TrimSpace(req.MeterKey))
	if err != nil {
		respond.FromError(w, r, err, "usage_event")
		return
	}
	if n == 0 {
		audit.MarkSkip(r.Context()) // nothing was held: no row changed, none audited
	}
	respond.JSON(w, r, http.StatusOK, map[string]int{"discarded": n})
}
