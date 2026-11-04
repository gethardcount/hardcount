package leader

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Executor is the database surface the manager needs: a pool that runs
// single autocommit statements. *sql.DB satisfies it.
type Executor interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// LostFunc is told, once, why a led tick ended without completing. The
// reason is "takeover", "paused", "released" or "heartbeat_timeout" when the
// lease was lost. It is "panicked" when the work panicked and the lease was
// released as not completed. The API server wires it to a Prometheus
// counter. A nil LostFunc is fine.
type LostFunc func(role Role, reason string)

// Manager leads roles for one process.
type Manager struct {
	db       Executor
	holderID string
	onLost   LostFunc

	driftOnce sync.Once
}

// New returns a manager whose statements run on db.
func New(db Executor, onLost LostFunc) *Manager {
	return &Manager{db: db, holderID: holderID(), onLost: onLost}
}

// HolderID is this process's lease identity (observability).
func (m *Manager) HolderID() string { return m.holderID }

// The four statements. Each runs in autocommit, with no SET, no session
// state and no BEGIN, so a pooler may route each one to a different backend.
// The server stamps and compares every timestamp.
const (
	// sqlAcquire runs once per poll. One row back means this process leads
	// the tick, and no row means the role is not due, held, or paused.
	// Racing replicas serialize on the row lock, and ON CONFLICT DO UPDATE
	// re-evaluates its WHERE against the winner's row. The INSERT path
	// recreates a missing row with an epoch-ms token, above any issued. A
	// misspelled role violates the CHECK and fails with a loud error.
	sqlAcquire = `
INSERT INTO leader_leases (role, holder_token, holder_id, acquired_at, heartbeat_at, expires_at)
VALUES ($1, (extract(epoch FROM clock_timestamp()) * 1000)::bigint, $2, now(), now(), now() + make_interval(secs => $3))
ON CONFLICT (role) DO UPDATE
   SET holder_token = leader_leases.holder_token + 1,
       holder_id    = EXCLUDED.holder_id,
       acquired_at  = now(),
       heartbeat_at = now(),
       expires_at   = now() + make_interval(secs => $3)
 WHERE leader_leases.paused_at IS NULL
   AND (leader_leases.expires_at IS NULL OR leader_leases.expires_at <= now())
   AND (leader_leases.last_tick_ended_at IS NULL
        OR leader_leases.last_tick_ended_at <= now() - make_interval(secs => $4))
   AND (leader_leases.not_before IS NULL OR leader_leases.not_before <= now())
RETURNING holder_token, now()`

	// sqlRenew is the heartbeat. No row back is a definite loss: a takeover,
	// a release, or a pause, which sets the holder to NULL. With no
	// expires_at predicate, a holder may renew its own expired lease that no
	// one has taken. The row lock decides a renew racing a takeover on an
	// expired row. A statement error is a missed beat, retried next beat.
	sqlRenew = `
UPDATE leader_leases
   SET heartbeat_at = now(),
       expires_at   = now() + make_interval(secs => $3)
 WHERE role = $1 AND holder_token = $2 AND holder_id IS NOT NULL
RETURNING now()`

	// sqlWhyLost runs once after a renew finds no row, for the log line only.
	sqlWhyLost = `SELECT holder_id, holder_token, paused_by IS NOT NULL FROM leader_leases WHERE role = $1`

	// sqlRelease runs after work returns. $3 is the outcome:
	//   - 0, completed: stamp last_tick_* with this holder and now(), and
	//     clear any cooldown. The role is due again after one interval.
	//   - 1, interrupted by the parent ctx (a clean SIGTERM, even if the work
	//     then panicked): leave the cadence alone, so the role is due on
	//     another replica at its next poll.
	//   - 2, lease lost or heartbeat timed out: wait min(interval, 60 s), so
	//     a slow database cannot turn an hourly tick into a partial tick
	//     every few seconds.
	//   - 3, the work panicked: wait one full interval. Its other steps and
	//     rows already ran (each step recovers, and the first panic is
	//     raised again at the end), so a sooner rerun would only re-drive
	//     the rows that panic.
	//
	// Outcomes 2 and 3 are not completions. They leave last_tick_* alone, since
	// leader_status and the stall gauge report completed ticks only, and
	// hold the role back with not_before. No row back is fine: already lost.
	sqlRelease = `
UPDATE leader_leases
   SET holder_id = NULL, acquired_at = NULL, heartbeat_at = NULL, expires_at = NULL,
       last_tick_holder   = CASE $3::int WHEN 0 THEN holder_id ELSE last_tick_holder END,
       last_tick_ended_at = CASE $3::int WHEN 0 THEN now()     ELSE last_tick_ended_at END,
       not_before         = CASE $3::int
                              WHEN 0 THEN NULL
                              WHEN 1 THEN not_before
                              WHEN 3 THEN now() + make_interval(secs => $4)
                              ELSE now() + LEAST(make_interval(secs => $4), make_interval(secs => $5))
                            END
 WHERE role = $1 AND holder_token = $2 AND holder_id IS NOT NULL`
)

