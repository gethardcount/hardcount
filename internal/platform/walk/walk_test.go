package walk

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

type row struct {
	at time.Time
	id string
}

func keyOf(r row) Key { return Key{At: r.at, ID: r.id} }

var t0 = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

// rows returns n rows in key order: one second apart, and two rows on each
// instant so that the id breaks the tie.
func rows(n int) []row {
	out := make([]row, n)
	for i := range out {
		out[i] = row{at: t0.Add(time.Duration(i/2) * time.Second), id: fmt.Sprintf("r%02d", i)}
	}
	return out
}

// sorted is a list over rows already in key order. It records the key each
// call started after.
type sorted struct {
	rows   []row
	afters []Key
}

func (s *sorted) list(_ context.Context, after Key, limit int) ([]row, error) {
	s.afters = append(s.afters, after)
	var page []row
	for _, r := range s.rows {
		k := keyOf(r)
		if k.At.Before(after.At) || (k.At.Equal(after.At) && k.ID <= after.ID) {
			continue
		}
		page = append(page, r)
		if len(page) == limit {
			break
		}
	}
	return page, nil
}

func collect(t *testing.T, seq func(func(row, error) bool)) ([]string, []error) {
	t.Helper()
	var ids []string
	var errs []error
	looped := false
	for r, err := range seq {
		if len(ids)+len(errs) > 1000 {
			looped = true
			break
		}
		if err != nil {
			if r != (row{}) {
				t.Errorf("an error came with row %q, want a zero row", r.id)
			}
			errs = append(errs, err)
			continue
		}
		if len(errs) > 0 {
			t.Errorf("row %q came after an error, want the error last", r.id)
		}
		ids = append(ids, r.id)
	}
	if looped {
		t.Fatal("the walk did not end: it read past 1000 rows of a short list")
	}
	return ids, errs
}

func TestWalk_EveryRowOnceToTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name         string
		n, limit     int
		wantFetches  int
		wantLastFrom string
	}{
		{"a short last page", 5, 2, 3, "r03"},
		{"pages that end on the last row", 4, 2, 3, "r03"},
		{"one short page", 1, 2, 1, ""},
		{"an empty list", 0, 2, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &sorted{rows: rows(tc.n)}
			ids, errs := collect(t, Walk(context.Background(), "test list", s.list, keyOf, tc.limit))
			if len(errs) != 0 {
				t.Fatalf("errors: %v", errs)
			}
			if len(ids) != tc.n {
				t.Fatalf("rows = %v, want %d rows", ids, tc.n)
			}
			for i, id := range ids {
				if want := fmt.Sprintf("r%02d", i); id != want {
					t.Errorf("row %d = %s, want %s: every row once, in key order", i, id, want)
				}
			}
			if len(s.afters) != tc.wantFetches {
				t.Errorf("pages read = %d, want %d: the walk ends on the first page shorter than the limit", len(s.afters), tc.wantFetches)
			}
			if s.afters[0] != (Key{}) {
				t.Errorf("the first page started after %s, want the zero Key", s.afters[0])
			}
			if tc.wantLastFrom != "" && s.afters[len(s.afters)-1].ID != tc.wantLastFrom {
				t.Errorf("the last page started after %s, want after %s: each page starts after the last row of the page before",
					s.afters[len(s.afters)-1], tc.wantLastFrom)
			}
		})
	}
}

// A row the job leaves as it was stays in the list. The walk still moves past
// it, because the next page starts after the last row listed, not after the
// last row done.
func TestWalk_RowsThatStayListedDoNotHoldBackTheRest(t *testing.T) {
	s := &sorted{rows: rows(5)}
	ids, errs := collect(t, Walk(context.Background(), "test list", s.list, keyOf, 2))
	if len(errs) != 0 || len(ids) != 5 {
		t.Fatalf("rows = %v, errors = %v; want all 5 rows once and no error", ids, errs)
	}
}

func TestWalk_BreakReadsNoFurtherPage(t *testing.T) {
	s := &sorted{rows: rows(5)}
	for range Walk(context.Background(), "test list", s.list, keyOf, 2) {
		break
	}
	if len(s.afters) != 1 {
		t.Errorf("pages read = %d, want 1: a break stops the walk", len(s.afters))
	}
}

func TestWalk_AListErrorIsYieldedOnceAndEndsTheWalk(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	list := func(_ context.Context, _ Key, _ int) ([]row, error) {
		calls++
		if calls == 2 {
			return nil, boom
		}
		return rows(2), nil
	}
	ids, errs := collect(t, Walk(context.Background(), "test list", list, keyOf, 2))
	if len(ids) != 2 || len(errs) != 1 || !errors.Is(errs[0], boom) {
		t.Fatalf("rows = %v, errors = %v; want the first page's 2 rows, then boom once", ids, errs)
	}
	if errors.Is(errs[0], ErrCut) {
		t.Errorf("a list error with a live context is not a cut: %v", errs[0])
	}
	if calls != 2 {
		t.Errorf("pages read = %d, want 2: nothing after the error", calls)
	}
}

