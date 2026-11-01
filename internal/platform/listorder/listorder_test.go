package listorder

import (
	"errors"
	"strings"
	"testing"

	"github.com/gethardcount/hardcount/internal/errs"
)

var testSet = New(
	Key{Name: "created_at", Column: "created_at"},
	Key{Name: "amount", Column: "(amount_cents)"},
	Key{Name: "name", Column: "display_name"},
)

// TestOrderBy_KnownKeys: each key orders by its column, an empty sort is the
// first key, an empty dir is desc, and the tie-break follows the direction.
func TestOrderBy_KnownKeys(t *testing.T) {
	for _, c := range []struct{ sort, dir, want string }{
		{"", "", "created_at DESC, id DESC"},
		{"created_at", "asc", "created_at ASC, id ASC"},
		{"amount", "desc", "(amount_cents) DESC, id DESC"},
		{"name", "", "display_name DESC, id DESC"},
	} {
		got, err := testSet.OrderBy(c.sort, c.dir, "id")
		if err != nil || got != c.want {
			t.Errorf("OrderBy(%q, %q) = %q, %v; want %q", c.sort, c.dir, got, err, c.want)
		}
	}
}

// TestOrderBy_RefusesOutsideTheSet: a key or a dir outside the set is a
// validation error on that param naming the allowed values. They used to
// fall back to created_at and desc without a word; nothing outside the set
// ever reaches the ORDER BY.
// Mutation-verify: return the default column for an unknown key → fails.
func TestOrderBy_RefusesOutsideTheSet(t *testing.T) {
	for _, c := range []struct{ sort, dir, field, names string }{
		{"amount_due", "", "sort", "created_at, amount, name"},
		{"DROP TABLE invoices", "", "sort", "created_at, amount, name"},
		{"id; --", "asc", "sort", "created_at, amount, name"},
		{"", "ASC", "dir", "asc or desc"},
		{"name", "up", "dir", "asc or desc"},
	} {
		got, err := testSet.OrderBy(c.sort, c.dir, "id")
		if err == nil {
			t.Errorf("OrderBy(%q, %q) = %q, want a refusal", c.sort, c.dir, got)
			continue
		}
		if !errors.Is(err, errs.ErrValidation) || errs.Field(err) != c.field || !strings.Contains(err.Error(), c.names) {
			t.Errorf("OrderBy(%q, %q) error = %v (field %q), want a validation error on %s naming %s", c.sort, c.dir, err, errs.Field(err), c.field, c.names)
		}
		if verr := testSet.Validate(c.sort, c.dir); verr == nil {
			t.Errorf("Validate(%q, %q) = nil, want the same refusal", c.sort, c.dir)
		}
	}
	if err := testSet.Validate("amount", "asc"); err != nil {
		t.Errorf("Validate of a known pair = %v, want nil", err)
	}
}
