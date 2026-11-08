package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
// Some events HandleWebhook cannot apply, or would apply wrongly on a
// restored database. The catch-up lists them for the operator instead, and
// records them only once the operator acknowledges them:
//   - a payment whose invoice the restore lost or left as a draft, with the
//     refund, dispute and card events of that payment and its customer;
//   - a refund Hardcount made whose credit note the restore lost, and a
//     refund made in Stripe on an invoice reported to Stripe Tax, whose tax
//     the old server may already have reversed;
//   - a card event of a customer the restore lost, and any event whose
//     handler fails.

// catchUpEventTypes are the event types processEvent handles
// (TestCatchUpEventTypesMatchProcessEvent), in the order the catch-up
// applies events created in the same second: a payment, its Checkout
// session and its saved card before a refund or dispute of it.
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

// RestoreCustomers reads a customer, to find a card event of a customer the
// restore lost. customer.PostgresStore implements it; a missing customer is
// errs.ErrNotFound.
type RestoreCustomers interface {
	Get(ctx context.Context, tenantID, id string) (domain.Customer, error)
}

// RestoreRefunds reports whether the restored database holds a refund: a
// credit note carries its id, or it is recorded as a refund made in Stripe.
// creditnote.PostgresStore implements it.
type RestoreRefunds interface {
	StripeRefundRecorded(ctx context.Context, tenantID, stripeRefundID string) (bool, error)
}

// CatchUp applies Stripe's events since a restored backup.
type CatchUp struct {
	stripe    *Stripe
	clients   *StripeClients
	accounts  RestoreAccounts
	customers RestoreCustomers
	refunds   RestoreRefunds
}

// NewCatchUp wires the catch-up to the adapter whose HandleWebhook applies
// the events, the clients that read Stripe, the account list, and the two
// lookups that decide what to list instead of applying.
func NewCatchUp(s *Stripe, clients *StripeClients, accounts RestoreAccounts, customers RestoreCustomers, refunds RestoreRefunds) *CatchUp {
	return &CatchUp{stripe: s, clients: clients, accounts: accounts, customers: customers, refunds: refunds}
}

// CatchUpRun is one run's input.
type CatchUpRun struct {
	// Since is the creation time of the oldest event to read.
	Since time.Time
	// Extra are accounts to read besides those with stored keys: an
	// account connected after the backup, whose key is given for this run
	// only (KeyOverrides).
	Extra []tenantstripe.Account
	// Acknowledged are the ids of what the operator has handled: listed
	// payments (pi_…), listed refunds (re_…) and listed events (evt_…).
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
	// database could not be reached. A rerun is safe.
	Err error
	// KeyRefused says why the account's key cannot be used: Stripe refused
	// it (401 or 403), or it is a key of the other mode. Nothing more was
	// read for the account.
	KeyRefused string
	// SessionsExpired counts the open payment pages closed at Stripe.
	SessionsExpired int
	// Events counts the events Stripe listed, and ByType by event type.
	Events int
	ByType map[string]int
	// Applied counts events applied now, AlreadyRecorded events the
	// database already held, Skipped events of another tenant or not
	// Hardcount's, and Held the events held with a listed payment or
	// refund.
	Applied, AlreadyRecorded, Skipped, Held int
	// Lost are the payments whose invoice the restore lost or left as a
	// draft, oldest first.
	Lost []LostPayment
	// Refunds are the refunds not applied, oldest first.
	Refunds []ListedRefund
	// Unapplied are the other events not applied, in the order applied.
	Unapplied []UnappliedEvent
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
	// Held counts the events held with this payment: its refunds and
	// disputes, and the card events of its customer when the restore lost
	// the customer too. The operator sees them on the payment in Stripe.
	Held int
	// Acknowledged: the operator handled it, now or on an earlier run.
	Acknowledged bool
}

// Why a refund is listed instead of applied.
const (
	// RefundCreditNoteLost: Hardcount made the refund (hardcount_cn_id),
	// and no credit note in the restored database holds it. Applied, it
	// would be acknowledged with no record, and the invoice would show the
	// money as refundable again.
	RefundCreditNoteLost = "credit_note_lost"
	// RefundStripeTax: the refund was made in Stripe, the restored database
	// does not hold it, and its invoice was reported to Stripe Tax. The old
	// server may have recorded it and reversed its tax already; applied
	// again, it would reverse the tax a second time.
	RefundStripeTax = "stripe_tax"
)

