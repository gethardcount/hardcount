package invoice_test

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/credit"
	"github.com/gethardcount/hardcount/internal/creditnote"
	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/money"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

var (
	reliefSeed  = flag.Int64("relief-seed", 0, "run TestCommitRelief_RandomSequence with this seed only")
	reliefSteps = flag.Int("relief-steps", 300, "steps per seed in TestCommitRelief_RandomSequence")
)

// TestCommitRelief_RandomSequence looks for an interleaving nobody thought of:
// a seeded random sequence of reliefs (some of whose card refunds fail),
// "returned another way" on a failed one, usage and its close, expiry, refunds
// made in Stripe and disputes, on one customer with top-ups (every other one
// discounted) and promotional credit. A refund made in Stripe and a lost
// dispute write their own notes, which take credit back, and a void of a
// usage invoice gives drawn credit back to its grants. Stripe never refunds
// more than the card has left; the bank may take the whole charge back after
// part of it was refunded. Seven invariants are checked after every step, and
// each kind of step must have succeeded a few times, so a change that
// refuses every relief cannot pass. A failure names its seed; -relief-seed
// replays it.
func TestCommitRelief_RandomSequence(t *testing.T) {
	seeds := []int64{1, 2, 3}
	if *reliefSeed != 0 {
		seeds = []int64{*reliefSeed}
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			t.Logf("seed %d (replay: -relief-seed=%d)", seed, seed)
			newReliefSequence(t, seed).run(*reliefSteps)
		})
	}
}

// unbilledUsage is the usage not invoiced yet, which the sequence moves.
type unbilledUsage struct{ cents int64 }

func (u *unbilledUsage) UnbilledUsage(context.Context, string, string) (map[string]int64, error) {
	return map[string]int64{"USD": u.cents}, nil
}

type seqTopup struct {
	inv            domain.Invoice
	pi             string
	price, granted int64
	disputeID      string
	givenBack      int64 // credit voids gave back to its grant once its notes returned all of it (ADR-132)
}

type reliefSequence struct {
	t        *testing.T
	seed     int64
	h        *commitHarness
	ctx      context.Context
	rng      *rand.Rand
	bank     *rand.Rand // whether the bank takes the whole charge: its own stream, so the other steps draw as before
	svc      *creditnote.Service
	cards    *stripeRefunds
	unbilled *unbilledUsage
	tops     []*seqTopup
	promoID  string
	eventAt  time.Time          // the next Stripe event's time; each event is a second later
	refunds  []stripeRefundMade // refunds made in Stripe that succeeded
	inFlight []stripeRefundMade // refunds made in Stripe still pending
	closes   []string           // usage invoices a close made, not voided
	n        int
	done     map[string]int // steps that succeeded, by kind
}

type stripeRefundMade struct {
	id, pi string
	cents  int64
}

// seqGrant is a top-up's grant as the ledger holds it; live is what is left
// of it while it has not expired.
type seqGrant struct {
	id                                string
	amount, consumed, cnRetired, live int64
}

func newReliefSequence(t *testing.T, seed int64) *reliefSequence {
	h, ctx := newCommitHarness(t, "Relief Sequence")
	s := &reliefSequence{t: t, seed: seed, h: h, ctx: ctx, rng: rand.New(rand.NewSource(seed)), bank: rand.New(rand.NewSource(seed)),
		unbilled: &unbilledUsage{}, eventAt: time.Now().UTC().Truncate(time.Second), cards: &stripeRefunds{}, done: map[string]int{}}
	s.svc = newCardReliefService(h, s.cards)
	s.svc.SetUnbilledUsageReader(s.unbilled)
	h.invStore.SetDisputeLossRecorder(s.svc)
	s.buy()
	s.buy()
	expires := time.Now().Add(24 * time.Hour)
	promo, err := h.creditSvc.Grant(ctx, h.tenantID, credit.GrantInput{
		CustomerID: h.custID, Currency: "USD", AmountCents: 20000, Description: "promo", ExpiresAt: &expires, GrantKind: domain.GrantKindPromotional,
	})
	if err != nil {
		t.Fatalf("promotional grant: %v", err)
	}
	s.promoID = promo.ID
	return s
}

