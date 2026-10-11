package dunning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
)

var errBlip = errors.New("connection reset")

type stubPolicyReader struct {
	pid string
	err error
}

func (s stubPolicyReader) GetDunningPolicyID(context.Context, string, string) (string, error) {
	return s.pid, s.err
}

// policyErrStore fails GetPolicyByID with a read error.
type policyErrStore struct{ *memStore }

func (p policyErrStore) GetPolicyByID(context.Context, string, string) (domain.DunningPolicy, error) {
	return domain.DunningPolicy{}, errBlip
}

// A run stores its policy for life, so a read error resolving the customer's
// policy must fail — not silently bind the run to the tenant default. Only a
// policy that is genuinely not set (no id, or an assigned id that no longer
// exists) uses the default.
func TestGetEffectivePolicy_ReadErrorFailsNotSetDefaults(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	svc := NewService(store, &noopRetrier{}, nil)

	svc.SetCustomerPolicyReader(stubPolicyReader{err: errBlip})
	if _, err := svc.GetEffectivePolicyForCustomer(ctx, "t1", "cus_1"); !errors.Is(err, errBlip) {
		t.Errorf("customer read fails: err = %v, want the read error", err)
	}

	svc = NewService(policyErrStore{store}, &noopRetrier{}, nil)
	svc.SetCustomerPolicyReader(stubPolicyReader{pid: "dpol_custom"})
	if _, err := svc.GetEffectivePolicyForCustomer(ctx, "t1", "cus_1"); !errors.Is(err, errBlip) {
		t.Errorf("policy read fails: err = %v, want the read error", err)
	}

	svc = NewService(store, &noopRetrier{}, nil)
	for name, r := range map[string]stubPolicyReader{
		"no assignment":        {},
		"customer not found":   {err: errs.ErrNotFound},
		"assigned policy gone": {pid: "dpol_deleted"},
	} {
		svc.SetCustomerPolicyReader(r)
		p, err := svc.GetEffectivePolicyForCustomer(ctx, "t1", "cus_1")
		if err != nil || p.ID != "dpol_1" {
			t.Errorf("%s: got %q, %v; want the default policy", name, p.ID, err)
		}
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

// An operator resolution whose invoice action fails must leave the run
// active (dunning keeps working the invoice) and report the error. The old
// handler resolved the run first, logged the failure and returned 200, so a
// voided-in-name run sat beside an open invoice nothing would ever collect.
func TestResolveRun_InvoiceActionFailureLeavesRunActive(t *testing.T) {
	for _, resolution := range []domain.DunningResolution{domain.ResolutionPaymentRecovered, domain.ResolutionInvoiceVoided} {
		t.Run(string(resolution), func(t *testing.T) {
			ctx := context.Background()
			store := newMemStore()
			svc := NewService(store, &noopRetrier{}, nil)
			run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
			if err != nil {
				t.Fatalf("StartDunning: %v", err)
			}
			h := NewHandler(svc, HandlerDeps{})
			h.SetInvoiceVoider(&recordingVoider{err: errBlip})

			body, _ := json.Marshal(resolveInput{Resolution: string(resolution)})
			r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", run.ID)
			r = r.WithContext(context.WithValue(auth.WithTenantID(r.Context(), "t1"), chi.RouteCtxKey, rctx))
			rec := httptest.NewRecorder()
			h.resolveRun(rec, r)

			if rec.Code == http.StatusOK {
				t.Errorf("status = 200 after the invoice action failed, want an error")
			}
			if got := store.runs[run.ID]; got.State != domain.DunningActive {
				t.Errorf("run state = %s, want active", got.State)
			}
		})
	}
}

// resolveVia drives the operator resolve endpoint for run id.
func resolveVia(h *Handler, id string, resolution domain.DunningResolution) *httptest.ResponseRecorder {
	body, _ := json.Marshal(resolveInput{Resolution: string(resolution)})
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	r = r.WithContext(context.WithValue(auth.WithTenantID(r.Context(), "t1"), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.resolveRun(rec, r)
	return rec
}

// An invoice already in the requested end state is done, not an error: the
// operator's retry after a failed run update (or a race with a webhook) must
// close the run — an escalated run has no tick that would. The writer that
// would refuse ("already paid" / "already voided") is never called.
func TestResolveRun_InvoiceAlreadyInEndStateResolves(t *testing.T) {
	for resolution, status := range map[domain.DunningResolution]domain.InvoiceStatus{
		domain.ResolutionPaymentRecovered: domain.InvoicePaid,
		domain.ResolutionInvoiceVoided:    domain.InvoiceVoided,
	} {
		t.Run(string(resolution), func(t *testing.T) {
			ctx := context.Background()
			store := newMemStore()
			svc := NewService(store, &noopRetrier{}, nil)
			run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
			if err != nil {
				t.Fatalf("StartDunning: %v", err)
			}
			inv := unpaidInv()
			inv.Status = status
			svc.SetSubscriptionPauser(&recordingPauser{}, &seqInvGet{steps: []stubInvGet{{inv: inv}}})
			voider := &recordingVoider{err: errs.InvalidState("invoice is already " + string(status))}
			h := NewHandler(svc, HandlerDeps{})
			h.SetInvoiceVoider(voider)

			if rec := resolveVia(h, run.ID, resolution); rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
			}
			if voider.calls+voider.recordCalls != 0 {
				t.Errorf("writer called %d time(s) on an invoice already %s", voider.calls+voider.recordCalls, status)
			}
			if got := store.runs[run.ID]; got.State != domain.DunningResolved || got.Resolution != resolution {
				t.Errorf("run = %s/%s, want resolved/%s", got.State, got.Resolution, resolution)
			}
		})
	}
}

// A failed write-off leaves the run open (it used to resolve the run first,
// leaving an open invoice nothing revisits); an invoice already written off
// resolves without writing again.
func TestResolveRun_NotCollectibleWritesOffFirst(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T, status domain.InvoiceStatus, markErr error) (*memStore, *Service, *capturingUncollect, string) {
		store := newMemStore()
		svc := NewService(store, &noopRetrier{}, nil)
		run, err := svc.StartDunning(ctx, "t1", "inv_1", "cus_1", time.Now(), domain.DunningCausePaymentFailed)
		if err != nil {
			t.Fatalf("StartDunning: %v", err)
		}
		inv := unpaidInv()
		inv.Status = status
		svc.SetSubscriptionPauser(&recordingPauser{}, &seqInvGet{steps: []stubInvGet{{inv: inv}}})
		u := &capturingUncollect{err: markErr}
		svc.SetInvoiceUncollectibleMarker(u)
		return store, svc, u, run.ID
	}

	store, svc, _, id := start(t, domain.InvoiceFinalized, errBlip)
	if _, err := svc.ResolveRun(ctx, "t1", id, domain.ResolutionInvoiceNotCollectible); !errors.Is(err, errBlip) {
		t.Fatalf("err = %v, want the write-off error", err)
	}
	if got := store.runs[id]; got.State != domain.DunningActive {
		t.Errorf("run state = %s after a failed write-off, want active", got.State)
	}

	store, svc, u, id := start(t, domain.InvoiceUncollectible, nil)
	if _, err := svc.ResolveRun(ctx, "t1", id, domain.ResolutionInvoiceNotCollectible); err != nil {
		t.Fatalf("already written off: %v", err)
	}
	if len(u.calls) != 0 || store.runs[id].State != domain.DunningResolved {
		t.Errorf("write-off calls = %d, run = %s; want 0 and resolved", len(u.calls), store.runs[id].State)
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
