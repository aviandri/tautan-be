package server

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

// analyticsQuery is the parsed range, interval, timezone and filters shared by
// every analytics v2 endpoint.
type analyticsQuery struct {
	from, to         time.Time // absolute bounds [from, to)
	prevFrom, prevTo time.Time // the previous period of equal length
	loc              *time.Location
	interval         string // hour | day | week
	where            string // SQL predicate over events e (filters only)
	args             []any  // args for where, starting at $4
}

// Breakdown dimensions and their event columns. Also the filter params.
var analyticsDims = map[string]string{
	"country": "country", "device": "device", "browser": "browser", "os": "os",
	"utm_source": "utm_source", "utm_medium": "utm_medium", "utm_campaign": "utm_campaign",
	"channel": "channel", "referrer": "referrer",
}

var dimOrder = []string{"country", "device", "browser", "os", "utm_source", "utm_medium", "utm_campaign", "channel", "referrer"}

const maxCustomDays = 731

func parseAnalyticsQuery(r *http.Request, now time.Time) (*analyticsQuery, error) {
	q := r.URL.Query()
	loc := time.UTC
	if tz := q.Get("tz"); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return nil, badReq("invalid_tz", "tz must be an IANA time zone such as Asia/Jakarta")
		}
		loc = l
	}
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	var from, to time.Time
	switch rng := q.Get("range"); rng {
	case "today":
		from, to = today, today.AddDate(0, 0, 1)
	case "yesterday":
		from, to = today.AddDate(0, 0, -1), today
	case "7d", "30d", "90d", "":
		n := map[string]int{"7d": 7, "30d": 30, "90d": 90, "": 30}[rng]
		from, to = today.AddDate(0, 0, -(n-1)), today.AddDate(0, 0, 1)
	case "month":
		from, to = time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc), today.AddDate(0, 0, 1)
	case "year":
		from, to = time.Date(local.Year(), 1, 1, 0, 0, 0, 0, loc), today.AddDate(0, 0, 1)
	case "custom":
		f, err1 := time.ParseInLocation("2006-01-02", q.Get("from"), loc)
		t, err2 := time.ParseInLocation("2006-01-02", q.Get("to"), loc)
		if err1 != nil || err2 != nil || t.Before(f) {
			return nil, badReq("invalid_range", "custom range needs from and to as YYYY-MM-DD with from <= to")
		}
		from, to = f, t.AddDate(0, 0, 1)
		if to.Sub(from) > maxCustomDays*24*time.Hour+time.Hour {
			return nil, badReq("invalid_range", "custom range can be at most 2 years")
		}
	default:
		return nil, badReq("invalid_range", "range must be today, yesterday, 7d, 30d, 90d, month, year or custom")
	}
	span := to.Sub(from)
	interval := q.Get("interval")
	switch {
	case interval == "":
		interval = "week"
		if span <= 49*time.Hour {
			interval = "hour"
		} else if span <= 93*24*time.Hour {
			interval = "day"
		}
	case interval == "hour" && span > 8*24*time.Hour:
		return nil, badReq("invalid_interval", "hourly buckets are available for ranges up to 7 days")
	case interval == "day" && span > 367*24*time.Hour:
		return nil, badReq("invalid_interval", "daily buckets are available for ranges up to a year")
	case interval != "hour" && interval != "day" && interval != "week":
		return nil, badReq("invalid_interval", "interval must be hour, day or week")
	}
	aq := &analyticsQuery{from: from, to: to, prevFrom: from.Add(-span), prevTo: from, loc: loc, interval: interval}
	// Filters: any dimension param present (even empty, e.g. referrer= for direct).
	var preds []string
	for _, d := range dimOrder {
		if vals, ok := q[d]; ok {
			aq.args = append(aq.args, vals[0])
			preds = append(preds, fmt.Sprintf("e.%s = $%d", analyticsDims[d], 3+len(aq.args)))
		}
	}
	aq.where = "TRUE"
	if len(preds) > 0 {
		aq.where = strings.Join(preds, " AND ")
	}
	return aq, nil
}

// base returns the shared predicate and args ($1 profile, $2 from, $3 to, then filters).
func (aq *analyticsQuery) base(pid string, from, to time.Time) (string, []any) {
	args := append([]any{pid, from, to}, aq.args...)
	return `e.profile_id = $1 AND e.created_at >= $2 AND e.created_at < $3 AND ` + aq.where, args
}

type totals struct {
	Visitors   int     `json:"visitors"`
	Views      int     `json:"views"`
	Clicks     int     `json:"clicks"`
	CTR        float64 `json:"ctr"`        // clicks / views
	Engagement float64 `json:"engagement"` // share of visitors who clicked something
}

func round4(x float64) float64 { return math.Round(x*10000) / 10000 }

