package usage

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// AggregationMemo remembers AggregateByPricingRules results for the length of
// one request, so two readers that need the same (customer, meter, window)
// run the scan once.
//
// It exists for the public cost dashboard: the page shows this period's usage
// (CustomerUsageService) and an estimated remaining credit
// (billing.Engine.LiveCreditBalance), and both aggregate the same meter over
// the same period. That scan is the expensive part of the request (it reads
// every usage event of the period), and without the memo the dashboard would
// pay for it twice. The two readers live in different packages and call the
// store through different interfaces, so the shared result rides on ctx
// rather than being passed between them.
//
// The readers' windows differ in two cases, and each then runs its own scan:
// after a threshold invoice (the live figure reads only the window since the
// invoice) and for a second subscription on a different cycle.
//
// Never put one on a ctx that outlives a request: a memoized result does not
// see usage that arrives later.
type AggregationMemo struct {
	mu      sync.Mutex
	results map[string][]domain.RuleAggregation
	queries int
	hits    int
}

type aggregationMemoKey struct{}

// WithAggregationMemo returns ctx carrying a memo, and the memo. A memo
// already on ctx is kept and returned, so a caller further out (a test
// counting scans) observes the same one.
func WithAggregationMemo(ctx context.Context) (context.Context, *AggregationMemo) {
	if m, ok := ctx.Value(aggregationMemoKey{}).(*AggregationMemo); ok {
		return ctx, m
	}
	m := &AggregationMemo{results: map[string][]domain.RuleAggregation{}}
	return context.WithValue(ctx, aggregationMemoKey{}, m), m
}

// Queries is how many aggregations reached the database.
func (m *AggregationMemo) Queries() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.queries
}

// Hits is how many aggregations were answered from the memo.
func (m *AggregationMemo) Hits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hits
}

// through answers from the memo, or runs query and remembers its result.
// Errors are not remembered. Results are copied in and out so a caller that
// rewrites its slice cannot change what the next reader gets.
func (m *AggregationMemo) through(key string, query func() ([]domain.RuleAggregation, error)) ([]domain.RuleAggregation, error) {
	m.mu.Lock()
	if got, ok := m.results[key]; ok {
		m.hits++
		m.mu.Unlock()
		return slices.Clone(got), nil
	}
	m.mu.Unlock()

	got, err := query()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.queries++
	m.results[key] = slices.Clone(got)
	m.mu.Unlock()
	return got, nil
}

func aggregationMemoKeyFor(tenantID, customerID, meterID string, defaultMode domain.AggregationMode, from, to time.Time) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d|%d", tenantID, customerID, meterID, defaultMode, from.UnixNano(), to.UnixNano())
}