// ListedRefund is a refund of a payment the restored database holds, which
// the catch-up does not apply. The operator handles it by hand
// (docs/ops/restore.md).
type ListedRefund struct {
	RefundID        string
	PaymentIntentID string
	// InvoiceID is the invoice that holds the refunded payment.
	InvoiceID string
	// CreditNoteID is the refund's hardcount_cn_id, for a refund Hardcount
	// made.
	CreditNoteID string
	// Reason is RefundCreditNoteLost or RefundStripeTax.
	Reason      string
	AmountCents int64
	Currency    string
	// Status is the refund's status in its newest event.
	Status    string
	CreatedAt time.Time
	// Events counts the refund's events held with it.
	Events int
	// Acknowledged: the operator handled it, now or on an earlier run.
	Acknowledged bool
}

// UnappliedEvent is an event the catch-up did not apply and that no listed
// payment or refund holds: a card event of a customer the restore lost, or
// an event whose handler failed.
type UnappliedEvent struct {
	EventID   string
	Type      string
	CreatedAt time.Time
	// CustomerID is the lost customer of a card event.
	CustomerID string
	// Err is why the handler failed, nil for a card event of a lost
	// customer.
	Err error
	// Acknowledged: the event is recorded without being applied. A failed
	// event is tried again first, and recorded only if it fails again.
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

// listed is what one account's run lists, keyed by the id the operator
// acknowledges.
type listed struct {
	payments map[string]*LostPayment
	refunds  map[string]*ListedRefund
}

// errWrongModeKey stops an account whose key lists events of the other
// mode: a test key given for live mode, or the reverse.
type errWrongModeKey struct{ eventID string }

func (e errWrongModeKey) Error() string {
	return fmt.Sprintf("Stripe listed event %s of the other mode: the key is not a key of this mode", e.eventID)
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
	l := listed{payments: map[string]*LostPayment{}, refunds: map[string]*ListedRefund{}}
	for _, ev := range events {
		err := c.applyEvent(ctx, acct, ev, run.Acknowledged, l, &res)
		var wrongMode errWrongModeKey
		if errors.As(err, &wrongMode) {
			res.KeyRefused = wrongMode.Error()
			break
		}
		if err != nil {
			res.Err = err
			break
		}
	}
	for _, p := range l.payments {
		res.Lost = append(res.Lost, *p)
	}
	sort.SliceStable(res.Lost, func(i, j int) bool {
		if !res.Lost[i].PaidAt.Equal(res.Lost[j].PaidAt) {
			return res.Lost[i].PaidAt.Before(res.Lost[j].PaidAt)
		}
		return res.Lost[i].PaymentIntentID < res.Lost[j].PaymentIntentID
	})
	for _, r := range l.refunds {
		res.Refunds = append(res.Refunds, *r)
	}
	sort.SliceStable(res.Refunds, func(i, j int) bool {
		if !res.Refunds[i].CreatedAt.Equal(res.Refunds[j].CreatedAt) {
			return res.Refunds[i].CreatedAt.Before(res.Refunds[j].CreatedAt)
		}
		return res.Refunds[i].RefundID < res.Refunds[j].RefundID
	})
	return res
}

// failAccount records a Stripe error on the account: a refused key, or a
// failure a rerun may clear.
func failAccount(res AccountCatchUp, err error) AccountCatchUp {
	var se *stripe.Error
	if errors.As(err, &se) && (se.HTTPStatusCode == http.StatusUnauthorized || se.HTTPStatusCode == http.StatusForbidden) {
		res.KeyRefused = "Stripe refused the key: " + errs.Scrub(se.Msg)
		return res
	}
	res.Err = err
	return res
}

// expireHostedSessions expires every open Checkout session of the hosted
// invoice page for this tenant (hardcount_purpose=hosted_invoice_pay)
// created since `since`. Other tenants' sessions on a shared Stripe account,
// and sessions Hardcount did not make, are left alone. It is the one call to
// Stripe the catch-up makes itself. The handlers it replays write to Stripe
// as a live delivery does: a settled payment expires the invoice's other
// sessions, a saved card becomes the Stripe customer's default card, and a
// refund made in Stripe reverses its tax in Stripe Tax.
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
// `since`, oldest first. Stripe lists newest first and does not define the
// order of events created in the same second, and its event ids carry no
// order. So events of the same second go in the order of
// catchUpEventTypes, which puts a payment before its refund, then by id.
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
		ri, rj := slices.Index(catchUpEventTypes, string(out[i].Type)), slices.Index(catchUpEventTypes, string(out[j].Type))
		if ri != rj {
			return ri < rj
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

// eventObject is what the catch-up reads of an event's data.object to
// decide whether to apply it.
type eventObject struct {
	ID            string            `json:"id"`
	PaymentIntent json.RawMessage   `json:"payment_intent"`
	Customer      json.RawMessage   `json:"customer"`
	Status        string            `json:"status"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	Created       int64             `json:"created"`
	Metadata      map[string]string `json:"metadata"`
}

func readEventObject(event domain.StripeWebhookEvent) (eventObject, error) {
	raw, ok := event.Payload["raw"].(string)
	if !ok {
		return eventObject{}, errors.New("no event body")
	}
	var body struct {
		Data struct {
			Object eventObject `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		return eventObject{}, err
	}
	return body.Data.Object, nil
}

// applyEvent applies one event, or lists it, or holds it with something
// listed. A returned error stops the account: the database or Stripe could
// not be reached, and a rerun applies the event and everything after it.
// An event whose handler fails is listed, and the account goes on: the
// handlers are written for events out of order, as webhooks arrive.
func (c *CatchUp) applyEvent(ctx context.Context, acct tenantstripe.Account, ev *stripe.Event, acks map[string]bool, l listed, res *AccountCatchUp) error {
	body, err := catchUpEventBody(ev)
	if err != nil {
		return fmt.Errorf("event %s: rebuild body: %w", ev.ID, err)
	}
	event, _, err := parseStripeEvent(body, acct.TenantID, acct.Livemode)
	switch {
	case errors.Is(err, errEventOtherTenant), errors.Is(err, errEventNotHardcount):
		// Another tenant's event on a shared Stripe account, or the tenant's
		// own Stripe object: the webhook endpoint skips these too.
		res.Skipped++
		return nil
	case errors.Is(err, errEventModeMismatch):
		return errWrongModeKey{eventID: ev.ID}
	case err != nil:
		return fmt.Errorf("event %s: %w", ev.ID, err)
	}
	obj, err := readEventObject(event)
	if err != nil {
		return fmt.Errorf("event %s: read its object: %w", event.StripeEventID, err)
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
			l.payments[p.PaymentIntentID] = p
			return c.recordHandled(ctx, acct, event, seen, p.Acknowledged, res)
		}
	}
	if p := l.payments[heldPaymentIntent(event, obj)]; p != nil {
		// A refund or dispute of a lost payment. HandleWebhook finds no
		// invoice for it, and fails on a payment that names a Hardcount
		// invoice, so it would never apply.
		return c.hold(ctx, acct, event, seen, &p.Held, p.Acknowledged, res)
	}

	if cust := cardEventCustomer(event, obj); cust != "" {
		_, err := c.customers.Get(ctx, acct.TenantID, cust)
		switch {
		case errors.Is(err, errs.ErrNotFound):
			// The restore lost the customer: the card cannot be saved on
			// it. Held with the customer's listed payment, or listed.
			if p := l.paymentOf(stripeObjectID(obj.PaymentIntent), cust); p != nil {
				return c.hold(ctx, acct, event, seen, &p.Held, p.Acknowledged, res)
			}
			u := UnappliedEvent{EventID: event.StripeEventID, Type: event.EventType, CreatedAt: event.OccurredAt,
				CustomerID: cust, Acknowledged: seen || acks[event.StripeEventID]}
			res.Unapplied = append(res.Unapplied, u)
			return c.recordHandled(ctx, acct, event, seen, u.Acknowledged, res)
		case err != nil:
			return fmt.Errorf("event %s: read customer %s: %w", event.StripeEventID, cust, err)
		}
	}

	if event.ObjectType == "refund" {
		r, err := c.listedRefund(ctx, acct.TenantID, obj, seen, acks, l)
		if err != nil {
			return fmt.Errorf("event %s: %w", event.StripeEventID, err)
		}
		if r != nil {
			return c.hold(ctx, acct, event, seen, &r.Events, r.Acknowledged, res)
		}
	}

	if seen {
		res.AlreadyRecorded++
		return nil
	}

	if event.EventType == "setup_intent.succeeded" && obj.Metadata["hardcount_customer_id"] == "" {
		ours, err := c.customerKnown(ctx, acct.TenantID, stripeObjectID(obj.Customer))
		if err != nil {
			return fmt.Errorf("event %s: %w", event.StripeEventID, err)
		}
		if !ours {
			// A SetupIntent Hardcount did not create. Hardcount's setup
			// sessions stamp hardcount_customer_id, and link the Stripe
			// customer before they create the session, so an unstamped one
			// whose Stripe customer no Hardcount customer holds is the
			// tenant's own. The live handler asks Stripe to deliver it again
			// for three days, in case the link is still being written; here
			// nothing is being written.
			res.Skipped++
			return nil
		}
	}

	if err := c.stripe.HandleWebhook(ctx, acct.TenantID, event); err != nil {
		u := UnappliedEvent{EventID: event.StripeEventID, Type: event.EventType, CreatedAt: event.OccurredAt,
			Err: err, Acknowledged: acks[event.StripeEventID]}
		res.Unapplied = append(res.Unapplied, u)
		return c.recordHandled(ctx, acct, event, false, u.Acknowledged, res)
	}
	res.Applied++
	return nil
}

