package analytics

import (
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/gethardcount/hardcount/internal/platform/postgres"
)

// Handler serves analytics API endpoints.
//
// All responses are tenant-scoped via RLS (postgres.TxTenant). Money is cents
// (int64), rates are fractions in [0, 1] (float64), and periods are closed-open
// windows [start, end) in UTC.
type Handler struct {
	db *postgres.DB
}

func NewHandler(db *postgres.DB) *Handler {
	return &Handler{db: db}
}

func (h *Handler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/overview", h.overview)
	r.Get("/revenue-chart", h.revenueChart)
	r.Get("/mrr-movement", h.mrrMovement)
	r.Get("/usage", h.usageAnalytics)
	return r
}

// Period models the closed-open comparison window [Start, End) against a
// matching prior window [PrevStart, PrevEnd) of equal length.
type Period struct {
	Key       string
	Start     time.Time
	End       time.Time
	PrevStart time.Time
	PrevEnd   time.Time
	Trunc     string // "day" | "month"
}

// parsePeriod resolves the `period` query param to a Period window.
// Unknown values fall through to "30d".
func parsePeriod(key string) Period {
	now := time.Now().UTC()
	var days int
	trunc := "day"
	switch key {
	case "7d":
		days = 7
	case "90d":
		days = 90
	case "12m":
		days = 365
		trunc = "month"
	case "30d":
		days = 30
	default:
		key = "30d"
		days = 30
	}
	start := now.AddDate(0, 0, -days)
	return Period{
		Key:       key,
		Start:     start,
		End:       now,
		PrevStart: start.AddDate(0, 0, -days),
		PrevEnd:   start,
		Trunc:     trunc,
	}
}
