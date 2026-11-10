package webhook

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/leader"
	"github.com/gethardcount/hardcount/internal/platform/leader/leadertest"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
	"github.com/gethardcount/hardcount/internal/testutil"
)

// Real-Postgres tests for the three guards of the delivery worker: a panic
// in an attempt, a mark made after its tick has ended, and a delivery that
// never records an outcome. Faults enter through the two seams production
// has: the HTTPClient and the Store passed to NewService.

// guardFixture is one tenant with one endpoint.
type guardFixture struct {
	// tick is what the retry worker's tick gets from the scheduler: a live
	// webhook_delivery lease and no mode. The worker takes each row's mode
	// from the row.
	tick context.Context
	// ctx is tick in test mode, for the test's own writes and reads.
	ctx      context.Context
	db       *postgres.DB
	tenantID string
	store    *PostgresStore
	endpoint domain.WebhookEndpoint
}

func newGuardFixture(t *testing.T, tenantName string) guardFixture {
	t.Helper()
	db := testutil.SetupTestDB(t)
	tick, cancel := context.WithTimeout(
		leadertest.Token(t, testutil.AdminPool(t), context.Background(), leader.RoleWebhookDelivery),
		60*time.Second)
	t.Cleanup(cancel)
	ctx := postgres.WithLivemode(tick, false)
	tenantID := testutil.CreateTestTenant(t, db, tenantName)
	return guardFixture{
		tick:     tick,
		ctx:      ctx,
		db:       db,
		tenantID: tenantID,
		store:    NewPostgresStore(db),
		endpoint: seedEndpoint(t, db, ctx, tenantID),
	}
}

