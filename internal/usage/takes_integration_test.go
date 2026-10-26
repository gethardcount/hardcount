package usage

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"testing"

	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/testutil"
)

// TestTakes_AgreesWithPostgresContainment checks the Go matcher ingest
// classifies with (takes) against the close's own test, `properties @>
// dimension_match` in Postgres (AggregateClaims), on a generated corpus:
// labels as ingest stores them (propertiesJSON), matches as a price stores
// them (string values only, ADR-130 §1). The labels mix strings with the
// numbers, booleans and nulls an event may carry, and strings that differ
// only in case, spacing, escaping or Unicode normal form.
//
// Mutation checks: compare label values with fmt.Sprint (so the number 1
// equals "1") → disagreements; skip keys absent from the labels → the
// matcher takes events the close does not.
func TestTakes_AgreesWithPostgresContainment(t *testing.T) {
	db := testutil.SetupTestDB(t)
	ctx := context.Background()

	keys := []string{"model", "tier", "region", "Model"}
	strs := []string{"a", "A", "a ", " a", "1", "1.0", "true", "false", "null", "", "é", "é", "<b>&", `"q"`, "\\", "😀", "claude-haiku-5-5"}
	vals := []any{1.0, 1.5, 0.0, -1.0, true, false, nil}
	for _, s := range strs {
		vals = append(vals, s)
	}
	rng := rand.New(rand.NewPCG(20261026, 6))
	randLabels := func() map[string]any {
		out := map[string]any{}
		for _, k := range keys {
			if rng.IntN(3) > 0 {
				out[k] = vals[rng.IntN(len(vals))]
			}
		}
		return out
	}
	randMatch := func(labels map[string]any) map[string]string {
		out := map[string]string{}
		for _, k := range keys {
			switch rng.IntN(4) {
			case 0:
				// Copy the label when it is a string, so many pairs match.
				if s, ok := labels[k].(string); ok {
					out[k] = s
				}
			case 1:
				out[k] = strs[rng.IntN(len(strs))]
			}
		}
		return out
	}

	const n = 4000
	labelDocs := make([]string, n)
	matchDocs := make([]string, n)
	want := make([]bool, n)
	for i := range n {
		labels := randLabels()
		match := randMatch(labels)
		doc, err := propertiesJSON(labels)
		if err != nil {
			t.Fatalf("labels %v: %v", labels, err)
		}
		m, err := json.Marshal(match)
		if err != nil {
			t.Fatalf("match %v: %v", match, err)
		}
		// Read the labels back the way the held release reads a stored row.
		var stored map[string]any
		if err := json.Unmarshal([]byte(doc), &stored); err != nil {
			t.Fatalf("decode %s: %v", doc, err)
		}
		labelDocs[i], matchDocs[i], want[i] = doc, string(m), takes(match, labels)
		if takes(match, stored) != want[i] {
			t.Fatalf("takes(%s, %s) differs between the labels sent and the labels stored", m, doc)
		}
	}

	rows, err := db.Pool.QueryContext(ctx, `
		SELECT l::jsonb @> m::jsonb
		FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS c(l, m, i)
		ORDER BY i`, postgres.StringArray(labelDocs), postgres.StringArray(matchDocs))
	if err != nil {
		t.Fatalf("containment: %v", err)
	}
	defer func() { _ = rows.Close() }()
	i, matched, disagree := 0, 0, 0
	for rows.Next() {
		var got bool
		if err := rows.Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got {
			matched++
		}
		if got != want[i] {
			disagree++
			if disagree <= 10 {
				t.Errorf("labels %s, match %s: Postgres @> = %v, takes = %v", labelDocs[i], matchDocs[i], got, want[i])
			}
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if i != n {
		t.Fatalf("compared %d pairs, want %d", i, n)
	}
	if disagree > 0 {
		t.Errorf("%d of %d pairs disagree", disagree, n)
	}
	// The corpus must exercise both answers, or it proves nothing.
	if matched < n/10 || matched > n-n/10 {
		t.Errorf("%d of %d pairs match: the corpus is too one-sided", matched, n)
	}
}
