// Package walk reads a list to its end, one page at a time, in the order of
// the list's key.
//
// A job that works a list ranges over Walk and keeps its own loop body: what
// it counts, which rows it skips, and what it does with an error. Walk only
// pages. Each page starts after the last row of the page before, so a row the
// job could not finish, and which stays in the list, never holds back the rows
// behind it.
package walk

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"
)

// Key is a row's place in its list: the column the list orders by, then the
// row's id. A list ordered by id alone leaves At zero. The zero Key is the
// start of the list.
type Key struct {
	At time.Time
	ID string
}

// List returns up to limit rows that come after the key, in key order.
type List[T any] func(ctx context.Context, after Key, limit int) ([]T, error)

// ErrCut means the context ended before the list did. The error Walk yields
// wraps it, the context's error and the context's cause.
var ErrCut = errors.New("stopped before the end of the list")

// Walk yields every row of the list, page by page, until a page comes back
// with fewer than limit rows.
//
// A list error, a cut and a list that does not move on are each yielded once,
// as the last value, with a zero row:
//   - a list error is passed on as the list returned it;
//   - when ctx has ended before a page, or the list failed because it had,
//     the error wraps ErrCut;
//   - when a page's first or last row is not after the page before it (the
//     same row again, or earlier in time), the list is not ordering or paging
//     by its key. None of that page's rows is yielded: going on would work
//     the same rows again, and for ever. The error names the list and the
//     keys.
//
// Breaking out of the loop stops the walk; no further page is read.
func Walk[T any](ctx context.Context, name string, list List[T], key func(T) Key, limit int) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		if limit < 1 {
			yield(zero, fmt.Errorf("walk %s: page size %d, want at least 1", name, limit))
			return
		}
		var after Key
		for {
			if ctx.Err() != nil {
				yield(zero, cut(ctx))
				return
			}
			page, err := list(ctx, after, limit)
			if err != nil {
				if ctx.Err() != nil {
					err = fmt.Errorf("%w: %w", cut(ctx), err)
				}
				yield(zero, err)
				return
			}
			if len(page) == 0 {
				return
			}
			first, last := key(page[0]), key(page[len(page)-1])
			if !movedOn(after, first) || !movedOn(after, last) {
				yield(zero, fmt.Errorf("walk %s: a page ran from %s to %s, which is not after the page before it (%s): the list does not order or page by its key", name, first, last, after))
				return
			}
			for _, row := range page {
				if !yield(row, nil) {
					return
				}
			}
			if len(page) < limit {
				return
			}
			after = last
		}
	}
}

// movedOn reports whether next can come after prev. Ids are compared only for
// being equal: the database orders them by its collation, which Go cannot
// repeat.
func movedOn(prev, next Key) bool {
	if next.At.Before(prev.At) {
		return false
	}
	return !next.At.Equal(prev.At) || next.ID != prev.ID
}

// cut is ErrCut with the context's error and, when it differs, its cause
// (leader.ErrLeaseLost, for one).
func cut(ctx context.Context) error {
	err, cause := ctx.Err(), context.Cause(ctx)
	if cause == nil || errors.Is(cause, err) {
		return fmt.Errorf("%w: %w", ErrCut, err)
	}
	return fmt.Errorf("%w: %w (%w)", ErrCut, err, cause)
}

// String prints the key for an error message.
func (k Key) String() string {
	if k.At.IsZero() {
		return fmt.Sprintf("(id %q)", k.ID)
	}
	return fmt.Sprintf("(%s, id %q)", k.At.UTC().Format(time.RFC3339Nano), k.ID)
}