// exec runs one statement as the tenant. The tests use it only for states
// that production reaches by waiting: an ended lease, an old row.
func (f guardFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(f.ctx, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// endLease moves a delivery's lease into the past, as waiting it out would,
// so the next retry pass claims the row.
func (f guardFixture) endLease(t *testing.T, deliveryID string) {
	t.Helper()
	f.exec(t, `UPDATE webhook_deliveries SET next_retry_at = now() - interval '1 minute' WHERE id = $1`, deliveryID)
}

// claimableDelivery creates an event with one delivery and ends its birth
// lease. That is the row a first attempt leaves when its process dies: the
// retry pass makes the attempt.
func (f guardFixture) claimableDelivery(t *testing.T) domain.WebhookDelivery {
	t.Helper()
	_, deliveries, err := f.store.CreateEventWithDeliveries(f.ctx, f.tenantID, domain.WebhookEvent{
		EventType: "invoice.paid", Payload: map[string]any{"invoice_id": "hc_inv_1"},
	}, []string{f.endpoint.ID}, birthLeaseWindow, "")
	if err != nil {
		t.Fatalf("create event with deliveries: %v", err)
	}
	f.endLease(t, deliveries[0].ID)
	return deliveries[0]
}

func (f guardFixture) delivery(t *testing.T, id string) domain.WebhookDelivery {
	t.Helper()
	d, err := f.store.GetDelivery(f.ctx, f.tenantID, id)
	if err != nil {
		t.Fatalf("read delivery %s: %v", id, err)
	}
	return d
}

// onlyDelivery returns the tenant's one delivery, for a test that did not
// create the row itself.
func (f guardFixture) onlyDelivery(t *testing.T) domain.WebhookDelivery {
	t.Helper()
	tx, err := f.db.BeginTx(f.ctx, postgres.TxTenant, f.tenantID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer postgres.Rollback(tx)
	var id string
	if err := tx.QueryRowContext(f.ctx, `SELECT id FROM webhook_deliveries`).Scan(&id); err != nil {
		t.Fatalf("read the one delivery: %v", err)
	}
	return f.delivery(t, id)
}

// retryPass runs one pass of the retry worker on ctx, a tick's context.
func retryPass(t *testing.T, svc *Service, ctx context.Context) {
	t.Helper()
	if err := svc.RetryPendingDeliveries(ctx); err != nil {
		t.Fatalf("retry pass: %v", err)
	}
}

// hookedStore is the real store with faults a test switches on.
type hookedStore struct {
	Store
	// getEndpoint, when set, replaces the endpoint read.
	getEndpoint func() (domain.WebhookEndpoint, error)
	// updateDelivery, when set, replaces the delivery mark.
	updateDelivery func() (domain.WebhookDelivery, error)
	// marked, when set, gets each mark's error after the mark returns. The
	// first attempt runs in a goroutine nothing waits for, so a test waits
	// here for its mark.
	marked chan error
}

func (s *hookedStore) GetEndpoint(ctx context.Context, tenantID, id string) (domain.WebhookEndpoint, error) {
	if s.getEndpoint != nil {
		return s.getEndpoint()
	}
	return s.Store.GetEndpoint(ctx, tenantID, id)
}

func (s *hookedStore) UpdateDelivery(ctx context.Context, tenantID string, d domain.WebhookDelivery, expectedAttempts int) (domain.WebhookDelivery, error) {
	if s.updateDelivery != nil {
		return s.updateDelivery()
	}
	out, err := s.Store.UpdateDelivery(ctx, tenantID, d, expectedAttempts)
	if s.marked != nil {
		s.marked <- err
	}
	return out, err
}

// panickingClient panics on every POST, as a bug in the send path would,
// and counts the POSTs.
type panickingClient struct{ posts atomic.Int32 }

func (c *panickingClient) Do(*http.Request) (*http.Response, error) {
	c.posts.Add(1)
	panic("send path bug")
}

// countingClient answers every POST with status and counts the POSTs.
type countingClient struct {
	status int
	posts  atomic.Int32
}

func (c *countingClient) Do(*http.Request) (*http.Response, error) {
	c.posts.Add(1)
	return &http.Response{StatusCode: c.status, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}

// wantRetryScheduled checks that d counts attempts failed attempts and waits
// for the retry the ladder gives after that many, not for the end of a
// lease. sent is a time before the attempt started.
func wantRetryScheduled(t *testing.T, d domain.WebhookDelivery, attempts int, sent time.Time) {
	t.Helper()
	if d.Status != domain.DeliveryPending || d.AttemptCount != attempts {
		t.Fatalf("delivery status/attempts = %s/%d, want pending/%d: the failed attempt was not counted", d.Status, d.AttemptCount, attempts)
	}
	if d.NextRetryAt == nil {
		t.Fatal("next_retry_at is NULL, want the ladder's next retry")
	}
	// The ladder's wait, plus up to 30 seconds of jitter.
	wait := retryBackoffs[attempts-1]
	earliest, latest := sent.Add(wait), time.Now().Add(wait+30*time.Second)
	if d.NextRetryAt.Before(earliest) || d.NextRetryAt.After(latest) {
		t.Errorf("next_retry_at = %v, want between %v and %v: the row was not put on the retry ladder", d.NextRetryAt, earliest, latest)
	}
}

// TestDeliveryPanic_FirstAttempt_IsOneFailedAttempt: the send path panics in
// the goroutine that makes an event's first attempt. The process survives,
// the delivery counts one failed attempt, and it waits for the ladder's
// first retry. Each later claim panics too and is counted, so the delivery
// ends failed after the last attempt.
//
// Without the recover the panic ends the test process, as it would the
// server. After the restart the row still counts no attempt, so the retry
// worker claims it at the end of its lease and panics again.
func TestDeliveryPanic_FirstAttempt_IsOneFailedAttempt(t *testing.T) {
	f := newGuardFixture(t, "Delivery panic first attempt")
	// Room for every mark of the ladder: only the first is waited for.
	store := &hookedStore{Store: f.store, marked: make(chan error, maxRetries+1)}
	client := &panickingClient{}
	// NewService, not NewTestService: the first attempt runs in its own
	// goroutine, as in production.
	svc := NewService(store, client, "local")

	sent := time.Now()
	if err := svc.dispatchFromOutbox(f.ctx, "", f.tenantID, "invoice.paid", map[string]any{"invoice_id": "hc_inv_1"}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	select {
	case err := <-store.marked:
		if err != nil {
			t.Fatalf("mark of the panicked attempt: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the panicked first attempt was never marked")
	}

	d := f.onlyDelivery(t)
	wantRetryScheduled(t, d, 1, sent)
	if d.ErrorMessage == "" {
		t.Error("error_message is empty, want the reason the attempt failed")
	}

	// Every later claim panics again. Each one is counted, so the delivery
	// walks the ladder and ends failed.
	for attempt := 2; attempt <= maxRetries; attempt++ {
		f.endLease(t, d.ID)
		sent = time.Now()
		retryPass(t, svc, f.tick)
		wantRetryScheduled(t, f.delivery(t, d.ID), attempt, sent)
	}
	f.endLease(t, d.ID)
	retryPass(t, svc, f.tick)
	got := f.delivery(t, d.ID)
	if got.Status != domain.DeliveryFailed || got.AttemptCount != maxRetries+1 || got.CompletedAt == nil {
		t.Errorf("delivery status/attempts/completed_at = %s/%d/%v, want failed/%d/set after the last attempt",
			got.Status, got.AttemptCount, got.CompletedAt, maxRetries+1)
	}
	if n := client.posts.Load(); n != int32(maxRetries+1) {
		t.Errorf("POSTs = %d, want %d: one per counted attempt", n, maxRetries+1)
	}
}

// TestDeliveryPanic_RetryWorker_IsOneFailedAttempt: a claimed delivery
// panics in the retry worker's goroutine, in the send or before it. The
// worker survives, the delivery counts one failed attempt and waits for the
// ladder's retry, and a second claim counts a second one.
//
// Without the recover the panic ends the test process, as it would the
// server.
func TestDeliveryPanic_RetryWorker_IsOneFailedAttempt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		faults    func(*hookedStore)
		panics    bool // the client panics; otherwise it answers 200
		wantPosts int32
	}{
		{
			name:      "the send panics",
			faults:    func(*hookedStore) {},
			panics:    true,
			wantPosts: 2,
		},
		{
			name: "the endpoint read panics",
			faults: func(s *hookedStore) {
				s.getEndpoint = func() (domain.WebhookEndpoint, error) { panic("endpoint read bug") }
			},
			wantPosts: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t, "Delivery panic retry "+tc.name)
			store := &hookedStore{Store: f.store}
			tc.faults(store)
			panicking, answering := &panickingClient{}, &countingClient{status: http.StatusOK}
			var client HTTPClient = answering
			if tc.panics {
				client = panicking
			}
			svc := NewService(store, client, "local")
			d := f.claimableDelivery(t)

			sent := time.Now()
			retryPass(t, svc, f.tick)
			wantRetryScheduled(t, f.delivery(t, d.ID), 1, sent)

			f.endLease(t, d.ID)
			sent = time.Now()
			retryPass(t, svc, f.tick)
			wantRetryScheduled(t, f.delivery(t, d.ID), 2, sent)

			if posts := panicking.posts.Load() + answering.posts.Load(); posts != tc.wantPosts {
				t.Errorf("POSTs = %d, want %d", posts, tc.wantPosts)
			}
		})
	}
}

// TestDeliveryPanic_MarkPanicsToo_WorkerSurvives: the mark itself panics, so
// the failed attempt cannot be counted either. The worker still survives.
// The row keeps its count and its claim lease, and a later claim takes it.
func TestDeliveryPanic_MarkPanicsToo_WorkerSurvives(t *testing.T) {
	f := newGuardFixture(t, "Delivery panic in the mark")
	store := &hookedStore{Store: f.store}
	store.updateDelivery = func() (domain.WebhookDelivery, error) { panic("mark bug") }
	client := &countingClient{status: http.StatusInternalServerError}
	svc := NewService(store, client, "local")
	d := f.claimableDelivery(t)

	claimed := time.Now()
	retryPass(t, svc, f.tick)
	got := f.delivery(t, d.ID)
	if got.Status != domain.DeliveryPending || got.AttemptCount != 0 {
		t.Errorf("delivery status/attempts = %s/%d, want pending/0: no mark could land", got.Status, got.AttemptCount)
	}
	if got.NextRetryAt == nil || got.NextRetryAt.Before(claimed.Add(time.Minute)) {
		t.Errorf("next_retry_at = %v, want the claim lease, over a minute away", got.NextRetryAt)
	}
	if n := client.posts.Load(); n != 1 {
		t.Errorf("POSTs = %d, want 1", n)
	}
}

// tickEndingClient ends the retry tick while the POST is in flight, as a
// deploy or a lost leader lease does. Then it answers with status, or with
// err when the POST itself was cut short.
type tickEndingClient struct {
	endTick context.CancelFunc
	status  int
	err     error
}

func (c *tickEndingClient) Do(*http.Request) (*http.Response, error) {
	c.endTick()
	if c.err != nil {
		return nil, c.err
	}
	return &http.Response{StatusCode: c.status, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
}

// TestDeliveryMark_LandsAfterItsTickEnded: the tick's context ends between
// the receiver's answer and the mark. The mark still lands.
//
// With the mark on the tick's context, a delivered row stays pending and is
// sent again at the end of its lease, and a failed attempt is not counted.
func TestDeliveryMark_LandsAfterItsTickEnded(t *testing.T) {
	t.Run("a 2xx answer is marked delivered", func(t *testing.T) {
		f := newGuardFixture(t, "Delivery mark detached 2xx")
		tick, endTick := context.WithCancel(f.tick)
		defer endTick()
		svc := NewService(f.store, &tickEndingClient{endTick: endTick, status: http.StatusOK}, "local")
		d := f.claimableDelivery(t)

		retryPass(t, svc, tick)
		if tick.Err() == nil {
			t.Fatal("the tick context is still live: the test did not end it")
		}
		got := f.delivery(t, d.ID)
		if got.Status != domain.DeliverySucceeded || got.AttemptCount != 1 {
			t.Errorf("delivery status/attempts = %s/%d, want succeeded/1: the receiver has the event, and the row would be sent again",
				got.Status, got.AttemptCount)
		}
	})

	t.Run("a 5xx answer is counted and retried", func(t *testing.T) {
		f := newGuardFixture(t, "Delivery mark detached 5xx")
		tick, endTick := context.WithCancel(f.tick)
		defer endTick()
		svc := NewService(f.store, &tickEndingClient{endTick: endTick, status: http.StatusInternalServerError}, "local")
		d := f.claimableDelivery(t)

		sent := time.Now()
		retryPass(t, svc, tick)
		if tick.Err() == nil {
			t.Fatal("the tick context is still live: the test did not end it")
		}
		wantRetryScheduled(t, f.delivery(t, d.ID), 1, sent)
	})
}

// TestDelivery_POSTCutShortByItsTick_IsNotCounted: the tick ends while the
// POST is in flight, and the POST fails because of it. The receiver did not
// fail, so no attempt is counted: the row keeps its count and its claim
// lease, and a later pass sends it.
//
// A detached mark alone would count it. Every deploy would then use up one
// retry of each delivery in flight.
func TestDelivery_POSTCutShortByItsTick_IsNotCounted(t *testing.T) {
	f := newGuardFixture(t, "Delivery cut short")
	tick, endTick := context.WithCancel(f.tick)
	defer endTick()
	svc := NewService(f.store, &tickEndingClient{endTick: endTick, err: context.Canceled}, "local")
	d := f.claimableDelivery(t)

	claimed := time.Now()
	retryPass(t, svc, tick)
	got := f.delivery(t, d.ID)
	if got.Status != domain.DeliveryPending || got.AttemptCount != 0 {
		t.Errorf("delivery status/attempts = %s/%d, want pending/0: a POST its own tick cut short is not the receiver's failure",
			got.Status, got.AttemptCount)
	}
	if got.NextRetryAt == nil || got.NextRetryAt.Before(claimed.Add(2*time.Minute)) {
		t.Errorf("next_retry_at = %v, want the claim lease, over two minutes away, not the ladder's first retry", got.NextRetryAt)
	}
}

// TestDelivery_PendingPastTheBound_IsGivenUpWithoutAPOST: a delivery still
// pending longer than the whole retry ladder plus a day is marked failed
// before anything is sent, with an error that says no final outcome was
// recorded. Its attempt count is left as it was.
//
// Only a mark moves the attempt count. A delivery whose endpoint cannot be
// read never reaches a mark, so without the bound it is claimed again at the
// end of every lease, for good.
func TestDelivery_PendingPastTheBound_IsGivenUpWithoutAPOST(t *testing.T) {
	var ladder time.Duration
	for _, wait := range retryBackoffs {
		ladder += wait
	}
	bound := ladder + 24*time.Hour
	unreadable := func(s *hookedStore) {
		s.getEndpoint = func() (domain.WebhookEndpoint, error) {
			return domain.WebhookEndpoint{}, errors.New("decrypt the signing secret: key not found")
		}
	}

	for _, tc := range []struct {
		name       string
		age        time.Duration
		faults     func(*hookedStore)
		wantStatus domain.DeliveryStatus
		wantPosts  int32
	}{
		{"past the bound", bound + time.Minute, func(*hookedStore) {}, domain.DeliveryFailed, 0},
		{"past the bound, endpoint unreadable", bound + time.Minute, unreadable, domain.DeliveryFailed, 0},
		{"inside the bound", bound - time.Minute, func(*hookedStore) {}, domain.DeliverySucceeded, 1},
		{"inside the bound, endpoint unreadable", bound - time.Minute, unreadable, domain.DeliveryPending, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t, "Delivery bound "+tc.name)
			store := &hookedStore{Store: f.store}
			tc.faults(store)
			client := &countingClient{status: http.StatusOK}
			svc := NewService(store, client, "local")
			d := f.claimableDelivery(t)
			// Backdated by hand: no production path makes an old row in
			// test time.
			f.exec(t, `UPDATE webhook_deliveries SET created_at = $2 WHERE id = $1`, d.ID, time.Now().UTC().Add(-tc.age))

			retryPass(t, svc, f.tick)
			got := f.delivery(t, d.ID)
			if got.Status != tc.wantStatus {
				t.Errorf("delivery status = %s, want %s", got.Status, tc.wantStatus)
			}
			if n := client.posts.Load(); n != tc.wantPosts {
				t.Errorf("POSTs = %d, want %d", n, tc.wantPosts)
			}
			if tc.wantStatus != domain.DeliveryFailed {
				return
			}
			if got.AttemptCount != 0 {
				t.Errorf("attempt_count = %d, want 0: giving up is not an attempt", got.AttemptCount)
			}
			if got.CompletedAt == nil || got.NextRetryAt != nil {
				t.Errorf("completed_at/next_retry_at = %v/%v, want set/NULL", got.CompletedAt, got.NextRetryAt)
			}
			if !strings.Contains(got.ErrorMessage, "no final outcome recorded") {
				t.Errorf("error_message = %q, want it to say no final outcome was recorded", got.ErrorMessage)
			}

			// The row is resolved: a later pass has nothing to claim.
			f.endLease(t, d.ID)
			retryPass(t, svc, f.tick)
			if n := client.posts.Load(); n != 0 {
				t.Errorf("POSTs after a second pass = %d, want 0", n)
			}
		})
	}
}

// TestDelivery_GivenUpAfterFailedAttempts_KeepsTheLastError: a delivery that
// recorded failures, and was then delayed past the bound before its last
// retry, is given up with its attempt count and its last error kept.
func TestDelivery_GivenUpAfterFailedAttempts_KeepsTheLastError(t *testing.T) {
	f := newGuardFixture(t, "Delivery bound after failures")
	client := &countingClient{status: http.StatusServiceUnavailable}
	svc := NewService(f.store, client, "local")
	d := f.claimableDelivery(t)

	retryPass(t, svc, f.tick) // one counted failure: HTTP 503
	f.exec(t, `UPDATE webhook_deliveries SET created_at = now() - interval '60 hours' WHERE id = $1`, d.ID)
	f.endLease(t, d.ID)
	retryPass(t, svc, f.tick)

	got := f.delivery(t, d.ID)
	if got.Status != domain.DeliveryFailed || got.AttemptCount != 1 {
		t.Errorf("delivery status/attempts = %s/%d, want failed/1: the one attempt made stays counted", got.Status, got.AttemptCount)
	}
	if !strings.Contains(got.ErrorMessage, "no final outcome recorded") || !strings.Contains(got.ErrorMessage, "HTTP 503") {
		t.Errorf("error_message = %q, want the give-up text and the last error, HTTP 503", got.ErrorMessage)
	}
	if got.HTTPStatusCode != http.StatusServiceUnavailable {
		t.Errorf("http_status_code = %d, want 503 kept from the last attempt", got.HTTPStatusCode)
	}
	if n := client.posts.Load(); n != 1 {
		t.Errorf("POSTs = %d, want 1: giving up sends nothing", n)
	}
}
