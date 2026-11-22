package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gethardcount/hardcount/internal/domain"
	"github.com/gethardcount/hardcount/internal/platform/clock"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// previewServer is the server's clock: it moves a second each time it answers
// a read itself (no instant bound), and counts those reads.
type previewServer struct {
	at    time.Time
	reads int
}

func (c *previewServer) Now(ctx context.Context) time.Time {
	if at, ok := clock.EffectiveNow(ctx); ok {
		return at
	}
	c.reads++
	c.at = c.at.Add(time.Second)
	return c.at
}

// previewSubs answers one subscription, or fails with err, and counts the
// reads: the engine's resolver reads the subscription to find its clock, so
// each read is one decision. Only Get is used.
type previewSubs struct {
	SubscriptionReader
	sub   domain.Subscription
	err   error
	reads int
}

func (p *previewSubs) Get(context.Context, string, string) (domain.Subscription, error) {
	p.reads++
	return p.sub, p.err
}

// previewClocks answers every test clock at frozen.
type previewClocks struct{ frozen time.Time }

func (p previewClocks) Get(_ context.Context, _, id string) (domain.TestClock, error) {
	return domain.TestClock{ID: id, FrozenTime: p.frozen}, nil
}

// A preview decides its time from the subscription it previews and binds it,
// so its generated_at and any later read of now read one instant: on a clock,
// the clock's frozen time, with no read of the server's clock; on no clock,
// and in live mode, one read of the server's clock, by the bind (the engine's
// answer for a record on no clock reads nothing). A subscription that cannot
// be read refuses the preview, binds nothing and reads no server clock.
func TestPreviewDecide_BindsOneInstant(t *testing.T) {
	frozen := time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)
	past := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	testMode := postgres.WithLivemode(context.Background(), false)
	for _, c := range []struct {
		name            string
		ctx             context.Context
		sub             domain.Subscription
		getErr          error
		wantAt          time.Time
		wantClock       string
		wantServerReads int
		wantDecisions   int
		wantErr         bool
	}{
		{"a subscription on a clock", testMode, domain.Subscription{ID: "sub_1", TenantID: "t1", TestClockID: "clk_1"}, nil, frozen, "clk_1", 0, 1, false},
		{"a subscription on no clock", testMode, domain.Subscription{ID: "sub_1", TenantID: "t1"}, nil, past.Add(time.Second), "", 1, 1, false},
		{"live mode", postgres.WithLivemode(context.Background(), true), domain.Subscription{ID: "sub_1", TenantID: "t1"}, nil, past.Add(time.Second), "", 1, 0, false},
		{"a subscription that cannot be read", testMode, domain.Subscription{ID: "sub_1", TenantID: "t1"}, errors.New("connection reset by peer"), time.Time{}, "", 0, 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			server := &previewServer{at: past}
			subs := &previewSubs{sub: c.sub, err: c.getErr}
			engine := NewEngine(subs, nil, nil, nil, nil, nil, nil, nil, server)
			engine.SetTestClockReader(previewClocks{frozen: frozen})
			s := NewPreviewService(engine, nil, nil)
			ctx, err := s.decide(c.ctx, "t1", "sub_1")
			if c.wantErr != (err != nil) {
				t.Fatalf("err %v; want error %v", err, c.wantErr)
			}
			at, _ := clock.EffectiveNow(ctx)
			sim, _ := clock.SimOf(ctx)
			if !at.Equal(c.wantAt) || sim.TestClockID != c.wantClock || server.reads != c.wantServerReads || subs.reads != c.wantDecisions {
				t.Errorf("bound %v on clock %q after %d server reads and %d decisions; want %v on %q after %d and %d",
					at, sim.TestClockID, server.reads, subs.reads, c.wantAt, c.wantClock, c.wantServerReads, c.wantDecisions)
			}
		})
	}
}