// sequenceFloor is how many times each kind of step must succeed in a run of
// the default length: well under what the seeds do, well over zero.
var sequenceFloor = map[string]int{
	"relief": 10, "returned another way": 1, "close drew credit": 3, "expiry": 2,
	"purchase": 1, "refund made in Stripe": 5, "dispute": 10,
	"refund recorded as a note": 1, "loss recorded as a note": 1, "void gave credit back": 1,
}

func (s *reliefSequence) run(steps int) {
	for step := 0; step < steps; step++ {
		did := s.step()
		s.checkInvariants(did)
		if s.t.Failed() {
			s.t.Fatalf("seed %d failed at step %d: %s", s.seed, step, did)
		}
	}
	s.t.Logf("seed %d: steps that succeeded, by kind: %v", s.seed, s.done)
	for kind, floor := range sequenceFloor {
		if s.done[kind] < floor*steps/300 {
			s.t.Errorf("seed %d: %d %q steps succeeded, want at least %d", s.seed, s.done[kind], kind, floor*steps/300)
		}
	}
}

func (s *reliefSequence) top() *seqTopup { return s.tops[s.rng.Intn(len(s.tops))] }

// buy is a top-up paid by card: 90.00 for 100.00 of credit and 50.00 for
// 50.00 in turn, at most six.
func (s *reliefSequence) buy() string {
	if len(s.tops) == 6 {
		return "no purchase: six top-ups"
	}
	price, granted := int64(90000), int64(100000)
	if len(s.tops)%2 == 1 {
		price, granted = 50000, 50000
	}
	inv := s.h.createTopup(s.t, s.ctx, "USD", price, granted)
	pi := fmt.Sprintf("pi_seq_%d_%d", s.seed, len(s.tops))
	payByCard(s.t, s.h, s.ctx, inv.ID, pi)
	s.tops = append(s.tops, &seqTopup{inv: inv, pi: pi, price: price, granted: granted})
	s.done["purchase"]++
	return fmt.Sprintf("bought %s, %d for %d", inv.ID, price, granted)
}

func (s *reliefSequence) nextEventAt() time.Time {
	s.eventAt = s.eventAt.Add(time.Second)
	return s.eventAt
}

// step takes one random action and says what it did.
func (s *reliefSequence) step() string {
	// Expiry is for good, a chargeback blocks relief and relief uses credit
	// up, so the customer buys another top-up now and then and reliefs keep
	// succeeding between them.
	switch k := s.rng.Intn(100); {
	case k < 40:
		return s.relief()
	case k < 52:
		s.unbilled.cents += s.rng.Int63n(2000)
		return fmt.Sprintf("usage, unbilled %d", s.unbilled.cents)
	case k < 61:
		return s.close()
	case k < 64:
		return s.void()
	case k < 67:
		return s.expire()
	case k < 71:
		return s.buy()
	case k < 84:
		return s.stripeRefund()
	case k < 90:
		return s.returnedAnotherWay()
	default:
		return s.dispute()
	}
}

