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
// a seeded random sequence of reliefs, usage and its close, expiry, refunds
// made in Stripe and disputes, on one customer with top-ups (every other one
// discounted) and promotional credit. Six invariants are checked after every
// step. A failure names its seed; -relief-seed replays it.
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
}

type reliefSequence struct {
	t        *testing.T
	seed     int64
	h        *commitHarness
	ctx      context.Context
	rng      *rand.Rand
	svc      *creditnote.Service
	unbilled *unbilledUsage
	tops     []*seqTopup
	promoID  string
	eventAt  time.Time          // the next Stripe event's time; each event is a second later
	refunds  []stripeRefundMade // refunds made in Stripe that succeeded
	n        int
}

type stripeRefundMade struct {
	id, pi string
	cents  int64
}

// seqGrant is a top-up's grant as the ledger holds it.
type seqGrant struct {
	id                          string
	amount, consumed, cnRetired int64
}

func newReliefSequence(t *testing.T, seed int64) *reliefSequence {
	h, ctx := newCommitHarness(t, "Relief Sequence")
	s := &reliefSequence{t: t, seed: seed, h: h, ctx: ctx, rng: rand.New(rand.NewSource(seed)),
		unbilled: &unbilledUsage{}, eventAt: time.Now().UTC().Truncate(time.Second)}
	s.svc = newCardReliefService(h, &stripeRefunds{})
	s.svc.SetUnbilledUsageReader(s.unbilled)
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

func (s *reliefSequence) run(steps int) {
	for step := 0; step < steps; step++ {
		did := s.step()
		s.checkInvariants(did)
		if s.t.Failed() {
			s.t.Fatalf("seed %d failed at step %d: %s", s.seed, step, did)
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
	case k < 64:
		return s.close()
	case k < 67:
		return s.expire()
	case k < 71:
		return s.buy()
	case k < 86:
		return s.stripeRefund()
	default:
		return s.dispute()
	}
}

func (s *reliefSequence) relief() string {
	tp := s.top()
	at := time.Now().UTC().Truncate(time.Microsecond)
	unused := s.unusedAt(tp, at)
	blocked := s.chargebackOrLoss(tp)
	k := s.grant(tp).cnRetired

	in := creditnote.CommitReliefInput{InvoiceID: tp.inv.ID}
	if s.rng.Intn(20) == 0 {
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
	cn, err := s.svc.CreateAndIssueCommitRelief(clock.WithEffectiveNow(s.ctx, at), s.h.tenantID, in)
	did := fmt.Sprintf("relief of %s (retire %d, all %v, unused %d)", tp.inv.ID, in.RetireCents, in.RetireAll, unused)
	if err != nil {
		if !errors.Is(err, errs.ErrInvalidState) && !errors.Is(err, errs.ErrValidation) {
			s.t.Fatalf("%s: unexpected error: %v", did, err)
		}
		return did + ": refused: " + err.Error()
	}
	if blocked {
		s.t.Errorf("invariant 4: %s was issued while a chargeback is open or lost", did)
	}
	if cn.CommitRetiredCents > unused {
		s.t.Errorf("invariant 5: %s took %d credit, more than the %d unused at its read", did, cn.CommitRetiredCents, unused)
	}
	return fmt.Sprintf("%s: took %d for %d cash", did, cn.CommitRetiredCents, cn.TotalCents)
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
	return did
}

// expire moves a grant past its expiry, as time does, and sometimes sweeps.
func (s *reliefSequence) expire() string {
	id := s.promoID
	if s.rng.Intn(2) == 0 {
		id = s.grant(s.top()).id
	}
	execBypassStamp(s.t, s.h.db, `UPDATE customer_credit_ledger SET expires_at = LEAST(COALESCE(expires_at, NOW()), NOW()) - INTERVAL '1 millisecond' WHERE id = $1`, id)
	did := "expiry of " + id
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
// earlier one fails after it succeeded.
func (s *reliefSequence) stripeRefund() string {
	if len(s.refunds) > 0 && s.rng.Intn(4) == 0 {
		re := s.refunds[s.rng.Intn(len(s.refunds))]
		recordStripeRefund(s.t, s.ctx, s.svc, s.h.tenantID, re.id, re.pi, re.cents, domain.StripeRefundFailed, s.nextEventAt())
		return "refund " + re.id + " failed after it succeeded"
	}
	tp := s.top()
	left := tp.price - s.cardRefunded(tp)
	if left <= 0 {
		return "no refund: the card is fully refunded"
	}
	statuses := []domain.StripeRefundStatus{domain.StripeRefundSucceeded, domain.StripeRefundPending, domain.StripeRefundRequiresAction, domain.StripeRefundFailed}
	status := statuses[s.rng.Intn(len(statuses))]
	s.n++
	re := fmt.Sprintf("re_seq_%d_%d", s.seed, s.n)
	cents := 1 + s.rng.Int63n(min(left, 20000))
	recordStripeRefund(s.t, s.ctx, s.svc, s.h.tenantID, re, tp.pi, cents, status, s.nextEventAt())
	if status == domain.StripeRefundSucceeded {
		s.refunds = append(s.refunds, stripeRefundMade{re, tp.pi, cents})
	}
	return fmt.Sprintf("refund %s of %d made in Stripe, %s", re, cents, status)
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
	}
	status := statuses[s.rng.Intn(len(statuses))]
	at := s.nextEventAt()
	if _, err := s.h.invStore.RecordStripeDispute(s.ctx, s.h.tenantID, domain.DisputeSnapshot{
		StripeEventID: fmt.Sprintf("evt_%s_%d", tp.disputeID, at.Unix()), EventAt: at, StripeDisputeID: tp.disputeID,
		StripePaymentIntentID: tp.pi, Status: status, AmountCents: tp.price, Currency: "usd", OpenedAt: s.eventAt,
	}); err != nil {
		s.t.Fatalf("dispute: %v", err)
	}
	return fmt.Sprintf("dispute %s now %s", tp.disputeID, status)
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

func (s *reliefSequence) chargebackOrLoss(tp *seqTopup) bool {
	ds, err := s.h.invStore.ListDisputesByInvoice(s.ctx, s.h.tenantID, tp.inv.ID)
	if err != nil {
		s.t.Fatalf("disputes: %v", err)
	}
	for _, d := range ds {
		switch d.Status {
		case domain.DisputeNeedsResponse, domain.DisputeUnderReview, domain.DisputeAppealNeedsResponse,
			domain.DisputeAppealUnderReview, domain.DisputeLost:
			return true
		}
	}
	return false
}

// cardRefunded is what has left the card or may still leave it: the refunds
// of issued notes that did not fail, and the refunds made in Stripe of the
// same payment that moved or may move money and that no note holds.
func (s *reliefSequence) cardRefunded(tp *seqTopup) int64 {
	var notes, stripe int64
	s.query(func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(s.ctx, `
			SELECT COALESCE(SUM(refund_amount_cents), 0) FROM credit_notes
			WHERE invoice_id = $1 AND status = 'issued' AND COALESCE(refund_status, 'none') IN ('none', 'pending', 'succeeded', 'unknown')`,
			tp.inv.ID).Scan(&notes); err != nil {
			return err
		}
		return tx.QueryRowContext(s.ctx, `
			SELECT COALESCE(SUM(r.amount_cents), 0) FROM invoice_stripe_refunds r
			WHERE r.invoice_id = $1 AND r.stripe_payment_intent_id = $2
			  AND r.status IN ('succeeded', 'pending', 'requires_action') AND r.credit_note_id IS NULL
			  AND NOT EXISTS (SELECT 1 FROM credit_notes n WHERE n.stripe_refund_id = r.stripe_refund_id)`,
			tp.inv.ID, tp.pi).Scan(&stripe)
	})
	return notes + stripe
}

func (s *reliefSequence) grant(tp *seqTopup) seqGrant {
	var g seqGrant
	s.query(func(tx *sql.Tx) error {
		return tx.QueryRowContext(s.ctx, `
			SELECT id, amount_cents, consumed_cents, cn_retired_cents FROM customer_credit_ledger
			WHERE source_invoice_id = $1 AND grant_kind = 'commit'`, tp.inv.ID).Scan(&g.id, &g.amount, &g.consumed, &g.cnRetired)
	})
	return g
}

func (s *reliefSequence) checkInvariants(did string) {
	for _, tp := range s.tops {
		if card := s.cardRefunded(tp); card > tp.price {
			s.t.Errorf("invariant 1: after %s, %d is refunded to the card of %s, which paid %d", did, card, tp.inv.ID, tp.price)
		}
		g := s.grant(tp)
		if g.consumed > g.amount || g.cnRetired > g.consumed {
			s.t.Errorf("invariant 2: after %s, grant %s has consumed %d, amount %d, retired by relief %d", did, g.id, g.consumed, g.amount, g.cnRetired)
		}
		var cash int64
		for _, cn := range reliefNotes(s.t, s.ctx, s.svc, s.h.tenantID, tp.inv.ID) {
			if cn.Status == domain.CreditNoteIssued {
				cash += cn.TotalCents
			}
		}
		if want := reliefCash(tp.price, tp.granted, 0, g.cnRetired); cash != want || cash > tp.price {
			s.t.Errorf("invariant 3: after %s, relief of %s paid %d for %d credit, want %d of at most %d", did, tp.inv.ID, cash, g.cnRetired, want, tp.price)
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