func (s *Server) analyticsTotals(ctx context.Context, aq *analyticsQuery, pid string, from, to time.Time) (totals, error) {
	where, args := aq.base(pid, from, to)
	var t totals
	var engaged int
	err := s.db.QueryRow(ctx, `SELECT
			count(*) FILTER (WHERE e.kind = 'view'),
			count(*) FILTER (WHERE e.kind = 'click'),
			count(DISTINCT e.visitor_hash) FILTER (WHERE e.kind = 'view' AND e.visitor_hash <> ''),
			count(DISTINCT e.visitor_hash) FILTER (WHERE e.kind = 'click' AND e.visitor_hash <> '')
		FROM events e WHERE `+where, args...).Scan(&t.Views, &t.Clicks, &t.Visitors, &engaged)
	if t.Views > 0 {
		t.CTR = round4(float64(t.Clicks) / float64(t.Views))
	}
	if t.Visitors > 0 {
		t.Engagement = round4(math.Min(1, float64(engaged)/float64(t.Visitors)))
	}
	return t, err
}

// pctChange is the % change from prev to cur (1 decimal), or nil when prev is 0.
func pctChange(cur, prev float64) *float64 {
	if prev == 0 {
		return nil
	}
	v := math.Round((cur-prev)/prev*1000) / 10
	return &v
}

type seriesRow struct {
	T        string `json:"t"`
	Views    int    `json:"views"`
	Clicks   int    `json:"clicks"`
	Visitors int    `json:"visitors"`
}

var intervalFmt = map[string]string{"hour": `YYYY-MM-DD"T"HH24:00`, "day": "YYYY-MM-DD", "week": "YYYY-MM-DD"}

// bucketStart truncates a local time to the start of its bucket (weeks start Monday).
func bucketStart(t time.Time, interval string) time.Time {
	switch interval {
	case "hour":
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
	case "week":
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
	default:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	}
}

// series returns zero-filled buckets in local time. extra narrows events
// further (e.g. one block); each %d in it is the arg index of the matching extraArg.
func (s *Server) analyticsSeries(ctx context.Context, aq *analyticsQuery, pid, extra string, extraArgs ...any) ([]seriesRow, error) {
	where, args := aq.base(pid, aq.from, aq.to)
	if extra != "" {
		idx := make([]any, len(extraArgs))
		for i := range idx {
			idx[i] = len(args) + 1 + i
		}
		where += " AND " + fmt.Sprintf(extra, idx...)
		args = append(args, extraArgs...)
	}
	const layout = "2006-01-02 15:04:05"
	start := bucketStart(aq.from.In(aq.loc), aq.interval).Format(layout)
	last := bucketStart(aq.to.In(aq.loc).Add(-time.Nanosecond), aq.interval).Format(layout)
	n := len(args)
	args = append(args, aq.loc.String(), start, last)
	q := fmt.Sprintf(`
		WITH buckets AS (
			SELECT generate_series($%[2]d::timestamp, $%[3]d::timestamp, interval '1 %[4]s') AS t
		), agg AS (
			SELECT date_trunc('%[4]s', e.created_at AT TIME ZONE $%[1]d) AS t,
				count(*) FILTER (WHERE e.kind = 'view') AS views,
				count(*) FILTER (WHERE e.kind = 'click') AS clicks,
				count(DISTINCT e.visitor_hash) FILTER (WHERE e.kind = 'view' AND e.visitor_hash <> '') AS visitors
			FROM events e WHERE %[5]s GROUP BY 1
		)
		SELECT to_char(b.t, '%[6]s'), COALESCE(a.views, 0), COALESCE(a.clicks, 0), COALESCE(a.visitors, 0)
		FROM buckets b LEFT JOIN agg a ON a.t = b.t ORDER BY b.t`,
		n+1, n+2, n+3, aq.interval, where, intervalFmt[aq.interval])
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (seriesRow, error) {
		var p seriesRow
		err := r.Scan(&p.T, &p.Views, &p.Clicks, &p.Visitors)
		return p, err
	})
}

func (aq *analyticsQuery) rangeJSON() map[string]any {
	return map[string]any{
		"from": aq.from.In(aq.loc).Format(time.RFC3339), "to": aq.to.In(aq.loc).Format(time.RFC3339),
		"tz": aq.loc.String(), "interval": aq.interval,
	}
}