func (s *reliefSequence) relief() string {
	tp := s.top()
	at := time.Now().UTC().Truncate(time.Microsecond)
	unused := s.unusedAt(tp, at)
	blocked := s.chargebackOpen(tp)
	k := s.grant(tp).cnRetired

	in := creditnote.CommitReliefInput{InvoiceID: tp.inv.ID}
	// One relief in four takes all it can, which reaches what the payment's
	// documents allow once money went back outside Hardcount.
	if s.rng.Intn(4) == 0 {
		in.RetireAll = true
	} else {
		in.RetireCents = 1 + s.rng.Int63n(5000)
	}
	r := in.RetireCents
	if in.RetireAll {
		r = unused
	}
	// Half the reliefs name the split: a share to the card, the rest paid
	// back outside Stripe.
	if x := reliefCash(tp.price, tp.granted, k, r); s.rng.Intn(2) == 0 && x > 0 {
		card := s.rng.Int63n(x + 1)
		oob := x - card
		in.RefundAmountCents, in.OutOfBandAmountCents = &card, &oob
	}
	// One card refund in five is refused by Stripe, as a chargeback opened
	// meanwhile makes it.
	if s.rng.Intn(5) == 0 {
		s.cards.refuseNext("charge_disputed")
	}
	cn, err := s.svc.CreateAndIssueCommitRelief(clock.WithEffectiveNow(s.ctx, at), s.h.tenantID, in)
	s.cards.refuse.Store(nil)
	did := fmt.Sprintf("relief of %s (retire %d, all %v, unused %d)", tp.inv.ID, in.RetireCents, in.RetireAll, unused)
	if err != nil {
		if !errors.Is(err, errs.ErrInvalidState) && !errors.Is(err, errs.ErrValidation) {
			s.t.Fatalf("%s: unexpected error: %v", did, err)
		}
		return did + ": refused: " + err.Error()
	}
	if blocked {
		s.t.Errorf("invariant 4: %s was issued while a chargeback is open", did)
	}
	if cn.CommitRetiredCents > unused {
		s.t.Errorf("invariant 5: %s took %d credit, more than the %d unused at its read", did, cn.CommitRetiredCents, unused)
	}
	s.done["relief"]++
	return fmt.Sprintf("%s: took %d for %d cash, card refund %s", did, cn.CommitRetiredCents, cn.TotalCents, cn.RefundStatus)
}

// returnedAnotherWay marks a failed card refund of a relief as paid back
// outside Stripe. The service may refuse it (a chargeback open, a refund made
// in Stripe not recorded yet).
func (s *reliefSequence) returnedAnotherWay() string {
	var failed []domain.CreditNote
	for _, tp := range s.tops {
		for _, cn := range reliefNotes(s.t, s.ctx, s.svc, s.h.tenantID, tp.inv.ID) {
			if cn.RefundStatus == domain.RefundFailed {
				failed = append(failed, cn)
			}
		}
	}
	if len(failed) == 0 {
		return "no failed card refund to mark"
	}
	cn := failed[s.rng.Intn(len(failed))]
	did := "returned another way on " + cn.CreditNoteNumber
	_, err := s.svc.MarkRefundReturnedAnotherWay(s.ctx, s.h.tenantID, cn.ID)
	if errs.Code(err) == "stripe_refund_unresolved" {
		// As the operator does: the refunds made in Stripe on the invoice are
		// not this refund's money, so dismiss them, then mark it.
		did += ", after dismissing the refunds made in Stripe"
		s.dismissStripeRefunds(cn.InvoiceID)
		_, err = s.svc.MarkRefundReturnedAnotherWay(s.ctx, s.h.tenantID, cn.ID)
	}
	if err != nil {
		if !errors.Is(err, errs.ErrInvalidState) {
			s.t.Fatalf("%s: unexpected error: %v", did, err)
		}
		return did + ": refused: " + err.Error()
	}
	s.done["returned another way"]++
	return did
}

// dismissStripeRefunds dismisses every refund made in Stripe on the invoice
// that needs review. One still in flight cannot be dismissed and stays.
func (s *reliefSequence) dismissStripeRefunds(invoiceID string) {
	rows, err := creditnote.NewPostgresStore(s.h.db).ListStripeRefundsByInvoice(s.ctx, s.h.tenantID, invoiceID)
	if err != nil {
		s.t.Fatalf("list refunds made in Stripe: %v", err)
	}
	for _, r := range rows {
		if r.Outcome != domain.StripeRefundNeedsReview {
			continue
		}
		if _, err := s.svc.DismissStripeRefund(s.ctx, s.h.tenantID, invoiceID, r.StripeRefundID, "not this refund's money"); err != nil {
			s.t.Fatalf("dismiss %s: %v", r.StripeRefundID, err)
		}
	}
}

