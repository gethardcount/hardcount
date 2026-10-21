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

type stamped struct {
	name     string
	at       time.Time
	recorded time.Time
	rank     int
}

func stampedNames(rows []stamped) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.name
	}
	return out
}

func TestOrder(t *testing.T) {
	story := time.Date(2027, 2, 1, 4, 30, 0, 0, time.UTC)
	wall := func(h int) time.Time { return time.Date(2026, 10, 21, h, 0, 0, 0, time.UTC) }
	order := func(rows []stamped) []string {
		Order(rows,
			func(r stamped) time.Time { return r.at },
			func(r stamped) time.Time { return r.recorded },
			func(a, b stamped) bool { return a.rank < b.rank })
		return stampedNames(rows)
	}
	check := func(t *testing.T, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("a fully stamped same-instant group reads in the real order, against the rank", func(t *testing.T) {
		check(t, order([]stamped{
			// The rank alone would read finalize, email, void.
			{"finalize", story, wall(15), 10},
			{"email", story, wall(10), 50},
			{"void", story, wall(9), 90},
		}), "void", "email", "finalize")
	})

	t.Run("one bare row sends the group back to the rank", func(t *testing.T) {
		check(t, order([]stamped{
			{"email", story, wall(8), 50},
			{"credit note", story, time.Time{}, 40},
			{"finalize", story, wall(9), 10},
		}), "finalize", "credit note", "email")
	})

	t.Run("equal recorded stamps keep the rank, then insertion", func(t *testing.T) {
		check(t, order([]stamped{
			{"email b", story, wall(9), 50},
			{"start", story, wall(9), 20},
			{"email a", story, wall(9), 50},
		}), "start", "email b", "email a")
	})

	t.Run("the entity instant always comes first", func(t *testing.T) {
		check(t, order([]stamped{
			{"later story, recorded first", story.Add(time.Hour), wall(8), 0},
			{"earlier story, recorded last", story, wall(18), 99},
		}), "earlier story, recorded last", "later story, recorded first")
	})
}
