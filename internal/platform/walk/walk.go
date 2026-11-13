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

// Walk yields every row of the list, page by page, until a page comes back
// with fewer than limit rows.
//
// Four errors end a walk. Each is yielded once, as the last value, with a
// zero row:
//   - a page size below 1;
//   - a list error, passed on as the list returned it;
//   - a context that has ended, before a page or while the list was read:
//     "stopped before the end of the list", wrapping the context's error and
//     its cause;
//   - a page that is not after the page before it: its first or last row is
//     the row the page before ended on, or, for a list keyed by a time, has
//     an earlier time. The list is not ordering or paging by its key, a bug.
//     None of that page's rows is yielded, since going on would work the same
//     rows again, and for ever. The error names the list and the keys; on an
//     operator's screen it reads as an internal error.
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
				yield(zero, cut(ctx, nil))
				return
			}
			page, err := list(ctx, after, limit)
			if err != nil {
				if ctx.Err() != nil {
					err = cut(ctx, err)
				}
				yield(zero, err)
				return
			}
			if len(page) == 0 {
				return
			}
			first, last := key(page[0]), key(page[len(page)-1])
			if !movedOn(after, first) || !movedOn(after, last) {
				yield(zero, &orderError{list: name, first: first, last: last, after: after})
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

// cut is the error for a context that has ended: the context's error, its
// cause when that differs (leader.ErrLeaseLost, for one), and the list's own
// error when reading it failed.
func cut(ctx context.Context, listErr error) error {
	err := ctx.Err()
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, err) {
		err = fmt.Errorf("%w (%w)", err, cause)
	}
	if listErr != nil {
		return fmt.Errorf("stopped before the end of the list: %w: %w", err, listErr)
	}
	return fmt.Errorf("stopped before the end of the list: %w", err)
}

// orderError is a page that is not after the page before it.
type orderError struct {
	list               string
	first, last, after Key
}

func (e *orderError) Error() string {
	return fmt.Sprintf("walk %s: a page ran from %s to %s, which is not after the page before it (%s): the list does not order or page by its key",
		e.list, e.first, e.last, e.after)
}

// OperatorSafeMessage is what an operator's screen shows (errs.SafeMessageError):
// the error is a bug, and its detail belongs in the log.
func (e *orderError) OperatorSafeMessage() string {
	return "An internal error occurred. See server logs for details."
}

// String prints the key for an error message.
func (k Key) String() string {
	if k.At.IsZero() {
		return fmt.Sprintf("(id %q)", k.ID)
	}
	return fmt.Sprintf("(%s, id %q)", k.At.UTC().Format(time.RFC3339Nano), k.ID)
}