// close bills the unbilled usage: credit pays what it can.
func (s *reliefSequence) close() string {
	if s.unbilled.cents == 0 {
		return "close with nothing to bill"
	}
	s.n++
	inv := s.h.seedPayable(s.t, s.ctx, s.h.custID, s.unbilled.cents, 10000+s.n)
	drawn, err := credit.NewPostgresStore(s.h.db).ApplyToInvoiceAtomic(s.ctx, s.h.tenantID, s.h.custID, inv, "usage", s.unbilled.cents, time.Now().UTC())
	if err != nil {
		s.t.Fatalf("close: %v", err)
	}
	did := fmt.Sprintf("close of %d usage, credit paid %d", s.unbilled.cents, drawn)
	s.unbilled.cents = 0
	if drawn > 0 {
		s.done["close drew credit"]++
		s.closes = append(s.closes, inv)
	}
	return did
}

// void voids a usage invoice a close made, as an operator does: the credit
// it drew goes back to the grants it came from (ADR-132), so credit already
// used, and already paid back by a note for money returned outside
// Hardcount, can be unused again.
func (s *reliefSequence) void() string {
	if len(s.closes) == 0 {
		return "no usage invoice to void"
	}
	i := s.rng.Intn(len(s.closes))
	id := s.closes[i]
	s.closes = append(s.closes[:i], s.closes[i+1:]...)
	consumed := map[*seqTopup]int64{}
	for _, tp := range s.tops {
		consumed[tp] = s.grant(tp).consumed
	}
	if _, err := s.h.invSvc.Void(s.ctx, s.h.tenantID, id); err != nil {
		s.t.Fatalf("void %s: %v", id, err)
	}
	// A void can also write off a share whose grant expired, which raises
	// consumed; only what it gave back counts.
	for _, tp := range s.tops {
		if back := consumed[tp] - s.grant(tp).consumed; back > 0 && s.issuedTotal(tp) >= tp.price {
			tp.givenBack += back
		}
	}
	s.done["void gave credit back"]++
	return "voided " + id
}

// expire moves a grant past its expiry, as time does, and sometimes sweeps.
func (s *reliefSequence) expire() string {
	id := s.promoID
	if s.rng.Intn(2) == 0 {
		id = s.grant(s.top()).id
	}
	execBypassStamp(s.t, s.h.db, `UPDATE customer_credit_ledger SET expires_at = LEAST(COALESCE(expires_at, NOW()), NOW()) - INTERVAL '1 millisecond' WHERE id = $1`, id)
	did := "expiry of " + id
	s.done["expiry"]++
	if s.rng.Intn(2) == 0 {
		if _, errsList := s.h.creditSvc.ExpireCredits(s.ctx); len(errsList) > 0 {
			s.t.Fatalf("sweep: %v", errsList)
		}
		did += ", swept"
	}
	return did
}