func (s *Server) handleAnalyticsOverview(w http.ResponseWriter, r *http.Request) {
	aq, err := parseAnalyticsQuery(r, s.now())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	ctx, pid := r.Context(), profileID(r)
	cur, err := s.analyticsTotals(ctx, aq, pid, aq.from, aq.to)
	if err != nil {
		s.internalError(w, err)
		return
	}
	prev, err := s.analyticsTotals(ctx, aq, pid, aq.prevFrom, aq.prevTo)
	if err != nil {
		s.internalError(w, err)
		return
	}
	series, err := s.analyticsSeries(ctx, aq, pid, "")
	if err != nil {
		s.internalError(w, err)
		return
	}
	links, err := s.analyticsBlocks(ctx, aq, pid, 1, true)
	if err != nil {
		s.internalError(w, err)
		return
	}
	var top any
	if len(links) > 0 && links[0].Clicks > 0 {
		top = map[string]any{"block_id": links[0].BlockID, "title": links[0].Title, "clicks": links[0].Clicks}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": aq.rangeJSON(), "totals": cur, "previous": prev,
		"change": map[string]*float64{
			"visitors":   pctChange(float64(cur.Visitors), float64(prev.Visitors)),
			"views":      pctChange(float64(cur.Views), float64(prev.Views)),
			"clicks":     pctChange(float64(cur.Clicks), float64(prev.Clicks)),
			"ctr":        pctChange(cur.CTR, prev.CTR),
			"engagement": pctChange(cur.Engagement, prev.Engagement),
		},
		"series": series, "top_link": top,
	})
}

type breakdownRow struct {
	Value    string `json:"value"`
	Views    int    `json:"views"`
	Clicks   int    `json:"clicks"`
	Visitors int    `json:"visitors"`
}

func (s *Server) analyticsBreakdown(ctx context.Context, aq *analyticsQuery, pid, dim string, limit int) ([]breakdownRow, error) {
	where, args := aq.base(pid, aq.from, aq.to)
	args = append(args, limit)
	rows, err := s.db.Query(ctx, fmt.Sprintf(`SELECT e.%[1]s,
			count(*) FILTER (WHERE e.kind = 'view'), count(*) FILTER (WHERE e.kind = 'click'),
			count(DISTINCT e.visitor_hash) FILTER (WHERE e.kind = 'view' AND e.visitor_hash <> '')
		FROM events e WHERE %[2]s GROUP BY 1 ORDER BY 2 DESC, 3 DESC, 1 LIMIT $%[3]d`,
		analyticsDims[dim], where, len(args)), args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (breakdownRow, error) {
		var b breakdownRow
		err := r.Scan(&b.Value, &b.Views, &b.Clicks, &b.Visitors)
		return b, err
	})
}

func breakdownParams(r *http.Request) (string, int, error) {
	dim := r.URL.Query().Get("dimension")
	if _, ok := analyticsDims[dim]; !ok {
		return "", 0, badReq("invalid_dimension", "dimension must be one of %s", strings.Join(dimOrder, ", "))
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 100 {
			return "", 0, badReq("invalid_limit", "limit must be 1-100")
		}
		limit = n
	}
	return dim, limit, nil
}

func (s *Server) handleAnalyticsBreakdown(w http.ResponseWriter, r *http.Request) {
	aq, err := parseAnalyticsQuery(r, s.now())
	if err == nil {
		var dim string
		var limit int
		if dim, limit, err = breakdownParams(r); err == nil {
			var items []breakdownRow
			if items, err = s.analyticsBreakdown(r.Context(), aq, profileID(r), dim, limit); err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"range": aq.rangeJSON(), "dimension": dim, "items": items})
				return
			}
		}
	}
	s.writeErr(w, err)
}

type blockStat struct {
	BlockID   string         `json:"block_id"`
	Type      string         `json:"type"`
	Title     string         `json:"title"`
	Clicks    int            `json:"clicks"`
	Actions   map[string]int `json:"actions"` // clicks by meta.action (P3-A); {} when none
	IsVisible bool           `json:"is_visible"`
	Deleted   bool           `json:"deleted"`
	CreatedAt time.Time      `json:"created_at"`
}

// analyticsBlocks lists the page's blocks (deleted ones included) with clicks in range.
func (s *Server) analyticsBlocks(ctx context.Context, aq *analyticsQuery, pid string, limit int, clickedOnly bool) ([]blockStat, error) {
	where, args := aq.base(pid, aq.from, aq.to)
	args = append(args, limit)
	having := ""
	if clickedOnly {
		having = "HAVING count(e.id) > 0"
	}
	rows, err := s.db.Query(ctx, fmt.Sprintf(`SELECT b.id, b.type, COALESCE(b.data->>'title', b.data->>'name', b.data->>'code', b.data->>'label', ''),
			count(e.id), b.is_visible, b.deleted_at IS NOT NULL, b.created_at
		FROM blocks b LEFT JOIN events e ON e.block_id = b.id AND e.kind = 'click' AND %s
		WHERE b.profile_id = $1 GROUP BY b.id %s ORDER BY 4 DESC, b.created_at LIMIT $%d`, where, having, len(args)), args...)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (blockStat, error) {
		b := blockStat{Actions: map[string]int{}}
		err := r.Scan(&b.BlockID, &b.Type, &b.Title, &b.Clicks, &b.IsVisible, &b.Deleted, &b.CreatedAt)
		return b, err
	})
	if err != nil || len(items) == 0 {
		return items, err
	}
	byID := make(map[string]*blockStat, len(items))
	for i := range items {
		byID[items[i].BlockID] = &items[i]
	}
	where, args = aq.base(pid, aq.from, aq.to)
	arows, err := s.db.Query(ctx, fmt.Sprintf(`SELECT e.block_id, e.action, count(*) FROM events e
		WHERE e.kind = 'click' AND e.action <> '' AND %s GROUP BY 1, 2`, where), args...)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var id, action string
		var n int
		if err := arows.Scan(&id, &action, &n); err != nil {
			return nil, err
		}
		if b := byID[id]; b != nil {
			b.Actions[action] = n
		}
	}
	return items, arows.Err()
}

