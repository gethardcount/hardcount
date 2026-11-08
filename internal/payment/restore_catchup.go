package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/stripe/stripe-go/v82"

	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/tenantstripe"
)

// The restore catch-up (ADR-135). A database restored from a backup holds
// none of what Stripe did after the backup. Before Hardcount serves it
// again, `hardcount restore catch-up` closes Hardcount's open payment pages
// at Stripe, then reads every Stripe event since the backup and applies it
// through HandleWebhook, the code that applies a webhook delivery. Applying
// adds no logic of its own, and HandleWebhook's dedup makes a rerun safe.
//
// One case HandleWebhook cannot apply: a payment whose invoice the restore
// lost or left as a draft. It is listed for the operator instead, with the
// refund and dispute events of that payment, until the operator handles it
// and acknowledges it.

// catchUpEventTypes are the event types processEvent handles. The catch-up
// lists only these.
var catchUpEventTypes = []string{
	"payment_intent.succeeded",
	"payment_intent.payment_failed",
	"checkout.session.completed",
	"setup_intent.succeeded",
	"setup_intent.setup_failed",
	"refund.created",
	"refund.updated",
	"refund.failed",
	"charge.refund.updated",
	"charge.dispute.created",
	"charge.dispute.updated",
	"charge.dispute.closed",
}

// RestoreAccounts lists every tenant and mode with stored Stripe keys,
// across tenants. tenantstripe.Service implements it.
type RestoreAccounts interface {
	ListAccounts(ctx context.Context) ([]tenantstripe.Account, error)
}

// CatchUp applies Stripe's events since a restored backup.
type CatchUp struct {
	stripe   *Stripe
	clients  *StripeClients
	accounts RestoreAccounts
}

// NewCatchUp wires the catch-up to the adapter whose HandleWebhook applies
// the events, the clients that read Stripe, and the account list.
func NewCatchUp(s *Stripe, clients *StripeClients, accounts RestoreAccounts) *CatchUp {
	return &CatchUp{stripe: s, clients: clients, accounts: accounts}
}

// CatchUpRun is one run's input.
type CatchUpRun struct {
	// Since is the creation time of the oldest event to read.
	Since time.Time
	// Extra are accounts to read besides those with stored keys: an
	// account connected after the backup, whose key is given for this run
	// only (KeyOverrides).
	Extra []tenantstripe.Account
	// Acknowledged are the PaymentIntent IDs of listed payments the
	// operator has handled.
	Acknowledged map[string]bool
}

// CatchUpReport is what one run did, per account.
type CatchUpReport struct {
	Accounts []AccountCatchUp
}

// AccountCatchUp is what one run did for one account.
type AccountCatchUp struct {
	Account tenantstripe.Account
	// Err is a failure that stopped this account part-way: Stripe or the
	// database could not be reached, or an event could not be applied yet.
	// A rerun is safe.
	Err error
	// KeyRefused is Stripe's message when it refused the account's key
	// (401 or 403). Nothing was read for the account.
	KeyRefused string
	// SessionsExpired counts the open payment pages closed at Stripe.
	SessionsExpired int
	// Events counts the events Stripe listed, and ByType by event type.
	Events int
	ByType map[string]int
	// Applied counts events applied now, AlreadyRecorded events the
	// database already held, Skipped events of another tenant or not
	// Hardcount's, and Held refund and dispute events of a listed payment.
	Applied, AlreadyRecorded, Skipped, Held int
	// Lost are the payments whose invoice the restore lost or left as a
	// draft, oldest first.
	Lost []LostPayment
}

// LostPayment is a payment Stripe took whose invoice the restored database
// does not have, or has only as a draft. The catch-up does not record it on
// any invoice: the operator matches it by hand (docs/ops/restore.md).
type LostPayment struct {
	PaymentIntentID string
	// InvoiceID and CustomerID are the PaymentIntent's hardcount_invoice_id
	// and hardcount_customer_id metadata.
	InvoiceID  string
	CustomerID string
	// InvoiceDraft is true when the invoice exists as a draft, false when
	// no row holds it.
	InvoiceDraft bool
	AmountCents  int64
	Currency     string
	PaidAt       time.Time
	// Held counts the refund and dispute events of this payment, held with
	// it. The operator sees them on the payment in Stripe.
	Held int
	// Acknowledged: the operator handled it, now or on an earlier run.
	Acknowledged bool
}