// stripeRefund is a refund made in Stripe, never more than Stripe would
// allow: what the card has left after every refund of it. Sometimes an
// earlier one still in flight settles, or one fails after it succeeded.
func (s *reliefSequence) stripeRefund() string {
	if len(s.inFlight) > 0 && s.rng.Intn(3) == 0 {
		i := s.rng.Intn(len(s.inFlight))
		re := s.inFlight[i]
		s.inFlight = append(s.inFlight[:i], s.inFlight[i+1:]...)
		status := domain.StripeRefundSucceeded
		if s.rng.Intn(2) == 0 {
			status = domain.StripeRefundFailed
		} else {
			s.refunds = append(s.refunds, re)
		}
		recordStripeRefund(s.t, s.ctx, s.svc, s.h.tenantID, re.id, re.pi, re.cents, status, s.nextEventAt())
		return "refund " + re.id + " settled, " + string(status)
	}
	if len(s.refunds) > 0 && s.rng.Intn(4) == 0 {
		re := s.refunds[s.rng.Intn(len(s.refunds))]
		recordStripeRefund(s.t, s.ctx, s.svc, s.h.tenantID, re.id, re.pi, re.cents, domain.StripeRefundFailed, s.nextEventAt())
		return "refund " + re.id + " failed after it succeeded"
	}
	tp := s.top()
	if s.chargebackOpen(tp) || s.lost(tp) {
		return "no refund: Stripe refuses a refund of a disputed payment"
	}
	left := tp.price - s.cash(tp)
	if left <= 0 {
		return "no refund: the payment is returned in full"
	}
	statuses := []domain.StripeRefundStatus{domain.StripeRefundSucceeded, domain.StripeRefundPending, domain.StripeRefundRequiresAction, domain.StripeRefundFailed}
	status := statuses[s.rng.Intn(len(statuses))]
	s.n++
	re := fmt.Sprintf("re_seq_%d_%d", s.seed, s.n)
	cents := 1 + s.rng.Int63n(min(left, 20000))
	if status == domain.StripeRefundSucceeded && s.rng.Intn(3) == 0 {
		return s.lateRefund(tp, re, cents)
	}
	recordStripeRefund(s.t, s.ctx, s.svc, s.h.tenantID, re, tp.pi, cents, status, s.nextEventAt())
	switch {
	case status == domain.StripeRefundSucceeded:
		s.refunds = append(s.refunds, stripeRefundMade{re, tp.pi, cents})
	case status.InFlight():
		s.inFlight = append(s.inFlight, stripeRefundMade{re, tp.pi, cents})
	}
	if status == domain.StripeRefundSucceeded && s.heldByNote(re) {
		s.done["refund recorded as a note"]++
	}
	s.done["refund made in Stripe"]++
	return fmt.Sprintf("refund %s of %d made in Stripe, %s", re, cents, status)
}

// lateRefund is a refund made in Stripe that Hardcount hears of two days
// late, so it is flagged; the operator records it with a credit note linked
// to it, or says they handled it outside Hardcount.
func (s *reliefSequence) lateRefund(tp *seqTopup, re string, cents int64) string {
	at := s.nextEventAt()
	if _, err := s.svc.RecordStripeRefund(s.ctx, s.h.tenantID, domain.StripeRefundSnapshot{
		StripeEventID: "evt_" + re, EventAt: at, StripeRefundID: re, StripePaymentIntentID: tp.pi,
		AmountCents: cents, Currency: "usd", Status: domain.StripeRefundSucceeded, CreatedAt: at.Add(-48 * time.Hour),
	}); err != nil {
		s.t.Fatalf("record refund %s: %v", re, err)
	}
	s.refunds = append(s.refunds, stripeRefundMade{re, tp.pi, cents})
	s.done["refund made in Stripe"]++
	did := fmt.Sprintf("refund %s of %d made in Stripe, seen late", re, cents)
	if s.rng.Intn(2) == 0 {
		s.svc.SetRefundReader(fixedRefund{domain.StripeRefundFacts{ID: re, PaymentIntentID: tp.pi, AmountCents: cents, Currency: "usd", Status: domain.StripeRefundSucceeded}})
		_, err := s.svc.Create(s.ctx, s.h.tenantID, creditnote.CreateInput{
			InvoiceID: tp.inv.ID, Reason: "Refunded in Stripe", RefundAmountCents: cents, AutoIssue: true, StripeRefundID: re,
			Lines: []creditnote.CreditLineInput{{Description: "Refund made in Stripe (" + re + ")", Quantity: 1, UnitAmountCents: cents}},
		})
		if err == nil {
			s.done["refund recorded as a note"]++
			return did + ", recorded"
		}
		if !errors.Is(err, errs.ErrInvalidState) && !errors.Is(err, errs.ErrValidation) {
			s.t.Fatalf("%s: record: %v", did, err)
		}
		did += ", not recorded (" + err.Error() + ")"
	}
	if _, err := s.svc.DismissStripeRefund(s.ctx, s.h.tenantID, tp.inv.ID, re, "handled outside Hardcount"); err != nil {
		s.t.Fatalf("%s: dismiss: %v", did, err)
	}
	return did + ", dismissed"
}

