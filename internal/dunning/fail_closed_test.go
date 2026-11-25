package dunning

import (
	"context"
	"errors"
	"testing"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/errs"
)

var errBlip = errors.New("connection reset")

// policyErrStore fails GetPolicy with a read error.
type policyErrStore struct{ *memStore }

func (p policyErrStore) GetPolicy(context.Context, string) (domain.DunningPolicy, error) {
	return domain.DunningPolicy{}, errBlip
}

// A read error on the account's policy fails the start. It is never read as
// "no policy", which skips the invoice for good, so the caller's retry can
// start the run once the database answers.
func TestStartDunning_PolicyReadFailureFailsTheStart(t *testing.T) {
	store := newMemStore()
	svc := NewService(policyErrStore{store}, &noopRetrier{}, nil)
	_, err := svc.StartDunning(context.Background(), "t1", "inv_1", "cus_1", domain.DunningCausePaymentFailed)
	if !errors.Is(err, errBlip) {
		t.Fatalf("err = %v, want the read error", err)
	}
	if errors.Is(err, errs.ErrInvalidState) {
		t.Errorf("err = %v is the deliberate skip; a read error must not be", err)
	}
	if len(store.runs) != 0 {
		t.Errorf("runs = %d, want none", len(store.runs))
	}
}

// seqInvGet serves a scripted sequence of reads; the last entry repeats.
type seqInvGet struct {
	steps []stubInvGet
	calls int
}

func (s *seqInvGet) Get(context.Context, string, string) (domain.Invoice, error) {
	i := s.calls
	if i >= len(s.steps) {
		i = len(s.steps) - 1
	}
	s.calls++
	return s.steps[i].inv, s.steps[i].err
}

// The late paid re-check in exhaustRun guards an IRREVERSIBLE cancel. When
// that read fails, the invoice's state is unknown, so no terminal action may
// fire. The old code fell through, and a second read that succeeded went on
// to cancel — here, the subscription of a customer whose invoice was paid.
func TestExhaust_LateRecheckReadErrorFiresNothing(t *testing.T) {
	store := newMemStore()
	twoAxisPolicy(store, domain.SubActionCancel, domain.InvActionNone)
	svc := NewService(store, &noopRetrier{}, nil)
	paid := unpaidInv()
	paid.Status, paid.PaymentStatus = domain.InvoicePaid, domain.PaymentSucceeded
	getter := &seqInvGet{steps: []stubInvGet{
		{inv: unpaidInv()}, // tick-start pre-check
		{err: errBlip},     // the late re-check
		{inv: paid},        // any later read
	}}
	canceler := &recordingSubCanceler{}
	svc.SetSubscriptionPauser(&recordingPauser{}, getter)
	svc.SetSubscriptionCanceler(canceler)

	run := exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if canceler.calls != 0 {
		t.Fatalf("Cancel calls = %d after an unreadable late re-check, want 0", canceler.calls)
	}
	if got := store.runs[run.ID]; got.State != domain.DunningActive || got.Resolution != domain.ResolutionActionFailed {
		t.Errorf("run = %s/%s, want active/action_failed (re-queued)", got.State, got.Resolution)
	}
}

// activeRunErrStore fails GetActiveRunByInvoice with a read error.
type activeRunErrStore struct{ *memStore }

func (a activeRunErrStore) GetActiveRunByInvoice(context.Context, string, string) (domain.InvoiceDunningRun, error) {
	return domain.InvoiceDunningRun{}, errBlip
}

// A read error is not "no active run": swallowing it left the run active
// beside a paid or voided invoice while the caller saw success.
func TestResolveByInvoice_ReadErrorReturns(t *testing.T) {
	ctx := context.Background()
	svc := NewService(activeRunErrStore{newMemStore()}, &noopRetrier{}, nil)
	if err := svc.ResolveByInvoice(ctx, "t1", "inv_1", domain.ResolutionPaymentRecovered); !errors.Is(err, errBlip) {
		t.Fatalf("err = %v, want the read error", err)
	}
	if err := NewService(newMemStore(), &noopRetrier{}, nil).ResolveByInvoice(ctx, "t1", "inv_none", domain.ResolutionPaymentRecovered); err != nil {
		t.Fatalf("no active run: err = %v, want nil", err)
	}
}

// A policy with no terminal actions: an unreadable invoice fails the tick
// (re-checked next tick) instead of escalating or recording action_failed.
func TestExhaust_NoActionPolicyReadErrorRetries(t *testing.T) {
	store := newMemStore()
	twoAxisPolicy(store, domain.SubActionNone, domain.InvActionNone)
	svc := NewService(store, &noopRetrier{}, nil)
	svc.SetSubscriptionPauser(&recordingPauser{}, &seqInvGet{steps: []stubInvGet{{inv: unpaidInv()}, {err: errBlip}}})
	run := exhaustedRun(t, store, svc)
	_, runErrs := svc.ProcessDueRuns(context.Background(), "t1", 20)
	if len(runErrs) == 0 {
		t.Fatal("want the tick to fail on an unreadable invoice")
	}
	if got := store.runs[run.ID]; got.State != domain.DunningActive || got.Resolution == domain.ResolutionActionFailed {
		t.Errorf("run = %s/%s, want active with no action_failed label", got.State, got.Resolution)
	}
}

// The dunning cancel can void the dunned invoice in its own transaction (an
// invoice whose period was wholly unused, ADR-118). The write-off then has
// nothing to act on: the run resolves as invoice_voided right away instead of
// failing MarkUncollectible on a voided invoice and resolving only on the
// 24h re-attempt.
func TestExhaust_CancelVoidedTheInvoice_ResolvesWithoutWriteOff(t *testing.T) {
	store := newMemStore()
	twoAxisPolicy(store, domain.SubActionCancel, domain.InvActionMarkUncollectible)
	svc := NewService(store, &noopRetrier{}, nil)
	voided := unpaidInv()
	voided.Status = domain.InvoiceVoided
	svc.SetSubscriptionPauser(&recordingPauser{}, &seqInvGet{steps: []stubInvGet{
		{inv: unpaidInv()}, // tick-start pre-check
		{inv: unpaidInv()}, // the late re-check
		{inv: voided},      // after the cancel
	}})
	canceler := &recordingSubCanceler{}
	svc.SetSubscriptionCanceler(canceler)
	uncollect := &capturingUncollect{}
	svc.SetInvoiceUncollectibleMarker(uncollect)

	run := exhaustedRun(t, store, svc)
	svc.ProcessDueRuns(context.Background(), "t1", 20)

	if canceler.calls != 1 {
		t.Fatalf("cancel calls = %d, want 1", canceler.calls)
	}
	if len(uncollect.calls) != 0 {
		t.Errorf("write-off attempted on a voided invoice: %v", uncollect.calls)
	}
	if got := store.runs[run.ID]; got.State != domain.DunningResolved || got.Resolution != domain.ResolutionInvoiceVoided {
		t.Errorf("run = %s/%s, want resolved/invoice_voided", got.State, got.Resolution)
	}
}