// Run reads and applies the events of every account. A failure on one
// account is reported on it and the run moves on; an error is returned only
// when the account list cannot be read.
func (c *CatchUp) Run(ctx context.Context, run CatchUpRun) (CatchUpReport, error) {
	accounts, err := c.accounts.ListAccounts(ctx)
	if err != nil {
		return CatchUpReport{}, fmt.Errorf("list Stripe accounts: %w", err)
	}
	seen := map[tenantstripe.Account]bool{}
	for _, a := range accounts {
		seen[a] = true
	}
	for _, a := range run.Extra {
		if !seen[a] {
			seen[a] = true
			accounts = append(accounts, a)
		}
	}
	var report CatchUpReport
	for _, a := range accounts {
		report.Accounts = append(report.Accounts, c.catchUpAccount(ctx, a, run))
	}
	return report, nil
}

func (c *CatchUp) catchUpAccount(ctx context.Context, acct tenantstripe.Account, run CatchUpRun) AccountCatchUp {
	res := AccountCatchUp{Account: acct, ByType: map[string]int{}}
	ctx = postgres.WithLivemode(auth.WithTenantID(ctx, acct.TenantID), acct.Livemode)
	sc, err := c.clients.Resolve(ctx, acct.TenantID, acct.Livemode)
	if err != nil {
		res.Err = fmt.Errorf("read the Stripe key: %w", err)
		return res
	}
	if sc == nil {
		res.Err = errors.New("no Stripe key is stored for this account any more")
		return res
	}
	// Close the payment pages first, so no payment can arrive after the
	// events are listed. A page paid while it closes is complete, and its
	// payment's event is in the listing.
	if err := c.expireHostedSessions(ctx, sc, acct, run.Since, &res); err != nil {
		return failAccount(res, err)
	}
	events, err := listCatchUpEvents(ctx, sc, run.Since)
	if err != nil {
		return failAccount(res, err)
	}
	res.Events = len(events)
	lost := map[string]*LostPayment{}
	for _, ev := range events {
		if err := c.applyEvent(ctx, acct, ev, run.Acknowledged, lost, &res); err != nil {
			res.Err = err
			break
		}
	}
	for _, p := range lost {
		res.Lost = append(res.Lost, *p)
	}
	sort.SliceStable(res.Lost, func(i, j int) bool {
		if !res.Lost[i].PaidAt.Equal(res.Lost[j].PaidAt) {
			return res.Lost[i].PaidAt.Before(res.Lost[j].PaidAt)
		}
		return res.Lost[i].PaymentIntentID < res.Lost[j].PaymentIntentID
	})
	return res
}

// failAccount records a Stripe error on the account: a refused key, or a
// failure a rerun may clear.
func failAccount(res AccountCatchUp, err error) AccountCatchUp {
	var se *stripe.Error
	if errors.As(err, &se) && (se.HTTPStatusCode == http.StatusUnauthorized || se.HTTPStatusCode == http.StatusForbidden) {
		res.KeyRefused = errs.Scrub(se.Msg)
		return res
	}
	res.Err = err
	return res
}

// expireHostedSessions expires every open Checkout session of the hosted
// invoice page for this tenant (hardcount_purpose=hosted_invoice_pay)
// created since `since`. Other tenants' sessions on a shared Stripe account,
// and sessions Hardcount did not make, are left alone. Expiring is the only
// write the catch-up makes at Stripe.
func (c *CatchUp) expireHostedSessions(ctx context.Context, sc *stripe.Client, acct tenantstripe.Account, since time.Time, res *AccountCatchUp) error {
	params := &stripe.CheckoutSessionListParams{
		Status:       stripe.String(string(stripe.CheckoutSessionStatusOpen)),
		CreatedRange: &stripe.RangeQueryParams{GreaterThanOrEqual: since.Unix()},
	}
	params.Limit = stripe.Int64(100)
	var open []string
	for sess, err := range sc.V1CheckoutSessions.List(ctx, params) {
		if err != nil {
			return fmt.Errorf("list open Checkout sessions: %w", err)
		}
		if sess.Metadata["hardcount_purpose"] == PurposeHostedInvoicePay && sess.Metadata["hardcount_tenant_id"] == acct.TenantID {
			open = append(open, sess.ID)
		}
	}
	for _, id := range open {
		_, err := sc.V1CheckoutSessions.Expire(ctx, id, &stripe.CheckoutSessionExpireParams{})
		if err == nil {
			res.SessionsExpired++
			continue
		}
		// Expire refuses a session that is no longer open. Ask its status:
		// the error's text is not a contract.
		sess, rErr := sc.V1CheckoutSessions.Retrieve(ctx, id, nil)
		if rErr == nil && sess.Status != stripe.CheckoutSessionStatusOpen {
			continue
		}
		return fmt.Errorf("expire Checkout session %s: %w", id, err)
	}
	return nil
}