func (s *Server) handleAnalyticsBlocks(w http.ResponseWriter, r *http.Request) {
	aq, err := parseAnalyticsQuery(r, s.now())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	items, err := s.analyticsBlocks(r.Context(), aq, profileID(r), 1000, false)
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"range": aq.rangeJSON(), "items": items})
}

func (s *Server) handleAnalyticsBlockSeries(w http.ResponseWriter, r *http.Request) {
	aq, err := parseAnalyticsQuery(r, s.now())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	ctx, pid, id := r.Context(), profileID(r), chi.URLParam(r, "id")
	var exists bool
	err = s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blocks WHERE id = $1 AND profile_id = $2)`, id, pid).Scan(&exists)
	if isInvalidUUID(err) || (err == nil && !exists) {
		writeError(w, http.StatusNotFound, "not_found", "block not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	pred, extra := "e.block_id = $%d", []any{id}
	if a := r.URL.Query().Get("action"); a != "" {
		pred += " AND e.action = $%d"
		extra = append(extra, a)
	}
	series, err := s.analyticsSeries(ctx, aq, pid, pred, extra...)
	if err != nil {
		s.internalError(w, err)
		return
	}
	type pt struct {
		T      string `json:"t"`
		Clicks int    `json:"clicks"`
	}
	out := make([]pt, len(series))
	total := 0
	for i, p := range series {
		out[i] = pt{p.T, p.Clicks}
		total += p.Clicks
	}
	writeJSON(w, http.StatusOK, map[string]any{"range": aq.rangeJSON(), "block_id": id, "clicks": total, "series": out})
}

// handleAnalyticsExport downloads CSV: type=series (default) | breakdown (needs dimension) | blocks.
func (s *Server) handleAnalyticsExport(w http.ResponseWriter, r *http.Request) {
	aq, err := parseAnalyticsQuery(r, s.now())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	ctx, pid := r.Context(), profileID(r)
	itoa := strconv.Itoa
	switch typ := r.URL.Query().Get("type"); typ {
	case "", "series":
		series, err := s.analyticsSeries(ctx, aq, pid, "")
		if err != nil {
			s.internalError(w, err)
			return
		}
		rows := make([][]string, len(series))
		for i, p := range series {
			rows[i] = []string{p.T, itoa(p.Views), itoa(p.Clicks), itoa(p.Visitors)}
		}
		writeCSV(w, "analytics-series.csv", []string{"Bucket (" + aq.interval + ", " + aq.loc.String() + ")", "Views", "Clicks", "Visitors"}, rows)
	case "breakdown":
		dim, _, err := breakdownParams(r)
		if err != nil {
			s.writeErr(w, err)
			return
		}
		items, err := s.analyticsBreakdown(ctx, aq, pid, dim, 1000)
		if err != nil {
			s.internalError(w, err)
			return
		}
		rows := make([][]string, len(items))
		for i, b := range items {
			rows[i] = []string{b.Value, itoa(b.Views), itoa(b.Clicks), itoa(b.Visitors)}
		}
		writeCSV(w, "analytics-"+dim+".csv", []string{dim, "Views", "Clicks", "Visitors"}, rows)
	case "blocks":
		items, err := s.analyticsBlocks(ctx, aq, pid, 1000, false)
		if err != nil {
			s.internalError(w, err)
			return
		}
		rows := make([][]string, len(items))
		for i, b := range items {
			rows[i] = []string{b.BlockID, b.Type, b.Title, itoa(b.Clicks), strconv.FormatBool(b.IsVisible), strconv.FormatBool(b.Deleted), ts(b.CreatedAt)}
		}
		writeCSV(w, "analytics-blocks.csv", []string{"Block ID", "Type", "Title", "Clicks", "Visible", "Deleted", "Added"}, rows)
	default:
		s.writeErr(w, badReq("invalid_type", "type must be series, breakdown or blocks"))
	}
}
