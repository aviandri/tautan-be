package server

import (
	"math"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type seriesPoint struct {
	Date   string `json:"date"` // YYYY-MM-DD (UTC)
	Views  int    `json:"views"`
	Clicks int    `json:"clicks"`
}

type topLink struct {
	BlockID string `json:"block_id"`
	Title   string `json:"title"`
	Clicks  int    `json:"clicks"`
}

type topReferrer struct {
	Referrer string `json:"referrer"` // host, "" = direct
	Count    int    `json:"count"`
}

type analyticsSummary struct {
	Views        int           `json:"views"`
	Clicks       int           `json:"clicks"`
	CTR          float64       `json:"ctr"` // clicks / views, 0..1
	Series       []seriesPoint `json:"series"`
	TopLinks     []topLink     `json:"top_links"`
	TopReferrers []topReferrer `json:"top_referrers"`
}

var analyticsRanges = map[string]int{"7d": 7, "30d": 30, "90d": 90}

// handleAnalyticsSummary aggregates events over the last N UTC days, today included.
func (s *Server) handleAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	rng := r.URL.Query().Get("range")
	if rng == "" {
		rng = "7d"
	}
	days, ok := analyticsRanges[rng]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_range", "range must be 7d, 30d or 90d")
		return
	}
	ctx, pid := r.Context(), profileID(r)
	today := s.now().UTC().Truncate(24 * time.Hour)
	from := today.AddDate(0, 0, -(days - 1))
	to := today.AddDate(0, 0, 1)

	out := analyticsSummary{}
	rows, err := s.db.Query(ctx, `
		SELECT to_char(d AT TIME ZONE 'UTC', 'YYYY-MM-DD'),
		       COALESCE(SUM((e.kind = 'view')::int), 0),
		       COALESCE(SUM((e.kind = 'click')::int), 0)
		FROM generate_series($2::timestamptz, $3::timestamptz - interval '24 hours', interval '24 hours') d
		LEFT JOIN events e ON e.profile_id = $1 AND e.created_at >= d AND e.created_at < d + interval '24 hours'
		GROUP BY d ORDER BY d`, pid, from, to)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out.Series, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (seriesPoint, error) {
		var p seriesPoint
		err := r.Scan(&p.Date, &p.Views, &p.Clicks)
		return p, err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	for _, p := range out.Series {
		out.Views += p.Views
		out.Clicks += p.Clicks
	}
	if out.Views > 0 {
		out.CTR = math.Round(float64(out.Clicks)/float64(out.Views)*10000) / 10000
	}

	rows, err = s.db.Query(ctx, `
		SELECT b.id, COALESCE(b.data->>'title', ''), count(*) AS n
		FROM events e JOIN blocks b ON b.id = e.block_id
		WHERE e.profile_id = $1 AND e.kind = 'click' AND e.created_at >= $2 AND e.created_at < $3
		GROUP BY b.id ORDER BY n DESC, b.id LIMIT 10`, pid, from, to)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out.TopLinks, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (topLink, error) {
		var l topLink
		err := r.Scan(&l.BlockID, &l.Title, &l.Clicks)
		return l, err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}

	rows, err = s.db.Query(ctx, `
		SELECT referrer, count(*) AS n FROM events
		WHERE profile_id = $1 AND kind = 'view' AND created_at >= $2 AND created_at < $3
		GROUP BY referrer ORDER BY n DESC, referrer LIMIT 10`, pid, from, to)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out.TopReferrers, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (topReferrer, error) {
		var t topReferrer
		err := r.Scan(&t.Referrer, &t.Count)
		return t, err
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
