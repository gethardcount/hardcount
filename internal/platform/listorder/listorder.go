// Package listorder is the one rule for a list's ?sort and ?dir: a closed
// set of sort keys, each naming the SQL column it orders by, and a direction
// of asc or desc. An empty sort is the list's default order and an empty dir
// is desc. Anything else is refused with errs.Invalid naming the allowed
// values, so it answers 422 with the param set.
//
// The lists used to map an unknown key to created_at and an unknown dir to
// desc without a word: a typo or a stale bookmark came back in an order the
// caller did not ask for, read as the order it did. A closed set is also
// what keeps raw input out of the ORDER BY.
package listorder

import (
	"fmt"
	"strings"

	"github.com/gethardcount/hardcount/internal/errs"
)

// Key is one sort key and the SQL column or expression it orders by.
type Key struct {
	Name   string
	Column string
}

// Set is a list's sort keys. The first key is the default order.
type Set struct {
	keys []Key
	cols map[string]string
}

// New builds a Set from its keys, the default first. A name used twice is a
// programming error and panics at init.
func New(keys ...Key) Set {
	if len(keys) == 0 {
		panic("listorder: a list needs at least one sort key")
	}
	cols := make(map[string]string, len(keys))
	for _, k := range keys {
		if _, dup := cols[k.Name]; dup {
			panic("listorder: sort key " + k.Name + " is listed twice")
		}
		cols[k.Name] = k.Column
	}
	return Set{keys: keys, cols: cols}
}

// Validate refuses a sort or dir outside the set. Empty is allowed for both.
func (s Set) Validate(sort, dir string) error {
	if _, err := s.column(sort); err != nil {
		return err
	}
	_, err := direction(dir)
	return err
}

// OrderBy returns the ORDER BY clause for sort and dir, with tieBreak in the
// same direction so a run of ties reads as one ordered group. It refuses what
// Validate refuses.
func (s Set) OrderBy(sort, dir, tieBreak string) (string, error) {
	col, err := s.column(sort)
	if err != nil {
		return "", err
	}
	d, err := direction(dir)
	if err != nil {
		return "", err
	}
	return col + " " + d + ", " + tieBreak + " " + d, nil
}

func (s Set) column(sort string) (string, error) {
	if sort == "" {
		return s.keys[0].Column, nil
	}
	if col, ok := s.cols[sort]; ok {
		return col, nil
	}
	names := make([]string, len(s.keys))
	for i, k := range s.keys {
		names[i] = k.Name
	}
	return "", errs.Invalid("sort", fmt.Sprintf("sort %q is not one of %s", sort, strings.Join(names, ", ")))
}

func direction(dir string) (string, error) {
	switch dir {
	case "", "desc":
		return "DESC", nil
	case "asc":
		return "ASC", nil
	}
	return "", errs.Invalid("dir", fmt.Sprintf("dir %q is not asc or desc", dir))
}
