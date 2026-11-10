package tenantstripe

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	hardcountauth "github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// seenCtx is what the retrier saw on the ctx it was given.
type seenCtx struct {
	userID, tenantID string
	livemode         bool
	modeErr          error
	errAfterRequest  error
	deadline         time.Time
	hasDeadline      bool
}

// observingRetrier records the ctx RetryProviderConfigErrors runs on, after
// the request it came from has ended (requestDone is closed first).
type observingRetrier struct {
	requestDone chan struct{}
	seen        chan seenCtx
}

func (r *observingRetrier) RetryProviderConfigErrors(ctx context.Context, _ string, _ bool) (int, []error) {
	<-r.requestDone
	live, err := postgres.Livemode(ctx)
	dl, ok := ctx.Deadline()
	r.seen <- seenCtx{
		userID: hardcountauth.UserID(ctx), tenantID: hardcountauth.TenantID(ctx),
		livemode: live, modeErr: err, errAfterRequest: ctx.Err(), deadline: dl, hasDeadline: ok,
	}
	return 0, nil
}

func (r *observingRetrier) CountProviderConfigErrors(context.Context, string, bool) (int, error) {
	return 1, nil
}

// TestPostConnectRetry_KeepsTheRequestsActorAndOutlivesIt: the post-connect
// retry runs on the request's values without its cancel. The operator who
// connected is on its ctx, so the invoices it finalizes are recorded under
// them (the audit actor comes from ctx), not "system"; so are the tenant
// and the mode. The request ending does not stop it, and it has a
// deadline.
//
// Mutation: build the ctx from context.Background(), and the actor, the
// tenant and the mode are gone.
func TestPostConnectRetry_KeepsTheRequestsActorAndOutlivesIt(t *testing.T) {
	const tenantID, userID = "hc_ten_pcr", "hc_usr_pcr"
	retrier := &observingRetrier{requestDone: make(chan struct{}), seen: make(chan seenCtx, 1)}
	svc := NewService(nil, nil)
	svc.SetStuckRetrier(retrier)

	req, endRequest := context.WithCancel(hardcountauth.WithUserID(
		hardcountauth.WithTenantID(postgres.WithLivemode(context.Background(), true), tenantID), userID))
	before := time.Now()
	svc.startPostConnectRetry(req, tenantID, true, 1)
	endRequest()
	close(retrier.requestDone)

	var got seenCtx
	select {
	case got = <-retrier.seen:
	case <-time.After(10 * time.Second):
		t.Fatal("the post-connect retry did not run")
	}
	if got.userID != userID {
		t.Errorf("actor on the retry's ctx = %q, want %q: its finalize audit rows would read system", got.userID, userID)
	}
	if got.tenantID != tenantID || got.modeErr != nil || !got.livemode {
		t.Errorf("tenant = %q, mode = %v (err %v); want %q, live", got.tenantID, got.livemode, got.modeErr, tenantID)
	}
	if got.errAfterRequest != nil {
		t.Errorf("retry ctx error after the request ended = %v, want none", got.errAfterRequest)
	}
	if !got.hasDeadline || got.deadline.After(before.Add(PostConnectRetryTimeout+time.Minute)) {
		t.Errorf("retry deadline = %v (set %v), want within PostConnectRetryTimeout", got.deadline, got.hasDeadline)
	}
}

// panickingRetrier panics in the retry.
type panickingRetrier struct{}

func (panickingRetrier) RetryProviderConfigErrors(context.Context, string, bool) (int, []error) {
	panic("retrier bug")
}

func (panickingRetrier) CountProviderConfigErrors(context.Context, string, bool) (int, error) {
	return 1, nil
}

// TestPostConnectRetry_PanicIsRecovered: a panic in the retry is logged at
// ERROR and ends only the retry. In a goroutine, an unrecovered panic stops
// the whole server.
//
// Mutation: remove the recover, and this test panics.
func TestPostConnectRetry_PanicIsRecovered(t *testing.T) {
	var logs bytes.Buffer
	svc := NewService(nil, nil)
	svc.logger = slog.New(slog.NewTextHandler(&logs, nil))
	svc.SetStuckRetrier(panickingRetrier{})

	ctx, cancel := context.WithCancel(postgres.WithLivemode(context.Background(), false))
	svc.runPostConnectRetry(ctx, cancel, "hc_ten_pcr", false, 1)

	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "post-connect retry panicked") || !strings.Contains(out, "retrier bug") {
		t.Errorf("log = %q, want an ERROR naming the panic", out)
	}
	if ctx.Err() == nil {
		t.Error("the retry's ctx is still live after a panic: its cancel did not run")
	}
}