// Lead implements Gate.
func (m *Manager) Lead(ctx context.Context, role Role, interval time.Duration, work func(context.Context)) (bool, error) {
	token, dbNow, ok, err := m.acquire(ctx, role, interval)
	if err != nil || !ok {
		return false, err
	}

	workCtx, cancel := context.WithCancelCause(WithToken(ctx, role, token))
	hb := m.startHeartbeat(role, token, dbNow, cancel)

	// The release always runs, on a panic too. The runner's recover wraps
	// Lead, so a panic in work unwinds through this defer first. returned
	// stays false, and the tick is released as failed, never as completed. A
	// completed release would stamp last_tick_ended_at, and a fresh stall
	// gauge would hide a tick that panics every interval. The panic then
	// continues to the runner, which logs it with the stack.
	//
	// A cancelled tick is released by its cause, even if its work then
	// panicked (on the cancelled ctx, or a guarded panic raised again at the
	// end). A lost lease is released as lost, with the lost-lease cooldown,
	// and is one event, as onLost below counts it. A shutdown is released as
	// interrupted, so the role is due at once on the next replica, which may
	// carry the fix for that panic.
	returned := false
	defer func() {
		hb.stop()
		cause := context.Cause(workCtx)
		outcome := releaseCompleted
		switch {
		case errors.Is(cause, ErrLeaseLost):
			outcome = releaseLost
		case cause != nil:
			outcome = releaseInterrupted // parent cancelled: clean shutdown
		case !returned:
			outcome = releaseFailed
		}
		cancel(nil)
		m.release(role, token, interval, outcome)
		// One report per tick. The heartbeat (m.lost) has already reported a
		// tick whose lease was lost. A panic while that tick unwinds on its
		// cancelled ctx follows from the loss and is not a second event. A
		// panic in a tick that a shutdown interrupted is still reported.
		if !returned && !errors.Is(cause, ErrLeaseLost) && m.onLost != nil {
			m.onLost(role, "panicked")
		}
	}()

	work(workCtx)
	returned = true
	return true, nil
}

func (m *Manager) acquire(ctx context.Context, role Role, interval time.Duration) (token int64, dbNow time.Time, ok bool, err error) {
	sctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()
	err = m.db.QueryRowContext(sctx, sqlAcquire, string(role), m.holderID, LeaseTTL.Seconds(), interval.Seconds()).Scan(&token, &dbNow)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	if err != nil {
		return 0, time.Time{}, false, err
	}
	return token, dbNow, true, nil
}

// The release outcomes are the $3 of sqlRelease. releaseFailed (the work
// panicked) and releaseLost are not completions. A lost lease waits
// min(interval, 60 s), and a panicked tick waits one full interval.
const (
	releaseCompleted   = 0
	releaseInterrupted = 1
	releaseLost        = 2
	releaseFailed      = 3
)

func (m *Manager) release(role Role, token int64, interval time.Duration, outcome int) {
	rctx, cancel := context.WithTimeout(context.Background(), StatementTimeout)
	defer cancel()
	if _, err := m.db.ExecContext(rctx, sqlRelease, string(role), token, outcome, interval.Seconds(), interruptedCooldown.Seconds()); err != nil {
		// The row stays held for at most LeaseTTL. The next acquire accepts
		// an expired row, and the role is due, because last_tick_ended_at was
		// not stamped.
		slog.Error("leader: release failed — lease expires on its own", "role", role, "token", token, "error", err)
	}
}

