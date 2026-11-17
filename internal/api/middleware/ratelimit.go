package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/go-redis/redis_rate/v10"
	goredis "github.com/redis/go-redis/v9"

	"github.com/gethardcount/hardcount/internal/api/respond"
	"github.com/gethardcount/hardcount/internal/audit"
	"github.com/gethardcount/hardcount/internal/auth"
	"github.com/gethardcount/hardcount/internal/platform/logging"
	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// RateLimiter is a distributed rate limiter that runs GCRA (Generic Cell Rate
// Algorithm) in Redis. GCRA is a leaky-bucket variant. It smooths traffic,
// instead of allowing a burst and then blocking at each window boundary.
//
// It uses go-redis/redis_rate, the official rate-limiting companion for
// go-redis, used across the go-redis ecosystem.
type RateLimiter struct {
	limiter *redis_rate.Limiter
	limit   redis_rate.Limit
	outage  OutageMode
	// name namespaces this limiter's Redis keys. Distinct limiters (general
	// API, hosted invoice, setup-link cooldown) share one Redis and key on
	// the same identifier, such as "ip:1.2.3.4", but each has its own GCRA
	// parameters. Without a namespace per limiter they would share one
	// bucket. A request to one surface would spend another's allowance, and
	// the conflicting rate and period would corrupt the smoothing state.
	name string
	// local counts requests in this process. It is set only for
	// CountLocally.
	local *localLimiter
}

// OutageMode is what a limiter does while Redis is not configured or a
// Redis call fails.
type OutageMode int

const (
	// FailOpen allows every request. It suits local development, and a
	// bucket whose refusals would cost more than an outage's extra traffic.
	FailOpen OutageMode = iota
	// FailClosed answers every request 429. Use it in production, where
	// serving without a limit is a DDoS vector.
	FailClosed
	// CountLocally counts in this process with the same rate and burst. The
	// count is per replica: with N replicas behind a balancer, one key gets
	// up to N times the limit while Redis is down.
	CountLocally
)

// OpenRedis parses url, opens a client and checks it with a Ping bounded by
// ctx. An empty url is not an error: it returns (nil, nil), and every limiter
// then follows its OutageMode. The caller decides what an error means.
// Production refuses to start on one (cmd/hardcount/main.go), because the
// general and hosted-invoice limiters fail closed without Redis. A replica
// that started anyway would look healthy and answer 429 to its share of
// traffic.
// Other environments warn and continue, failing open.
func OpenRedis(ctx context.Context, url string) (*goredis.Client, error) {
	if url == "" {
		return nil, nil
	}
	opt, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid REDIS_URL: %w", err)
	}
	c := goredis.NewClient(opt)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("redis unreachable at %s: %w", opt.Addr, err)
	}
	return c, nil
}

// NewRateLimiter creates a GCRA rate limiter backed by Redis that allows rate
// requests per window, with a burst of rate. name namespaces the limiter's
// Redis keys, such as "general", "hosted_invoice" or "setup_link", so
// limiters with different parameters do not share a bucket. For example,
// NewRateLimiter(rdb, "general", 100, time.Minute, FailClosed) allows 100
// requests a minute, spread smoothly with no bursts at the boundary. outage
// says what the limiter does while rdb is nil or a Redis call fails.
func NewRateLimiter(rdb *goredis.Client, name string, rate int, window time.Duration, outage OutageMode) *RateLimiter {
	rl := &RateLimiter{
		limit:  redis_rate.Limit{Rate: rate, Burst: rate, Period: window},
		outage: outage,
		name:   name,
	}
	if rdb != nil {
		rl.limiter = redis_rate.NewLimiter(rdb)
	}
	if outage == CountLocally {
		rl.local = newLocalLimiter(rl.limit)
	}
	return rl
}

// Middleware answers 429 to a request over the limiter's rate, or to every
// request while a FailClosed limiter cannot reach Redis. The bucket is the API
// key, the tenant and mode for a dashboard session, or the client IP before
// sign-in (rateLimitKey).
func (rl *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return RateLimited{limiter: rl, next: next}
	}
}

// RateLimited counts each request against its limiter. RateLimiter.Middleware
// makes it; its doc has the rule.
type RateLimited struct {
	limiter *RateLimiter
	next    http.Handler
}

func (l RateLimited) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, err := rateLimitKey(r)
	if err != nil {
		respond.FromError(w, r, err, "rate_limit")
		return
	}
	remaining, resetAt, retryIn, allowed := l.limiter.allow(r.Context(), key)

	// Set rate limit headers (Stripe convention)
	w.Header().Set("X-RateLimit-Limit", strconv.Itoa(l.limiter.limit.Rate))
	w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(remaining))
	w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))

	if !allowed {
		// Retry-After is when the next request may pass, rounded
		// up to a whole second. It is not the reset time, when
		// the whole bucket is full again. That is a full period
		// (61s at any per-minute limit), although one request's
		// token refills in period/rate. A client that honors the
		// header would wait a minute for nothing.
		retryAfter := int(math.Ceil(retryIn.Seconds()))
		if retryAfter < 1 {
			retryAfter = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		respond.RateLimited(w, r)
		return
	}

	l.next.ServeHTTP(w, r)
}

