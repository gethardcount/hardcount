package middleware

import (
	"math"
	"sync"
	"time"

	"github.com/go-redis/redis_rate/v10"
	"golang.org/x/time/rate"
)

// localLimiter counts requests in this process, one token bucket per key from
// golang.org/x/time/rate. It backs a RateLimiter whose Redis is not configured
// or failing (SetLocalFallback). A token bucket with the Redis limit's rate and
// burst allows the same requests as redis_rate's GCRA, so the only difference
// is scope: this counts what one process sees, not what the whole fleet sees.
type localLimiter struct {
	// every is the time one request's token takes to refill: period/rate.
	every time.Duration
	burst int
	// sweepEvery is how often allow drops buckets that have refilled. It is
	// the limit's period, the longest an emptied bucket takes to refill.
	sweepEvery time.Duration

	mu        sync.Mutex
	buckets   map[string]*rate.Limiter
	lastSweep time.Time
}

func newLocalLimiter(l redis_rate.Limit) *localLimiter {
	return &localLimiter{
		every:      l.Period / time.Duration(l.Rate),
		burst:      l.Burst,
		sweepEvery: l.Period,
		buckets:    make(map[string]*rate.Limiter),
	}
}

// allow spends one token from key's bucket at now. It returns what the Redis
// path returns: the requests left, when the bucket is full again, and whether
// this request may proceed.
func (l *localLimiter) allow(key string, now time.Time) (int, time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= l.sweepEvery {
		l.sweep(now)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = rate.NewLimiter(rate.Every(l.every), l.burst)
		l.buckets[key] = b
	}
	allowed := b.AllowN(now, 1)
	tokens := b.TokensAt(now)
	untilFull := time.Duration(math.Round((float64(l.burst) - tokens) * float64(l.every)))
	return int(math.Floor(tokens)), now.Add(untilFull), allowed
}

// sweep drops every bucket that has refilled. A full bucket allows exactly
// what a new one would, so dropping it changes no answer, and memory stays
// bounded by the keys seen in about the last two periods.
func (l *localLimiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if b.TokensAt(now) >= float64(l.burst) {
			delete(l.buckets, key)
		}
	}
	l.lastSweep = now
}