// hold counts an event held with a listed payment or refund, and records
// it once that is acknowledged.
func (c *CatchUp) hold(ctx context.Context, acct tenantstripe.Account, event domain.StripeWebhookEvent, seen bool, count *int, acknowledged bool, res *AccountCatchUp) error {
	*count++
	res.Held++
	return c.recordHandled(ctx, acct, event, seen, acknowledged, res)
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

// paymentOf returns the listed payment pi, or else a listed payment of the
// customer, to hold a card event of a customer the restore lost.
func (l listed) paymentOf(pi, customerID string) *LostPayment {
	if p := l.payments[pi]; p != nil {
		return p
	}
	var ids []string
	for id, p := range l.payments {
		if p.CustomerID == customerID {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	return l.payments[ids[0]]
}

// heldPaymentIntent returns the PaymentIntent a refund or dispute event is
// of, and "" for any other event.
func heldPaymentIntent(event domain.StripeWebhookEvent, obj eventObject) string {
	switch event.ObjectType {
	case "dispute":
		return event.PaymentIntentID
	case "refund":
		return stripeObjectID(obj.PaymentIntent)
	}
	return ""
}

// cardEventCustomer returns the hardcount_customer_id of an event that saves
// a card on a Hardcount customer, and "" for any other event.
func cardEventCustomer(event domain.StripeWebhookEvent, obj eventObject) string {
	switch event.EventType {
	case "checkout.session.completed", "setup_intent.succeeded":
		return obj.Metadata["hardcount_customer_id"]
	}
	return ""
}

// customerKnown reports whether a Hardcount customer of the tenant holds the
// Stripe customer. No Stripe customer, or no resolver, is not known.
func (c *CatchUp) customerKnown(ctx context.Context, tenantID, stripeCustomerID string) (bool, error) {
	if stripeCustomerID == "" || c.stripe.customerResolver == nil {
		return false, nil
	}
	_, err := c.stripe.customerResolver.CustomerIDByStripeID(ctx, tenantID, stripeCustomerID)
	switch {
	case errors.Is(err, errs.ErrNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("find the customer of Stripe customer %s: %w", stripeCustomerID, err)
	}
	return true, nil
}

// listedRefund returns the listed refund a refund event belongs to, and nil
// when HandleWebhook can apply it: the refund is of a payment no invoice of
// this tenant holds (the handler decides those), the restored database
// holds the refund, or it is a refund made in Stripe on an invoice not
// reported to Stripe Tax. seen: the event is already recorded, so the
// refund was acknowledged on an earlier run.
func (c *CatchUp) listedRefund(ctx context.Context, tenantID string, obj eventObject, seen bool, acks map[string]bool, l listed) (*ListedRefund, error) {
	if r := l.refunds[obj.ID]; r != nil {
		r.Status = obj.Status
		return r, nil
	}
	pi := stripeObjectID(obj.PaymentIntent)
	if obj.ID == "" || pi == "" {
		return nil, nil
	}
	inv, err := c.stripe.invoices.GetByStripePaymentIntentID(ctx, tenantID, pi)
	if errors.Is(err, errs.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find the invoice of %s: %w", pi, err)
	}
	recorded, err := c.refunds.StripeRefundRecorded(ctx, tenantID, obj.ID)
	if err != nil {
		return nil, fmt.Errorf("find refund %s: %w", obj.ID, err)
	}
	if recorded {
		return nil, nil
	}
	reason := ""
	switch {
	case obj.Metadata["hardcount_cn_id"] != "":
		reason = RefundCreditNoteLost
	case inv.TaxTransactionID != "":
		reason = RefundStripeTax
	default:
		return nil, nil
	}
	r := &ListedRefund{
		RefundID:        obj.ID,
		PaymentIntentID: pi,
		InvoiceID:       inv.ID,
		CreditNoteID:    obj.Metadata["hardcount_cn_id"],
		Reason:          reason,
		AmountCents:     obj.Amount,
		Currency:        strings.ToUpper(obj.Currency),
		Status:          obj.Status,
		CreatedAt:       time.Unix(obj.Created, 0).UTC(),
		Acknowledged:    seen || acks[obj.ID],
	}
	l.refunds[obj.ID] = r
	return r, nil
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
