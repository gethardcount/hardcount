package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/domain"
)

func TestAggregationMemo_SameAggregationRunsOnce(t *testing.T) {
	_, memo := WithAggregationMemo(context.Background())
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	key := aggregationMemoKeyFor("ten", "cus", "mtr", domain.AggSum, from, to)

	runs := 0
	query := func() ([]domain.RuleAggregation, error) {
		runs++
		return []domain.RuleAggregation{{RuleID: "r1", Quantity: decimal.NewFromInt(7)}}, nil
	}
	first, err := memo.through(key, query)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	// A reader that rewrites its slice must not change the next reader's.
	first[0].Quantity = decimal.NewFromInt(999)

	second, err := memo.through(key, query)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if runs != 1 || memo.Queries() != 1 || memo.Hits() != 1 {
		t.Errorf("query ran %d times (memo: %d queries, %d hits), want 1, 1, 1", runs, memo.Queries(), memo.Hits())
	}
	if !second[0].Quantity.Equal(decimal.NewFromInt(7)) {
		t.Errorf("second reader got quantity %s, want 7 (the first reader's edit leaked)", second[0].Quantity)
	}

	// A different window is a different aggregation.
	other := aggregationMemoKeyFor("ten", "cus", "mtr", domain.AggSum, from.Add(time.Hour), to)
	if _, err := memo.through(other, query); err != nil {
		t.Fatalf("other window: %v", err)
	}
	if runs != 2 {
		t.Errorf("a different window ran the query %d times in total, want 2", runs)
	}
}

func TestAggregationMemo_ErrorsAreNotRemembered(t *testing.T) {
	_, memo := WithAggregationMemo(context.Background())
	boom := errors.New("connection reset")
	fail := true
	query := func() ([]domain.RuleAggregation, error) {
		if fail {
			return nil, boom
		}
		return []domain.RuleAggregation{}, nil
	}
	if _, err := memo.through("k", query); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the query error", err)
	}
	fail = false
	if _, err := memo.through("k", query); err != nil {
		t.Errorf("retry after a failed query returned %v, want the fresh result", err)
	}
}

func TestWithAggregationMemo_KeepsTheOneAlreadyOnCtx(t *testing.T) {
	ctx, outer := WithAggregationMemo(context.Background())
	_, inner := WithAggregationMemo(ctx)
	if inner != outer {
		t.Error("a second WithAggregationMemo replaced the memo; the caller further out would count nothing")
	}
}