// dispute moves the top-up's dispute to a random status.
func (s *reliefSequence) dispute() string {
	tp := s.top()
	if tp.disputeID == "" {
		s.n++
		tp.disputeID = fmt.Sprintf("du_seq_%d_%d", s.seed, s.n)
	}
	statuses := []domain.DisputeStatus{
		domain.DisputeWarningNeedsResponse, domain.DisputeWarningUnderReview, domain.DisputeWarningClosed,
		domain.DisputeNeedsResponse, domain.DisputeUnderReview, domain.DisputeAppealNeedsResponse,
		domain.DisputeAppealUnderReview, domain.DisputeWon, domain.DisputeLost, domain.DisputePrevented,
		domain.DisputeLost, // twice, so a loss is recorded on every seed
	}
	status := statuses[s.rng.Intn(len(statuses))]
	// The bank takes what the payment has left, or, after part of it went
	// back, one time in three the whole charge.
	wasLost := s.lost(tp)
	amount := tp.price - s.cash(tp)
	if amount < tp.price && s.bank.Intn(3) == 0 {
		amount = tp.price
	}
	if wasLost {
		amount = s.disputeAmount(tp)
	}
	if status == domain.DisputeLost && amount <= 0 {
		status = domain.DisputeWon
	}
	at := s.nextEventAt()
	res, err := s.h.invStore.RecordStripeDispute(s.ctx, s.h.tenantID, domain.DisputeSnapshot{
		StripeEventID: fmt.Sprintf("evt_%s_%d", tp.disputeID, at.Unix()), EventAt: at, StripeDisputeID: tp.disputeID,
		StripePaymentIntentID: tp.pi, Status: status, AmountCents: max(amount, 1), Currency: "usd", OpenedAt: s.eventAt,
	})
	if err != nil {
		s.t.Fatalf("dispute: %v", err)
	}
	s.done["dispute"]++
	if !wasLost && res.Dispute.Status == domain.DisputeLost && res.Dispute.CreditNoteID != "" {
		s.done["loss recorded as a note"]++
	}
	return fmt.Sprintf("dispute %s now %s, of %d", tp.disputeID, status, max(amount, 1))
}

// reliefCash is the price-ratio anchor stated again: f(K+r) - f(K), with
// f(k) = round_half_even(price * k / granted), capped at the price.
func reliefCash(price, granted, k, r int64) int64 {
	f := func(x int64) int64 {
		if x <= 0 {
			return 0
		}
		if x >= granted {
			return price
		}
		v, _ := money.MulDivRoundHalfToEven(price, x, granted)
		return v
	}
	return f(k+r) - f(k)
}

// unusedAt is unused credit stated again: the smaller of what is left of the
// grant, live at its expiry, and the spendable balance less unbilled usage.
func (s *reliefSequence) unusedAt(tp *seqTopup, at time.Time) int64 {
	var live, spendable int64
	s.query(func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(s.ctx, `
			SELECT CASE WHEN expires_at IS NULL OR expires_at >= $2 THEN amount_cents - consumed_cents ELSE 0 END
			FROM customer_credit_ledger WHERE source_invoice_id = $1 AND grant_kind = 'commit'`, tp.inv.ID, at).Scan(&live); err != nil {
			return err
		}
		return tx.QueryRowContext(s.ctx, `
			SELECT COALESCE(SUM(amount_cents), 0) - COALESCE(SUM(CASE WHEN amount_cents > 0 AND consumed_cents < amount_cents
				AND expires_at IS NOT NULL AND expires_at < $2 THEN amount_cents - consumed_cents ELSE 0 END), 0)
			FROM customer_credit_ledger WHERE customer_id = $1 AND currency = 'USD'`, s.h.custID, at).Scan(&spendable)
	})
	return min(live, max(spendable-s.unbilled.cents, 0))
}