type heartbeat struct {
	done chan struct{}
	wg   sync.WaitGroup
}

func (h *heartbeat) stop() {
	close(h.done)
	h.wg.Wait()
}

// startHeartbeat renews the lease every HeartbeatEvery on its own root
// context, because the parent's cancellation must not stop the renewer
// before the release.
//
// lastAck is the send instant of the last acknowledged renew, on the
// monotonic clock. The abandon check runs before the next renew is sent.
// The database stamped expires_at >= T_send + LeaseTTL on its own clock, so
// cancelling at T_send + AbandonAfter (< LeaseTTL - StatementTimeout) means
// no statement of ours starts after a successor could exist. No app
// timestamp is compared with a database timestamp. RETURNING now() is used
// only to observe drift, as a duration against a duration.
func (m *Manager) startHeartbeat(role Role, token int64, dbNowAtAcquire time.Time, cancel context.CancelCauseFunc) *heartbeat {
	h := &heartbeat{done: make(chan struct{})}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		lastAck := time.Now()       // monotonic
		lastDBNow := dbNowAtAcquire // DB clock at the last ack
		ticker := time.NewTicker(HeartbeatEvery)
		defer ticker.Stop()
		for {
			select {
			case <-h.done:
				return
			case <-ticker.C:
			}
			if time.Since(lastAck) >= AbandonAfter {
				m.lost(role, token, "heartbeat_timeout", cancel)
				return
			}
			sendAt := time.Now()
			sctx, scancel := context.WithTimeout(context.Background(), StatementTimeout)
			var dbNow time.Time
			err := m.db.QueryRowContext(sctx, sqlRenew, string(role), token, LeaseTTL.Seconds()).Scan(&dbNow)
			scancel()
			switch {
			case err == nil:
				m.observeDrift(role, dbNow.Sub(lastDBNow), sendAt.Sub(lastAck))
				lastAck, lastDBNow = sendAt, dbNow
			case errors.Is(err, sql.ErrNoRows):
				m.lost(role, token, m.whyLost(role, token), cancel)
				return
			default:
				// A missed beat, not a loss. It is retried at the next beat,
				// and the abandon check above bounds how long we tolerate it.
				slog.Warn("leader: renew failed — will retry", "role", role, "token", token, "error", err)
			}
		}
	}()
	return h
}

func (m *Manager) whyLost(role Role, token int64) string {
	sctx, cancel := context.WithTimeout(context.Background(), StatementTimeout)
	defer cancel()
	var holder sql.NullString
	var current int64
	var paused bool
	if err := m.db.QueryRowContext(sctx, sqlWhyLost, string(role)).Scan(&holder, &current, &paused); err != nil {
		return "takeover"
	}
	switch {
	case paused:
		return "paused"
	case current != token:
		return "takeover"
	default:
		return "released"
	}
}

func (m *Manager) lost(role Role, token int64, reason string, cancel context.CancelCauseFunc) {
	slog.Error("leader: lease lost mid-tick — cancelling the tick's work", "role", role, "token", token, "reason", reason, "holder", m.holderID)
	cancel(ErrLeaseLost)
	if m.onLost != nil {
		m.onLost(role, reason)
	}
}

// observeDrift compares how far the database clock moved with how far the
// monotonic clock moved over the same span between acks: a duration against
// a duration. It warns once if they differ by more than StatementTimeout.
// The app host's clock does not affect correctness, but a skewed database
// clock across a failover is worth a line in the log.
func (m *Manager) observeDrift(role Role, dbDelta, monoDelta time.Duration) {
	diff := dbDelta - monoDelta
	if diff < 0 {
		diff = -diff
	}
	if diff > StatementTimeout {
		m.driftOnce.Do(func() {
			slog.Warn("leader: database clock advanced differently from the process clock between heartbeats", "role", role, "db_delta", dbDelta, "process_delta", monoDelta)
		})
	}
}
