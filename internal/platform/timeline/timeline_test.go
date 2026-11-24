package timeline

import (
	"testing"
	"time"
)

type row struct {
	name string
	at   time.Time
	rank int
}

func names(rows []row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.name
	}
	return out
}

func TestSortStable(t *testing.T) {
	base := time.Date(2027, 3, 8, 18, 30, 0, 0, time.UTC)
	at := func(r row) time.Time { return r.at }
	tie := func(a, b row) bool { return a.rank < b.rank }

	t.Run("full-precision instants dominate the tie rule", func(t *testing.T) {
		// 36ms apart — a second-truncated axis would collide and let the
		// (deliberately inverted) rank win.
		rows := []row{
			{"later-low-rank", base.Add(36 * time.Millisecond), 0},
			{"earlier-high-rank", base.Add(4 * time.Millisecond), 99},
		}
		SortStable(rows, at, tie)
		if rows[0].name != "earlier-high-rank" {
			t.Errorf("sub-second precision must beat rank: got %v", names(rows))
		}
	})

	t.Run("exact ties consult the causal rule regardless of insertion", func(t *testing.T) {
		rows := []row{
			{"terminal", base, 90},
			{"start", base, 10},
			{"middle", base, 50},
		}
		SortStable(rows, at, tie)
		if rows[0].name != "start" || rows[1].name != "middle" || rows[2].name != "terminal" {
			t.Errorf("exact ties must order causally: got %v", names(rows))
		}
	})

	t.Run("ties equal under both axes keep insertion order (stable)", func(t *testing.T) {
		rows := []row{
			{"first-inserted", base, 10},
			{"second-inserted", base, 10},
		}
		SortStable(rows, at, tie)
		if rows[0].name != "first-inserted" {
			t.Errorf("stability lost on full tie: got %v", names(rows))
		}
	})
}