// dispute returns the top-up's dispute on its payment, if any.
func (s *reliefSequence) disputeOf(tp *seqTopup) (domain.InvoiceDispute, bool) {
	ds, err := s.h.invStore.ListDisputesByInvoice(s.ctx, s.h.tenantID, tp.inv.ID)
	if err != nil {
		s.t.Fatalf("disputes: %v", err)
	}
	for _, d := range ds {
		if d.StripePaymentIntentID == tp.pi {
			return d, true
		}
	}
	return domain.InvoiceDispute{}, false
}

func (s *reliefSequence) chargebackOpen(tp *seqTopup) bool {
	d, ok := s.disputeOf(tp)
	return ok && !d.Status.Inquiry() && !d.Status.Closed()
}

func (s *reliefSequence) lost(tp *seqTopup) bool {
	d, ok := s.disputeOf(tp)
	return ok && d.Status == domain.DisputeLost
}

func (s *reliefSequence) disputeAmount(tp *seqTopup) int64 {
	d, _ := s.disputeOf(tp)
	return d.AmountCents
}

// issuedTotal adds up the totals of the top-up's issued notes.
func (s *reliefSequence) issuedTotal(tp *seqTopup) int64 {
	var total int64
	for _, cn := range reliefNotes(s.t, s.ctx, s.svc, s.h.tenantID, tp.inv.ID) {
		if cn.Status == domain.CreditNoteIssued {
			total += cn.TotalCents
		}
	}
	return total
}

func (s *reliefSequence) heldByNote(refundID string) bool {
	var held bool
	s.query(func(tx *sql.Tx) error {
		return tx.QueryRowContext(s.ctx, `SELECT EXISTS (SELECT 1 FROM credit_notes WHERE stripe_refund_id = $1)`, refundID).Scan(&held)
	})
	return held
}

// undocumented is the refunds made in Stripe of the top-up's payment that no
// note holds and that moved or may move money.
func (s *reliefSequence) undocumented(tp *seqTopup) int64 {
	var cents int64
	s.query(func(tx *sql.Tx) error {
		return tx.QueryRowContext(s.ctx, `
			SELECT COALESCE(SUM(r.amount_cents), 0) FROM invoice_stripe_refunds r
			WHERE r.invoice_id = $1 AND r.stripe_payment_intent_id = $2
			  AND r.status IN ('succeeded', 'pending', 'requires_action') AND r.credit_note_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM credit_notes h WHERE h.stripe_refund_id = r.stripe_refund_id)`,
			tp.inv.ID, tp.pi).Scan(&cents)
	})
	return cents
}

// cash is the money the top-up's documents and Stripe say went back to the
// customer, or may still: the card (the card refunds of notes not voided,
// failed ones too since a retry may send them, and the refunds made in
// Stripe of the payment that no note holds and that moved or may move
// money), and money returned another way, a loss's note included (the
// bank's money, as much of it as the documents had room for). What the bank
// took beyond that room is the customer's money twice, which Hardcount
// reports and does not return.
func (s *reliefSequence) cash(tp *seqTopup) int64 {
	var cash int64
	s.query(func(tx *sql.Tx) error {
		return tx.QueryRowContext(s.ctx, `
			SELECT
			  (SELECT COALESCE(SUM(n.refund_amount_cents), 0) FROM credit_notes n
			   WHERE n.invoice_id = $1 AND n.status <> 'voided')
			+ (SELECT COALESCE(SUM(r.amount_cents), 0) FROM invoice_stripe_refunds r
			   WHERE r.invoice_id = $1 AND r.stripe_payment_intent_id = $2
			     AND r.status IN ('succeeded', 'pending', 'requires_action') AND r.credit_note_id IS NULL
			     AND NOT EXISTS (SELECT 1 FROM credit_notes h WHERE h.stripe_refund_id = r.stripe_refund_id))
			+ (SELECT COALESCE(SUM(n.out_of_band_amount_cents), 0) FROM credit_notes n
			   WHERE n.invoice_id = $1 AND n.status <> 'voided')`,
			tp.inv.ID, tp.pi).Scan(&cash)
	})
	return cash
}

