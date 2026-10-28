package usage

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/errs"
)

// SetHeldDeps wires what the held-usage routes read besides the usage
// store: a customer by id, to check a release target. Wired in router.go
// with the customer store. Which released rows an invoice counts is read by
// the service (Service.Release).
func (h *Handler) SetHeldDeps(customers CustomerLookup) {
	h.heldCustomers = customers
}

// listHeld serves GET /v1/usage-events/held: the rows still held, grouped
// by (external customer id, event name, reason), with whether a matching
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
		exists, ok := meterOK[g.EventName]
		if !ok {
			if _, err := h.meters.GetMeterByEventName(r.Context(), tenantID, g.EventName); err == nil {
				exists = true
			} else if !errors.Is(err, errs.ErrNotFound) {
				respond.InternalError(w, r)
				slog.ErrorContext(r.Context(), "list held usage: meter lookup", "error", err)
				return
			}
			meterOK[g.EventName] = exists
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

type ReleaseHeldRequest struct {
	ExternalCustomerID string `json:"external_customer_id"`
	// CustomerID releases into a customer whose external_id differs (the
	// LiteLLM `user` value had a typo). Empty means the customer whose
	// external_id equals ExternalCustomerID.
	CustomerID string `json:"customer_id"`
}

// releaseHeld serves POST /v1/usage-events/held/release.
func (h *Handler) releaseHeld(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	var req ReleaseHeldRequest
	if !respond.DecodeStrict(w, r, &req) {
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
		m, err := h.meters.GetMeterByEventName(r.Context(), tenantID, k)
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

	// Nothing held, or every held meter still missing: nothing to release.
	res := ReleaseResult{CustomerID: customerID, StillHeld: heldRows}
	if len(meterIDs) > 0 {
		res, err = h.svc.Release(r.Context(), tenantID, ReleaseInput{
			ExternalCustomerID: ext,
			CustomerID:         customerID,
			MeterIDs:           meterIDs,
		})
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

type DiscardHeldRequest struct {
	ExternalCustomerID string `json:"external_customer_id"`
	// EventName limits the discard to one meter. Empty discards every held
	// row for the external id.
	EventName string `json:"event_name"`
}

// discardHeld serves POST /v1/usage-events/held/discard. The body decodes
// strictly: an unknown field such as the old `meter_key` would otherwise
// leave EventName empty and discard every held row of the customer, and a
// discarded row is never billed.
func (h *Handler) discardHeld(w http.ResponseWriter, r *http.Request) {
	tenantID := auth.TenantID(r.Context())
	var req DiscardHeldRequest
	if !respond.DecodeStrict(w, r, &req) {
		return
	}
	n, err := h.svc.Discard(r.Context(), tenantID, strings.TrimSpace(req.ExternalCustomerID), strings.TrimSpace(req.EventName))
	if err != nil {
		respond.FromError(w, r, err, "usage_event")
		return
	}
	if n == 0 {
		audit.MarkSkip(r.Context()) // nothing was held: no row changed, none audited
	}
	respond.JSON(w, r, http.StatusOK, map[string]int{"discarded": n})
}
