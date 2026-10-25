package subscription

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/testutil"
	"github.com/sagarsuperuser/velox/internal/webhook"
)

// A lifted pause is one whose resumes_at has passed: an operator's timed
// pause reached its date, or the invoice a dunning pause waited for was
// paid. Until the scheduled resumer clears it, the row still holds it.
// Whatever removes it from the row announces its resume, with its own
// trigger: the resumer, a new pause, or an operator's resume call.

// pauseService is the service as the server wires it, on the rig's store:
// the outbox on the store, the real audit log.
func (r *dunningPauseRig) pauseService(store Store) (*Service, *audit.Logger) {
	r.t.Helper()
	outbox := webhook.NewOutboxStore(r.db)
	r.subs.SetOutboxEnqueuer(outbox)
	logger := audit.NewLogger(r.db)
	svc := NewService(store, nil)
	svc.SetAuditLogger(logger)
	return svc, logger
}

// pauseEvent is one collection_paused or collection_resumed outbox row.
type pauseEvent struct {
	eventType   string
	triggeredBy any
	resumedAt   any
	pause       any
}

// pauseEvents reads the rig's collection_paused and collection_resumed
// outbox rows, oldest first.
func (r *dunningPauseRig) pauseEvents() []pauseEvent {
	r.t.Helper()
	rows, err := testutil.AdminPool(r.t).Query(`
		SELECT event_type, payload->'triggered_by', payload->'resumed_at', payload->'subscription'->'pause_collection'
		FROM webhook_outbox
		WHERE event_type IN ($1, $2) AND payload->'subscription'->>'id' = $3
		ORDER BY created_at, id`,
		domain.EventSubscriptionCollectionPaused, domain.EventSubscriptionCollectionResumed, r.subID)
	if err != nil {
		r.t.Fatalf("read outbox: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []pauseEvent
	for rows.Next() {
		var e pauseEvent
		var trig, at, pc []byte
		if err := rows.Scan(&e.eventType, &trig, &at, &pc); err != nil {
			r.t.Fatalf("scan outbox row: %v", err)
		}
		e.triggeredBy, e.resumedAt, e.pause = string(trig), string(at), string(pc)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("read outbox: %v", err)
	}
	return out
}

// pauseAuditRows reads the rig's "Collection paused" and "Collection
// resumed" audit rows, oldest first, as "action/triggered_by".
func (r *dunningPauseRig) pauseAuditRows(logger *audit.Logger) []domain.AuditEntry {
	r.t.Helper()
	rows, _, err := logger.Query(r.ctx, r.tenantID, audit.QueryFilter{ResourceType: "subscription", ResourceID: r.subID})
	if err != nil {
		r.t.Fatalf("query audit: %v", err)
	}
	var out []domain.AuditEntry
	for _, row := range rows {
		if a := row.Metadata["action"]; a == domain.SubAuditCollectionPaused || a == domain.SubAuditCollectionResumed {
			out = append(out, row)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// wantPauseEvents checks the outbox rows' types and triggers, in order.
func (r *dunningPauseRig) wantPauseEvents(want ...string) []pauseEvent {
	r.t.Helper()
	got := r.pauseEvents()
	var gotS []string
	for _, e := range got {
		gotS = append(gotS, e.eventType+"/"+e.triggeredBy.(string))
	}
	if len(gotS) != len(want) {
		r.t.Fatalf("outbox = %v\nwant %v", gotS, want)
	}
	for i := range want {
		if gotS[i] != want[i] {
			r.t.Fatalf("outbox = %v\nwant %v", gotS, want)
		}
	}
	return got
}

// wantPauseAudit checks the audit rows' actions and triggers, in order.
func (r *dunningPauseRig) wantPauseAudit(logger *audit.Logger, want ...string) []domain.AuditEntry {
	r.t.Helper()
	got := r.pauseAuditRows(logger)
	var gotS []string
	for _, e := range got {
		gotS = append(gotS, e.Metadata["action"].(string)+"/"+e.Metadata["triggered_by"].(string))
	}
	if len(gotS) != len(want) {
		r.t.Fatalf("audit rows = %v\nwant %v", gotS, want)
	}
	for i := range want {
		if gotS[i] != want[i] {
			r.t.Fatalf("audit rows = %v\nwant %v", gotS, want)
		}
	}
	return got
}

// wantResumedAt checks a resume's resumed_at, in the event and in the
// audit row, against when the pause lifted.
func wantResumedAt(t *testing.T, ev pauseEvent, row domain.AuditEntry, lifted time.Time) {
	t.Helper()
	var at time.Time
	if err := at.UnmarshalJSON([]byte(ev.resumedAt.(string))); err != nil || !at.Equal(lifted) {
		t.Errorf("event resumed_at = %v (%v), want %v", ev.resumedAt, err, lifted)
	}
	if got, err := time.Parse(time.RFC3339Nano, row.Metadata["resumed_at"].(string)); err != nil || !got.Equal(lifted) {
		t.Errorf("audit resumed_at = %v (%v), want %v", row.Metadata["resumed_at"], err, lifted)
	}
	if ev.pause != "" && ev.pause != "null" {
		t.Errorf("resumed event pause_collection = %v, want null: the resume's row has no pause", ev.pause)
	}
}

const (
	pausedDunning   = domain.EventSubscriptionCollectionPaused + `/"dunning"`
	resumedPayment  = domain.EventSubscriptionCollectionResumed + `/"payment"`
	resumedSchedule = domain.EventSubscriptionCollectionResumed + `/"schedule"`

	auditPausedDunning   = domain.SubAuditCollectionPaused + "/dunning"
	auditPausedOperator  = domain.SubAuditCollectionPaused + "/operator"
	auditResumedPayment  = domain.SubAuditCollectionResumed + "/payment"
	auditResumedSchedule = domain.SubAuditCollectionResumed + "/schedule"
)

// pay marks inv paid and returns when the pause lifted: the resumes_at
// the payment stamped (the instant it recorded the payment).
func (r *dunningPauseRig) pay(ctx context.Context, inv string) time.Time {
	r.t.Helper()
	if _, err := r.invs.MarkPaid(ctx, r.tenantID, inv, "", clock.Now(ctx)); err != nil {
		r.t.Fatalf("pay %s: %v", inv, err)
	}
	pc := r.pause()
	if pc == nil || pc.ResumesAt == nil {
		r.t.Fatalf("pause = %+v after paying %s, want it lifted", pc, inv)
	}
	return *pc.ResumesAt
}

// Review round 2, finding 1. Dunning pauses for invoice A, A is paid (the
// pause lifts), and before the resumer clears it, dunning pauses for
// invoice B. The new pause replaces the lifted one and announces its
// resume first, as the resumer would have: paused, resumed by payment,
// paused, each with its audit row.
func TestLiftedPause_ANewPauseAnnouncesItsResume(t *testing.T) {
	t.Run("dunning pause", func(t *testing.T) {
		r := newDunningPauseRig(t, "Lifted Pause Dunning Replaces")
		svc, logger := r.pauseService(r.subs)
		a, b := r.invoice("INV-LP-A1", 1000), r.invoice("INV-LP-B1", 1000)
		if applied, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, a); err != nil || !applied {
			t.Fatalf("pause for A = %v, %v; want applied", applied, err)
		}
		paid := r.pay(r.ctx, a)
		if applied, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, b); err != nil || !applied {
			t.Fatalf("pause for B = %v, %v; want applied", applied, err)
		}

		evs := r.wantPauseEvents(pausedDunning, resumedPayment, pausedDunning)
		rows := r.wantPauseAudit(logger, auditPausedDunning, auditResumedPayment, auditPausedDunning)
		wantResumedAt(t, evs[1], rows[1], paid)
		if rows[2].Metadata["invoice_id"] != b {
			t.Errorf("second pause's invoice_id = %v, want B %s", rows[2].Metadata["invoice_id"], b)
		}
		if pc := r.pause(); pc == nil || pc.ResumesAt != nil || pc.SetBy != domain.PauseSetByDunning {
			t.Errorf("pause = %+v, want B's dunning pause in force", pc)
		}
	})

	t.Run("operator pause", func(t *testing.T) {
		r := newDunningPauseRig(t, "Lifted Pause Operator Replaces")
		svc, logger := r.pauseService(r.subs)
		a := r.invoice("INV-LP-A2", 1000)
		if applied, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, a); err != nil || !applied {
			t.Fatalf("pause for A = %v, %v; want applied", applied, err)
		}
		paid := r.pay(r.ctx, a)
		if _, err := svc.PauseCollection(r.ctx, r.tenantID, r.subID, PauseCollectionInput{Behavior: domain.PauseCollectionKeepAsDraft}); err != nil {
			t.Fatalf("operator pause: %v", err)
		}

		// The operator's own collection_paused is sent by the handler.
		evs := r.wantPauseEvents(pausedDunning, resumedPayment)
		rows := r.wantPauseAudit(logger, auditPausedDunning, auditResumedPayment, auditPausedOperator)
		wantResumedAt(t, evs[1], rows[1], paid)
	})
}

// An operator's resume call on a lifted pause resumes nothing itself: the
// pause already ended. The lift's resume is announced, with its trigger.
func TestLiftedPause_AResumeCallAnnouncesTheLift(t *testing.T) {
	r := newDunningPauseRig(t, "Lifted Pause Resume Call")
	svc, logger := r.pauseService(r.subs)
	a := r.invoice("INV-LP-A3", 1000)
	if applied, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, a); err != nil || !applied {
		t.Fatalf("pause for A = %v, %v; want applied", applied, err)
	}
	paid := r.pay(r.ctx, a)
	sub, resumed, err := svc.ResumeCollection(r.ctx, r.tenantID, r.subID)
	if err != nil {
		t.Fatalf("resume call: %v", err)
	}
	if resumed {
		t.Error("resume call on a lifted pause reported resumed = true, want false: the handler would send triggered_by operator")
	}
	if sub.PauseCollection != nil {
		t.Errorf("pause = %+v after the resume call, want none", sub.PauseCollection)
	}
	evs := r.wantPauseEvents(pausedDunning, resumedPayment)
	rows := r.wantPauseAudit(logger, auditPausedDunning, auditResumedPayment)
	wantResumedAt(t, evs[1], rows[1], paid)
}

// raceAfterList is the concurrent-resolver fake for the scheduled resumer:
// after its list, and before its clear, it runs the racing writes.
type raceAfterList struct {
	*PostgresStore
	race func()
}

func (s *raceAfterList) ListExpiredPauseCollections(ctx context.Context, before time.Time, limit int) ([]domain.Subscription, error) {
	subs, err := s.PostgresStore.ListExpiredPauseCollections(ctx, before, limit)
	if race := s.race; race != nil {
		s.race = nil
		race()
	}
	return subs, err
}

// Review round 2, finding 2. The resumer lists a lifted pause, and before
// its clear, another writer replaces the pause. The clear is a
// compare-and-set on the pause it listed: it misses, leaves the new pause
// and announces nothing. Mutation: drop either part of the key and a case
// fails.
func TestScheduledResumer_ClearsOnlyThePauseItListed(t *testing.T) {
	t.Run("a new dunning pause", func(t *testing.T) {
		r := newDunningPauseRig(t, "Resumer Race New Pause")
		race := &raceAfterList{PostgresStore: r.subs}
		svc, logger := r.pauseService(r.subs)
		resumer, _ := r.pauseService(race)
		a, b := r.invoice("INV-RR-A1", 1000), r.invoice("INV-RR-B1", 1000)
		if _, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, a); err != nil {
			t.Fatalf("pause for A: %v", err)
		}
		paid := r.pay(r.ctx, a)
		race.race = func() {
			if applied, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, b); err != nil || !applied {
				t.Fatalf("pause for B = %v, %v; want applied", applied, err)
			}
		}
		if _, errs := resumer.ProcessExpiredPauseCollections(r.ctx, 100); len(errs) > 0 {
			t.Fatalf("resumer: %v", errs)
		}

		if pc := r.pause(); pc == nil || pc.ResumesAt != nil || pc.SetBy != domain.PauseSetByDunning {
			t.Fatalf("pause = %+v, want B's dunning pause, untouched: B is not paid", pc)
		}
		evs := r.wantPauseEvents(pausedDunning, resumedPayment, pausedDunning)
		rows := r.wantPauseAudit(logger, auditPausedDunning, auditResumedPayment, auditPausedDunning)
		wantResumedAt(t, evs[1], rows[1], paid)
	})

	t.Run("a new dunning pause, paid too", func(t *testing.T) {
		r := newDunningPauseRig(t, "Resumer Race New Pause Paid")
		race := &raceAfterList{PostgresStore: r.subs}
		svc, logger := r.pauseService(r.subs)
		resumer, _ := r.pauseService(race)
		a, b := r.invoice("INV-RR-A2", 1000), r.invoice("INV-RR-B2", 1000)
		if _, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, a); err != nil {
			t.Fatalf("pause for A: %v", err)
		}
		paidA := r.pay(r.ctx, a)
		var paidB time.Time
		race.race = func() {
			if _, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, b); err != nil {
				t.Fatalf("pause for B: %v", err)
			}
			paidB = r.pay(r.ctx, b)
		}
		if _, errs := resumer.ProcessExpiredPauseCollections(r.ctx, 100); len(errs) > 0 {
			t.Fatalf("resumer: %v", errs)
		}
		// The stale clear listed A's pause; the row holds B's, lifted later.
		if pc := r.pause(); pc == nil || pc.ResumesAt == nil || !pc.ResumesAt.Equal(paidB) {
			t.Fatalf("pause = %+v after the stale clear, want B's lifted pause still on the row", pc)
		}
		r.wantPauseEvents(pausedDunning, resumedPayment, pausedDunning)

		// The next run resumes B, at B's payment.
		if _, errs := resumer.ProcessExpiredPauseCollections(r.ctx, 100); len(errs) > 0 {
			t.Fatalf("second resumer run: %v", errs)
		}
		evs := r.wantPauseEvents(pausedDunning, resumedPayment, pausedDunning, resumedPayment)
		rows := r.wantPauseAudit(logger, auditPausedDunning, auditResumedPayment, auditPausedDunning, auditResumedPayment)
		wantResumedAt(t, evs[1], rows[1], paidA)
		wantResumedAt(t, evs[3], rows[3], paidB)
		if pc := r.pause(); pc != nil {
			t.Errorf("pause = %+v after the second run, want none", pc)
		}
	})

	// On a test clock, a payment can land at the same instant an operator's
	// timed pause reached its date: the pause's resumes_at is the same, and
	// only who set it differs.
	t.Run("an operator's pause replaced by a dunning pause lifted at the same instant", func(t *testing.T) {
		r := newDunningPauseRig(t, "Resumer Race Same Instant")
		race := &raceAfterList{PostgresStore: r.subs}
		svc, logger := r.pauseService(r.subs)
		resumer, _ := r.pauseService(race)
		b := r.invoice("INV-RR-B3", 1000)
		lifted := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
		// The store takes a past resumes_at (the API refuses one): the
		// operator's pause has reached its date.
		if _, err := testutil.AdminPool(t).Exec(`UPDATE subscriptions SET pause_collection_behavior = 'keep_as_draft',
			pause_collection_resumes_at = $1 WHERE id = $2`, lifted, r.subID); err != nil {
			t.Fatalf("operator's timed pause: %v", err)
		}
		race.race = func() {
			if _, err := svc.PauseCollectionForDunning(r.ctx, r.tenantID, r.subID, b); err != nil {
				t.Fatalf("pause for B: %v", err)
			}
			// The payment is recorded at the instant the operator's pause
			// reached its date, as a test-clock advance can do.
			if got := r.pay(clock.WithEffectiveNow(r.ctx, lifted), b); !got.Equal(lifted) {
				t.Fatalf("B lifted at %v, want %v", got, lifted)
			}
		}
		if _, errs := resumer.ProcessExpiredPauseCollections(r.ctx, 100); len(errs) > 0 {
			t.Fatalf("resumer: %v", errs)
		}
		if _, errs := resumer.ProcessExpiredPauseCollections(r.ctx, 100); len(errs) > 0 {
			t.Fatalf("second resumer run: %v", errs)
		}
		r.wantPauseEvents(resumedSchedule, pausedDunning, resumedPayment)
		r.wantPauseAudit(logger, auditResumedSchedule, auditPausedDunning, auditResumedPayment)
	})
}