func TestWalk_AContextThatEndedBeforeAPageIsACut(t *testing.T) {
	t.Run("before the first page", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s := &sorted{rows: rows(5)}
		ids, errs := collect(t, Walk(ctx, "test list", s.list, keyOf, 2))
		if len(ids) != 0 || len(errs) != 1 || !errors.Is(errs[0], ErrCut) || !errors.Is(errs[0], context.Canceled) {
			t.Fatalf("rows = %v, errors = %v; want no row and one cut that wraps context.Canceled", ids, errs)
		}
		if len(s.afters) != 0 {
			t.Errorf("pages read = %d, want 0", len(s.afters))
		}
	})

	t.Run("between pages, with a cause", func(t *testing.T) {
		lost := errors.New("lease lost")
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		s := &sorted{rows: rows(5)}
		var ids []string
		var errs []error
		for r, err := range Walk(ctx, "test list", s.list, keyOf, 2) {
			if err != nil {
				errs = append(errs, err)
				continue
			}
			ids = append(ids, r.id)
			cancel(lost)
		}
		if len(ids) != 2 {
			t.Errorf("rows = %v, want the first page's 2: the cut comes before the next page, not mid-page", ids)
		}
		if len(errs) != 1 || !errors.Is(errs[0], ErrCut) || !errors.Is(errs[0], context.Canceled) || !errors.Is(errs[0], lost) {
			t.Fatalf("errors = %v; want one cut that wraps context.Canceled and the cause", errs)
		}
		if got := errs[0].Error(); !strings.HasPrefix(got, "stopped before the end of the list") {
			t.Errorf("message = %q, want it to start with the plain cut: the caller adds which job", got)
		}
	})

	t.Run("a deadline", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
		defer cancel()
		_, errs := collect(t, Walk(ctx, "test list", (&sorted{rows: rows(1)}).list, keyOf, 2))
		if len(errs) != 1 || !errors.Is(errs[0], ErrCut) || !errors.Is(errs[0], context.DeadlineExceeded) {
			t.Fatalf("errors = %v; want one cut that wraps context.DeadlineExceeded", errs)
		}
	})

	t.Run("a list that failed because the context ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		list := func(ctx context.Context, _ Key, _ int) ([]row, error) {
			cancel()
			return nil, fmt.Errorf("query: %w", ctx.Err())
		}
		_, errs := collect(t, Walk(ctx, "test list", list, keyOf, 2))
		if len(errs) != 1 || !errors.Is(errs[0], ErrCut) || !strings.Contains(errs[0].Error(), "query: ") {
			t.Fatalf("errors = %v; want one cut that also carries the list's error", errs)
		}
	})
}

// A list that ignores the key it is given returns its first page again. The
// walk would work those rows again, and for ever; it stops before yielding
// any row of that page, with an error that names the list and the keys,
// which is what someone debugging the sort order needs.
func TestWalk_AListThatDoesNotMoveOnIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name    string
		page    func(after Key) []row
		wantIDs []string
	}{
		{"the same page again", func(Key) []row { return rows(2) }, []string{"r00", "r01"}},
		{"a page that ends earlier in time", func(after Key) []row {
			if after == (Key{}) {
				return []row{{at: t0.Add(time.Hour), id: "a"}, {at: t0.Add(2 * time.Hour), id: "b"}}
			}
			return []row{{at: t0.Add(2 * time.Hour), id: "c"}, {at: t0.Add(time.Hour), id: "d"}}
		}, []string{"a", "b"}},
		{"a page that starts earlier in time", func(after Key) []row {
			if after == (Key{}) {
				return []row{{at: t0.Add(time.Hour), id: "a"}, {at: t0.Add(2 * time.Hour), id: "b"}}
			}
			return []row{{at: t0.Add(time.Hour), id: "c"}, {at: t0.Add(3 * time.Hour), id: "d"}}
		}, []string{"a", "b"}},
		{"a key that is zero", func(Key) []row { return []row{{}, {}} }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			list := func(_ context.Context, after Key, _ int) ([]row, error) {
				calls++
				if calls > 3 {
					t.Fatal("the walk kept reading a list that does not move on")
				}
				return tc.page(after), nil
			}
			ids, errs := collect(t, Walk(context.Background(), "ListThings", list, keyOf, 2))
			if len(errs) != 1 {
				t.Fatalf("errors = %v, want one", errs)
			}
			if !slices.Equal(ids, tc.wantIDs) {
				t.Errorf("rows = %v, want %v: no row of the page that did not move on", ids, tc.wantIDs)
			}
			msg := errs[0].Error()
			if !strings.Contains(msg, "ListThings") || !strings.Contains(msg, "id \"") {
				t.Errorf("message = %q, want the list's name and the keys", msg)
			}
			if errors.Is(errs[0], ErrCut) {
				t.Errorf("a list that does not move on is not a cut: %v", errs[0])
			}
		})
	}
}

// Rows on one instant move on by id alone, and a list keyed by id alone (At
// zero) moves on too. Ids are not compared for order: the database's
// collation decides it.
func TestWalk_TiesAndIDOnlyKeysMoveOn(t *testing.T) {
	idOnly := []row{{id: "sub_b"}, {id: "sub_A"}, {id: "sub_c"}}
	calls := 0
	list := func(_ context.Context, after Key, _ int) ([]row, error) {
		calls++
		switch after.ID {
		case "":
			return idOnly[:2], nil
		case "sub_A":
			return idOnly[2:], nil
		}
		t.Fatalf("page after %s, want after the last row of the page before", after)
		return nil, nil
	}
	ids, errs := collect(t, Walk(context.Background(), "test list", list, keyOf, 2))
	if len(errs) != 0 || len(ids) != 3 {
		t.Fatalf("rows = %v, errors = %v; want 3 rows and no error", ids, errs)
	}
}

func TestWalk_APageSizeBelowOneIsAnError(t *testing.T) {
	s := &sorted{rows: rows(3)}
	_, errs := collect(t, Walk(context.Background(), "test list", s.list, keyOf, 0))
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "page size 0") {
		t.Fatalf("errors = %v, want one naming the page size", errs)
	}
	if len(s.afters) != 0 {
		t.Errorf("pages read = %d, want 0", len(s.afters))
	}
}