func (s *reliefSequence) grant(tp *seqTopup) seqGrant {
	var g seqGrant
	s.query(func(tx *sql.Tx) error {
		return tx.QueryRowContext(s.ctx, `
			SELECT id, amount_cents, consumed_cents, cn_retired_cents,
			       CASE WHEN expires_at IS NULL OR expires_at >= now() THEN amount_cents - consumed_cents ELSE 0 END
			FROM customer_credit_ledger
			WHERE source_invoice_id = $1 AND grant_kind = 'commit'`, tp.inv.ID).Scan(&g.id, &g.amount, &g.consumed, &g.cnRetired, &g.live)
	})
	return g
}

func (s *reliefSequence) checkInvariants(did string) {
	for _, tp := range s.tops {
		if cash := s.cash(tp); cash > tp.price {
			s.t.Errorf("invariant 1: after %s, %d went back for %s by every channel, which paid %d", did, cash, tp.inv.ID, tp.price)
		}
		g := s.grant(tp)
		if g.consumed > g.amount || g.cnRetired > g.consumed {
			s.t.Errorf("invariant 2: after %s, grant %s has consumed %d, amount %d, retired by relief %d", did, g.id, g.consumed, g.amount, g.cnRetired)
		}
		// Notes' totals stay within what was paid. While every note is a
		// relief and no refund made in Stripe went back without one, they add
		// up to the anchor of the credit taken back; a note for money
		// returned outside Hardcount takes credit at its own price, and a
		// relief that uses up what is left takes all the unused credit.
		var total, reliefs int64
		onlyReliefs := s.undocumented(tp) == 0
		for _, cn := range reliefNotes(s.t, s.ctx, s.svc, s.h.tenantID, tp.inv.ID) {
			if cn.Status != domain.CreditNoteIssued {
				continue
			}
			total += cn.TotalCents
			if cn.StripeRefundID != "" && cn.RecordedFromStripe || cn.Reason == "Payment reversed in a dispute" {
				onlyReliefs = false
				continue
			}
			reliefs += cn.TotalCents
		}
		anchor := reliefCash(tp.price, tp.granted, 0, g.cnRetired)
		if total > tp.price || reliefs > anchor || (onlyReliefs && total != anchor) {
			s.t.Errorf("invariant 3: after %s, notes of %s add up to %d (reliefs %d) for %d credit taken back, anchor %d, paid %d",
				did, tp.inv.ID, total, reliefs, g.cnRetired, anchor, tp.price)
		}
		// Once the notes return the whole payment, the top-up has no unused
		// credit: what is left of its grant is the usage not invoiced yet,
		// which draws it, and what a void gave back since.
		if total >= tp.price && g.live > s.unbilled.cents+tp.givenBack {
			s.t.Errorf("invariant 7: after %s, the notes of %s return all %d paid, and its grant still has %d (unbilled %d, given back by voids %d)",
				did, tp.inv.ID, tp.price, g.live, s.unbilled.cents, tp.givenBack)
		}
	}
	// Invariants 4 and 5 are checked by each relief, against its own read.
	var blocks, ledger int64
	s.query(func(tx *sql.Tx) error {
		return tx.QueryRowContext(s.ctx, `
			SELECT COALESCE(SUM(CASE WHEN amount_cents > 0 THEN amount_cents - consumed_cents ELSE 0 END), 0), COALESCE(SUM(amount_cents), 0)
			FROM customer_credit_ledger WHERE customer_id = $1 AND currency = 'USD'`, s.h.custID).Scan(&blocks, &ledger)
	})
	if blocks != ledger {
		s.t.Errorf("invariant 6: after %s, the blocks hold %d and the ledger sums to %d", did, blocks, ledger)
	}
}

// query reads on a transaction of the tenant.
func (s *reliefSequence) query(f func(*sql.Tx) error) {
	tx, err := s.h.db.BeginTx(s.ctx, postgres.TxTenant, s.h.tenantID)
	if err != nil {
		s.t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if err := f(tx); err != nil {
		s.t.Fatalf("read: %v", err)
	}
}