// listCatchUpEvents returns the events of catchUpEventTypes created since
// `since`, oldest first, and events of the same second in id order. Stripe
// lists newest first and does not define the order of a tie, and a refund
// must be applied after its payment.
func listCatchUpEvents(ctx context.Context, sc *stripe.Client, since time.Time) ([]*stripe.Event, error) {
	params := &stripe.EventListParams{
		CreatedRange: &stripe.RangeQueryParams{GreaterThanOrEqual: since.Unix()},
		Types:        stripe.StringSlice(catchUpEventTypes),
	}
	params.Limit = stripe.Int64(100)
	var out []*stripe.Event
	for ev, err := range sc.V1Events.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("list Stripe events: %w", err)
		}
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Created != out[j].Created {
			return out[i].Created < out[j].Created
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// catchUpEventBody rebuilds an event's body from what List Events returned.
// data.object keeps the bytes Stripe sent, which every handler reads again.
func catchUpEventBody(ev *stripe.Event) ([]byte, error) {
	var object json.RawMessage
	if ev.Data != nil {
		object = ev.Data.Raw
	}
	body := struct {
		ID         string `json:"id"`
		Object     string `json:"object"`
		APIVersion string `json:"api_version"`
		Created    int64  `json:"created"`
		Livemode   bool   `json:"livemode"`
		Type       string `json:"type"`
		Data       struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}{ID: ev.ID, Object: ev.Object, APIVersion: ev.APIVersion, Created: ev.Created, Livemode: ev.Livemode, Type: string(ev.Type)}
	body.Data.Object = object
	return json.Marshal(body)
}

// applyEvent applies one event, or lists or holds it with a lost payment.
// A returned error stops the account: the event could not be applied now,
// and a rerun applies it and everything after it.
func (c *CatchUp) applyEvent(ctx context.Context, acct tenantstripe.Account, ev *stripe.Event, acks map[string]bool, lost map[string]*LostPayment, res *AccountCatchUp) error {
	body, err := catchUpEventBody(ev)
	if err != nil {
		return fmt.Errorf("event %s: rebuild body: %w", ev.ID, err)
	}
	event, _, err := parseStripeEvent(body, acct.TenantID, acct.Livemode)
	if err != nil {
		// Another tenant's event on a shared Stripe account, or the tenant's
		// own Stripe object: the webhook endpoint skips these too.
		res.Skipped++
		return nil
	}
	res.ByType[event.EventType]++
	seen, err := c.stripe.webhooks.WasProcessed(ctx, acct.TenantID, event.StripeEventID)
	if err != nil {
		return fmt.Errorf("event %s: %w", event.StripeEventID, err)
	}

	if event.EventType == "payment_intent.succeeded" {
		p, err := c.lostPayment(ctx, acct.TenantID, event)
		if err != nil {
			return fmt.Errorf("event %s: %w", event.StripeEventID, err)
		}
		if p != nil {
			// Recorded with no invoice change, and only once the operator
			// has handled it: an event already recorded was acknowledged on
			// an earlier run.
			p.Acknowledged = seen || acks[p.PaymentIntentID]
			lost[p.PaymentIntentID] = p
			return c.recordHandled(ctx, acct, event, seen, p.Acknowledged, res)
		}
	}
	if p := lost[heldPaymentIntent(event)]; p != nil {
		// A refund or dispute of a lost payment. HandleWebhook finds no
		// invoice for it, and fails on a payment that names a Hardcount
		// invoice, so it would stop the catch-up for good.
		p.Held++
		res.Held++
		return c.recordHandled(ctx, acct, event, seen, p.Acknowledged, res)
	}

	if seen {
		res.AlreadyRecorded++
		return nil
	}
	if err := c.stripe.HandleWebhook(ctx, acct.TenantID, event); err != nil {
		return fmt.Errorf("apply event %s (%s): %w", event.StripeEventID, event.EventType, err)
	}
	res.Applied++
	return nil
}

// recordHandled writes the dedup row of an event the operator handles by
// hand, once it is acknowledged, so the live webhook path skips its
// redelivery.
func (c *CatchUp) recordHandled(ctx context.Context, acct tenantstripe.Account, event domain.StripeWebhookEvent, seen, acknowledged bool, res *AccountCatchUp) error {
	if seen {
		res.AlreadyRecorded++
		return nil
	}
	if !acknowledged {
		return nil
	}
	if _, _, err := c.stripe.webhooks.IngestEvent(ctx, acct.TenantID, event); err != nil {
		return fmt.Errorf("event %s: record: %w", event.StripeEventID, err)
	}
	return nil
}

// lostPayment returns the payment of a payment_intent.succeeded event when
// its invoice is missing or still a draft, and nil when HandleWebhook can
// apply it: an invoice already holds the payment, or its
// hardcount_invoice_id names an invoice past draft.
func (c *CatchUp) lostPayment(ctx context.Context, tenantID string, event domain.StripeWebhookEvent) (*LostPayment, error) {
	_, err := c.stripe.invoices.GetByStripePaymentIntentID(ctx, tenantID, event.PaymentIntentID)
	if err == nil || event.InvoiceID == "" {
		return nil, nil
	}
	if !errors.Is(err, errs.ErrNotFound) {
		return nil, fmt.Errorf("find the invoice of %s: %w", event.PaymentIntentID, err)
	}
	draft := false
	inv, err := c.stripe.invoices.Get(ctx, tenantID, event.InvoiceID)
	switch {
	case errors.Is(err, errs.ErrNotFound):
	case err != nil:
		return nil, fmt.Errorf("read invoice %s: %w", event.InvoiceID, err)
	case inv.Status == domain.InvoiceDraft:
		draft = true
	default:
		return nil, nil
	}
	meta, err := piMetadata(event.Payload)
	if err != nil {
		return nil, fmt.Errorf("read the metadata of %s: %w", event.PaymentIntentID, err)
	}
	var amount int64
	if event.AmountCents != nil {
		amount = *event.AmountCents
	}
	return &LostPayment{
		PaymentIntentID: event.PaymentIntentID,
		InvoiceID:       event.InvoiceID,
		CustomerID:      meta["hardcount_customer_id"],
		InvoiceDraft:    draft,
		AmountCents:     amount,
		Currency:        strings.ToUpper(event.Currency),
		PaidAt:          event.OccurredAt,
	}, nil
}

// heldPaymentIntent returns the PaymentIntent a refund or dispute event is
// of, and "" for any other event.
func heldPaymentIntent(event domain.StripeWebhookEvent) string {
	switch event.ObjectType {
	case "dispute":
		return event.PaymentIntentID
	case "refund":
		raw, ok := event.Payload["raw"].(string)
		if !ok {
			return ""
		}
		var body struct {
			Data struct {
				Object struct {
					PaymentIntent json.RawMessage `json:"payment_intent"`
				} `json:"object"`
			} `json:"data"`
		}
		if json.Unmarshal([]byte(raw), &body) != nil {
			return ""
		}
		return stripeObjectID(body.Data.Object.PaymentIntent)
	}
	return ""
}

// KeyOverrides returns creds with the given secret keys in place of the
// stored ones. `hardcount restore catch-up --stripe-key` uses it for a key
// Stripe refuses since it was stored, or an account connected after the
// backup. The keys live in this process only and are never saved.
func KeyOverrides(creds StripeCredentials, keys map[tenantstripe.Account]string) StripeCredentials {
	return keyOverrides{creds: creds, keys: keys}
}

type keyOverrides struct {
	creds StripeCredentials
	keys  map[tenantstripe.Account]string
}

func (k keyOverrides) GetPlaintext(ctx context.Context, tenantID string, livemode bool) (tenantstripe.PlaintextSecrets, error) {
	if key, ok := k.keys[tenantstripe.Account{TenantID: tenantID, Livemode: livemode}]; ok {
		return tenantstripe.PlaintextSecrets{SecretKey: key}, nil
	}
	return k.creds.GetPlaintext(ctx, tenantID, livemode)
}