// AllowKey is the exported form of allow, for a handler that enforces a
// bucket with its own key instead of using the middleware.
// paymentmethods.send-setup-email uses it for a per-customer cooldown, so a
// double click does not send two emails. It returns (remaining, resetAt,
// allowed), with the same outage mode as the middleware.
func (rl *RateLimiter) AllowKey(ctx context.Context, key string) (int, time.Time, bool) {
	remaining, resetAt, _, allowed := rl.allow(ctx, key)
	return remaining, resetAt, allowed
}

// allow checks the rate limit for key and returns (remaining, resetAt,
// retryIn, allowed). resetAt is when the bucket is full again. retryIn is how
// long until the next request may pass, and 0 when this one is allowed. When
// Redis is not configured or fails, the outage mode decides.
func (rl *RateLimiter) allow(ctx context.Context, key string) (int, time.Time, time.Duration, bool) {
	now := time.Now().UTC()

	// err stays nil when Redis is not configured, which is normal outside
	// production, so FailOpen and CountLocally log only a failed call.
	var err error
	if rl.limiter != nil {
		var res *redis_rate.Result
		res, err = rl.limiter.Allow(ctx, rl.bucketKey(key), rl.limit)
		if err == nil {
			resetAt := now.Add(res.ResetAfter)
			if res.Allowed > 0 {
				return res.Remaining, resetAt, 0, true
			}
			// redis_rate's RetryAfter is the time until the next request passes.
			return res.Remaining, resetAt, res.RetryAfter, false
		}
	}

	switch rl.outage {
	case CountLocally:
		if err != nil {
			slog.Warn("rate_limiter: redis error, counting in this process", "error", err, "key", key)
		}
		return rl.local.allow(key, now)
	case FailClosed:
		slog.Log(ctx, logging.ErrorLevel(ctx, err), "rate_limiter: redis error, failing closed", "error", err, "key", key)
		return 0, now.Add(rl.limit.Period), rl.limit.Period, false
	default:
		if err != nil {
			slog.Warn("rate_limiter: redis error, failing open", "error", err, "key", key)
		}
		return rl.limit.Rate - 1, now.Add(rl.limit.Period), 0, true
	}
}

// bucketKey is the Redis key for a logical bucket: the per-limiter namespace
// (name) keeps limiters with different GCRA parameters from colliding.
func (rl *RateLimiter) bucketKey(key string) string {
	return "rl:" + rl.name + ":" + key
}

// rateLimitKey chooses the most specific bucket for the request, as Stripe
// does:
//
//  1. The API key id, when there is one. A bucket per key stops a runaway
//     integration from starving the tenant's dashboard or a parallel
//     worker. Each key has a fixed livemode (hc_secret_test_* or
//     hc_secret_live_*), so a bucket per key is also a bucket per mode.
//  2. The tenant id, for dashboard traffic signed in with a session and no
//     key. It carries the active mode as a suffix, so test-mode exploration
//     and live-mode operations do not share a quota, as at Stripe. Without
//     the suffix, one operator's heavy test load could throttle another
//     operator's live action.
//  3. The trusted client IP (audit.ExtractClientIP), for routes with no
//     sign-in at all: bootstrap, /v1/auth and the public token pages. That
//     way a flood with no sign-in cannot bypass the limiter.
func rateLimitKey(r *http.Request) (string, error) {
	if keyID := auth.CallerOf(r.Context()).KeyID; keyID != "" {
		return "key:" + keyID, nil
	}
	if tenantID := auth.TenantID(r.Context()); tenantID != "" {
		live, err := postgres.Livemode(r.Context())
		if err != nil {
			return "", err
		}
		mode := "test"
		if live {
			mode = "live"
		}
		return "tenant:" + tenantID + ":" + mode, nil
	}
	return "ip:" + audit.ExtractClientIP(r), nil
}

// SplitRateLimit routes each request to one of two limiters: special when
// match(r) is true, and base otherwise. It splits ingest from CRUD. The
// ingest routes (usage events and LiteLLM callbacks, one POST per LLM call)
// need a bucket orders of magnitude larger than the operator CRUD default.
// Through the general bucket, revenue data would be dropped without a word:
// LiteLLM retries only on 5xx, so every 429 is an event lost for good.
func SplitRateLimit(match func(*http.Request) bool, special, base *RateLimiter) func(http.Handler) http.Handler {
	specialMW := special.Middleware()
	baseMW := base.Middleware()
	return func(next http.Handler) http.Handler {
		sp := specialMW(next)
		bs := baseMW(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if match(r) {
				sp.ServeHTTP(w, r)
				return
			}
			bs.ServeHTTP(w, r)
		})
	}
}
