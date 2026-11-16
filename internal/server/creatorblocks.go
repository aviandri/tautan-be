package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Phase 3-B "Creators and influencers" cards that need no remote fetching.
// See "P3-B: implemented (BE)" in SPEC.md.

type (
	MediaKitPlatform struct {
		Platform   string   `json:"platform"`
		Handle     string   `json:"handle"`
		Followers  int64    `json:"followers"`
		AvgViews   *int64   `json:"avg_views,omitempty"`
		Engagement *float64 `json:"engagement,omitempty"` // percent, 0-100
		URL        string   `json:"url,omitempty"`
	}
	AudienceSlice struct {
		Label string  `json:"label"`
		Pct   float64 `json:"pct"` // 0-100
	}
	MediaKitAudience struct {
		Age    []AudienceSlice `json:"age"`
		Gender []AudienceSlice `json:"gender"`
		Cities []AudienceSlice `json:"cities"`
	}
	MediaKitData struct {
		Title     string             `json:"title,omitempty"`
		Platforms []MediaKitPlatform `json:"platforms"`
		Audience  *MediaKitAudience  `json:"audience,omitempty"`
		UpdatedAt string             `json:"updated_at"` // YYYY-MM-DD
		PDFURL    string             `json:"pdf_url,omitempty"`
		Schedule  *Schedule          `json:"schedule,omitempty"`
	}
	RatePackage struct {
		Name         string   `json:"name"`
		PriceFrom    *int64   `json:"price_from,omitempty"`
		Deliverables []string `json:"deliverables"`
		Turnaround   string   `json:"turnaround,omitempty"`
	}
	RateCardData struct {
		Title       string        `json:"title,omitempty"`
		Packages    []RatePackage `json:"packages"`
		Currency    string        `json:"currency"`
		Note        string        `json:"note,omitempty"`
		FormBlockID string        `json:"form_block_id,omitempty"` // a form block on the same page
		ButtonLabel string        `json:"button_label,omitempty"`
		Schedule    *Schedule     `json:"schedule,omitempty"`
	}
	BrandLogo struct {
		ImageURL string `json:"image_url"`
		Name     string `json:"name"`
		URL      string `json:"url,omitempty"`
	}
	BrandsData struct {
		Title    string      `json:"title,omitempty"`
		Logos    []BrandLogo `json:"logos"`
		Schedule *Schedule   `json:"schedule,omitempty"`
	}
	LookSpot struct {
		X     float64 `json:"x"` // percent of image width, 0-100
		Y     float64 `json:"y"` // percent of image height, 0-100
		Title string  `json:"title"`
		Price *int64  `json:"price,omitempty"`
		URL   string  `json:"url"`
		Code  string  `json:"code,omitempty"`
	}
	ShopTheLookData struct {
		ImageURL string     `json:"image_url"`
		Alt      string     `json:"alt,omitempty"`
		Currency string     `json:"currency"`
		Spots    []LookSpot `json:"spots"`
		Schedule *Schedule  `json:"schedule,omitempty"`
	}
	GalleryImage struct {
		URL     string `json:"url"`
		Alt     string `json:"alt,omitempty"`
		LinkURL string `json:"link_url,omitempty"`
	}
	GalleryData struct {
		Title    string         `json:"title,omitempty"`
		Images   []GalleryImage `json:"images"`
		Layout   string         `json:"layout"` // carousel | grid
		Schedule *Schedule      `json:"schedule,omitempty"`
	}
	CommunityData struct {
		Platform    string    `json:"platform"`
		URL         string    `json:"url"`
		Title       string    `json:"title"`
		MembersNote string    `json:"members_note,omitempty"`
		ButtonLabel string    `json:"button_label,omitempty"`
		Schedule    *Schedule `json:"schedule,omitempty"`
	}
	EventDate struct {
		Date   string `json:"date"`           // YYYY-MM-DD in tz
		Time   string `json:"time,omitempty"` // HH:MM
		Title  string `json:"title,omitempty"`
		City   string `json:"city,omitempty"`
		Venue  string `json:"venue,omitempty"`
		URL    string `json:"url,omitempty"`
		Status string `json:"status,omitempty"` // sold_out | few_left | cancelled
	}
	WeeklySlot struct {
		Days        []string `json:"days"`  // mon..sun
		Start       string   `json:"start"` // HH:MM in tz
		DurationMin int      `json:"duration_min"`
		Title       string   `json:"title,omitempty"`
		Platform    string   `json:"platform,omitempty"`
		URL         string   `json:"url,omitempty"`
	}
	EventsData struct {
		Title    string       `json:"title,omitempty"`
		Mode     string       `json:"mode"` // dates | weekly
		TZ       string       `json:"tz"`
		Dates    []EventDate  `json:"dates,omitempty"`
		Weekly   []WeeklySlot `json:"weekly,omitempty"`
		Schedule *Schedule    `json:"schedule,omitempty"`
	}
)

var (
	creatorTypes = []string{"media_kit", "rate_card", "brands", "shop_the_look", "gallery", "community", "events"}

	creatorPlatforms   = set("instagram", "tiktok", "youtube", "x", "facebook", "threads", "twitch", "linkedin", "spotify", "podcast", "blog")
	livePlatforms      = set("tiktok", "instagram", "youtube", "twitch", "facebook", "shopee", "other")
	galleryLayouts     = set("carousel", "grid")
	eventStatuses      = set("sold_out", "few_left", "cancelled")
	communityPlatforms = map[string][]string{ // allowed hosts per platform
		"whatsapp_group":   {"chat.whatsapp.com"},
		"whatsapp_channel": {"whatsapp.com"}, // whatsapp.com/channel/…
		"telegram":         {"t.me", "telegram.me"},
		"discord":          {"discord.gg", "discord.com"},
		"line":             {"line.me"},
	}
	weekdayIndex = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
		"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}
)

func init() {
	for _, t := range creatorTypes {
		blockTypes[t] = true
	}
}

func checkPct(field string, v float64) error {
	if math.IsNaN(v) || v < 0 || v > 100 {
		return ferr(field, "%s must be between 0 and 100", field)
	}
	return nil
}

func checkDate(field, v string, required bool) error {
	if v == "" && !required {
		return nil
	}
	if _, err := time.Parse("2006-01-02", v); err != nil {
		return ferr(field, "%s must be a date as YYYY-MM-DD", field)
	}
	return nil
}

func checkTZ(tz string) error {
	if tz == "" || strings.EqualFold(tz, "local") {
		return ferr("tz", "tz is required (an IANA time zone such as Asia/Jakarta)")
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return ferr("tz", "tz must be an IANA time zone such as Asia/Jakarta")
	}
	return nil
}

func checkSlices(field string, xs []AudienceSlice, max int) error {
	if err := checkCount(field, len(xs), 0, max); err != nil {
		return err
	}
	sum := 0.0
	for i, x := range xs {
		f := fmt.Sprintf("%s[%d]", field, i)
		if err := firstErr(checkText(f+".label", x.Label, 40), checkPct(f+".pct", x.Pct)); err != nil {
			return err
		}
		sum += x.Pct
	}
	if sum > 100.5 {
		return ferr(field, "%s percentages add up to more than 100", field)
	}
	return nil
}

func checkMediaKit(d *MediaKitData) error {
	if d.Platforms == nil {
		d.Platforms = make([]MediaKitPlatform, 0)
	}
	if err := firstErr(maxLen("title", d.Title, 60), checkCount("platforms", len(d.Platforms), 1, 8),
		checkDate("updated_at", d.UpdatedAt, true), checkImageURL("pdf_url", d.PDFURL, false)); err != nil {
		return err
	}
	for i, p := range d.Platforms {
		f := fmt.Sprintf("platforms[%d]", i)
		if !creatorPlatforms[p.Platform] {
			return ferr(f+".platform", "%s.platform has an unsupported value %q", f, p.Platform)
		}
		if err := firstErr(checkText(f+".handle", p.Handle, 60), checkWebURL(f+".url", p.URL, false)); err != nil {
			return err
		}
		if p.Followers < 0 || (p.AvgViews != nil && *p.AvgViews < 0) {
			return ferr(f+".followers", "%s counts must not be negative", f)
		}
		if p.Engagement != nil {
			if err := checkPct(f+".engagement", *p.Engagement); err != nil {
				return err
			}
		}
	}
	if a := d.Audience; a != nil {
		for _, x := range []*[]AudienceSlice{&a.Age, &a.Gender, &a.Cities} {
			if *x == nil {
				*x = make([]AudienceSlice, 0)
			}
		}
		return firstErr(checkSlices("audience.age", a.Age, 8), checkSlices("audience.gender", a.Gender, 4),
			checkSlices("audience.cities", a.Cities, 10))
	}
	return nil
}

func checkRateCard(d *RateCardData) error {
	if d.Packages == nil {
		d.Packages = make([]RatePackage, 0)
	}
	if err := firstErr(maxLen("title", d.Title, 60), checkCount("packages", len(d.Packages), 1, 12),
		normCurrency("currency", &d.Currency), maxLen("note", d.Note, 300), maxLen("button_label", d.ButtonLabel, 40)); err != nil {
		return err
	}
	if d.FormBlockID != "" && !uuidRE.MatchString(d.FormBlockID) {
		return ferr("form_block_id", "form_block_id must be a form block id")
	}
	for i := range d.Packages {
		p := &d.Packages[i]
		f := fmt.Sprintf("packages[%d]", i)
		if p.Deliverables == nil {
			p.Deliverables = make([]string, 0)
		}
		if err := firstErr(checkText(f+".name", p.Name, 80), checkMoney(f+".price_from", p.PriceFrom),
			checkCount(f+".deliverables", len(p.Deliverables), 1, 10), maxLen(f+".turnaround", p.Turnaround, 40)); err != nil {
			return err
		}
		for j, dl := range p.Deliverables {
			if err := checkText(fmt.Sprintf("%s.deliverables[%d]", f, j), dl, 100); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkBrands(d *BrandsData) error {
	if d.Logos == nil {
		d.Logos = make([]BrandLogo, 0)
	}
	if err := firstErr(maxLen("title", d.Title, 60), checkCount("logos", len(d.Logos), 1, 30)); err != nil {
		return err
	}
	for i, l := range d.Logos {
		f := fmt.Sprintf("logos[%d]", i)
		if err := firstErr(checkImageURL(f+".image_url", l.ImageURL, true), checkText(f+".name", l.Name, 60),
			checkWebURL(f+".url", l.URL, false)); err != nil {
			return err
		}
	}
	return nil
}

func checkShopTheLook(d *ShopTheLookData) error {
	if d.Spots == nil {
		d.Spots = make([]LookSpot, 0)
	}
	if err := firstErr(checkImageURL("image_url", d.ImageURL, true), maxLen("alt", d.Alt, 200),
		normCurrency("currency", &d.Currency), checkCount("spots", len(d.Spots), 1, 12)); err != nil {
		return err
	}
	for i, sp := range d.Spots {
		f := fmt.Sprintf("spots[%d]", i)
		if err := firstErr(checkPct(f+".x", sp.X), checkPct(f+".y", sp.Y), checkText(f+".title", sp.Title, 80),
			checkMoney(f+".price", sp.Price), checkWebURL(f+".url", sp.URL, true), maxLen(f+".code", sp.Code, 40)); err != nil {
			return err
		}
	}
	return nil
}

func checkGallery(d *GalleryData) error {
	if d.Images == nil {
		d.Images = make([]GalleryImage, 0)
	}
	if d.Layout == "" {
		d.Layout = "grid"
	}
	if err := firstErr(maxLen("title", d.Title, 60), oneOf("layout", d.Layout, galleryLayouts),
		checkCount("images", len(d.Images), 1, 30)); err != nil {
		return err
	}
	for i, im := range d.Images {
		f := fmt.Sprintf("images[%d]", i)
		if err := firstErr(checkImageURL(f+".url", im.URL, true), maxLen(f+".alt", im.Alt, 200),
			checkWebURL(f+".link_url", im.LinkURL, false)); err != nil {
			return err
		}
	}
	return nil
}

func checkCommunity(d *CommunityData) error {
	hosts, ok := communityPlatforms[d.Platform]
	if !ok {
		return ferr("platform", "platform must be whatsapp_group, whatsapp_channel, telegram, discord or line")
	}
	if err := firstErr(checkWebURL("url", d.URL, true), checkText("title", d.Title, 80),
		maxLen("members_note", d.MembersNote, 60), maxLen("button_label", d.ButtonLabel, 40)); err != nil {
		return err
	}
	u, _ := url.Parse(strings.TrimSpace(d.URL))
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	match := false
	for _, h := range hosts {
		match = match || host == h
	}
	if !match || u.Scheme != "https" || (d.Platform == "whatsapp_channel" && !strings.HasPrefix(u.Path, "/channel/")) {
		return ferr("url", "url must be an https %s link (%s)", d.Platform, strings.Join(hosts, ", "))
	}
	return nil
}

func checkEvents(d *EventsData) error {
	if err := firstErr(maxLen("title", d.Title, 60), checkTZ(d.TZ)); err != nil {
		return err
	}
	switch d.Mode {
	case "dates":
		d.Weekly = nil
		if d.Dates == nil {
			d.Dates = make([]EventDate, 0)
		}
		if err := checkCount("dates", len(d.Dates), 1, 50); err != nil {
			return err
		}
		for i, e := range d.Dates {
			f := fmt.Sprintf("dates[%d]", i)
			if e.Time != "" && (!hhmmRE.MatchString(e.Time) || e.Time == "24:00") {
				return ferr(f+".time", "%s.time must be HH:MM", f)
			}
			if err := firstErr(checkDate(f+".date", e.Date, true), maxLen(f+".title", e.Title, 80),
				maxLen(f+".city", e.City, 60), maxLen(f+".venue", e.Venue, 100),
				checkWebURL(f+".url", e.URL, false), oneOf(f+".status", e.Status, eventStatuses)); err != nil {
				return err
			}
		}
	case "weekly":
		d.Dates = nil
		if d.Weekly == nil {
			d.Weekly = make([]WeeklySlot, 0)
		}
		if err := checkCount("weekly", len(d.Weekly), 1, 20); err != nil {
			return err
		}
		for i := range d.Weekly {
			sl := &d.Weekly[i]
			f := fmt.Sprintf("weekly[%d]", i)
			if sl.Days == nil {
				sl.Days = make([]string, 0)
			}
			if err := checkCount(f+".days", len(sl.Days), 1, 7); err != nil {
				return err
			}
			seen := map[string]bool{}
			for j, dd := range sl.Days {
				if !weekDays[dd] || seen[dd] {
					return ferr(fmt.Sprintf("%s.days[%d]", f, j), "%s.days must be unique days mon..sun", f)
				}
				seen[dd] = true
			}
			if !hhmmRE.MatchString(sl.Start) || sl.Start == "24:00" {
				return ferr(f+".start", "%s.start must be HH:MM", f)
			}
			if sl.DurationMin < 5 || sl.DurationMin > 720 {
				return ferr(f+".duration_min", "%s.duration_min must be 5 to 720", f)
			}
			if err := firstErr(maxLen(f+".title", sl.Title, 80), oneOf(f+".platform", sl.Platform, livePlatforms),
				checkWebURL(f+".url", sl.URL, false)); err != nil {
				return err
			}
		}
	default:
		return ferr("mode", "mode must be dates or weekly")
	}
	return nil
}

func creatorBlock(typ string) (v any, check func() error, sched func() *Schedule, ok bool) {
	switch typ {
	case "media_kit":
		d := &MediaKitData{}
		return d, func() error { return checkMediaKit(d) }, func() *Schedule { return d.Schedule }, true
	case "rate_card":
		d := &RateCardData{}
		return d, func() error { return checkRateCard(d) }, func() *Schedule { return d.Schedule }, true
	case "brands":
		d := &BrandsData{}
		return d, func() error { return checkBrands(d) }, func() *Schedule { return d.Schedule }, true
	case "shop_the_look":
		d := &ShopTheLookData{}
		return d, func() error { return checkShopTheLook(d) }, func() *Schedule { return d.Schedule }, true
	case "gallery":
		d := &GalleryData{}
		return d, func() error { return checkGallery(d) }, func() *Schedule { return d.Schedule }, true
	case "community":
		d := &CommunityData{}
		return d, func() error { return checkCommunity(d) }, func() *Schedule { return d.Schedule }, true
	case "events":
		d := &EventsData{}
		return d, func() error { return checkEvents(d) }, func() *Schedule { return d.Schedule }, true
	}
	return nil, nil, nil, false
}

// queryRower is satisfied by pgx.Tx and *pgxpool.Pool.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// checkBlockRefs verifies that block ids referenced from data point at a form
// block on the same page (rate_card.form_block_id, payment.confirm form target).
func checkBlockRefs(ctx context.Context, q queryRower, pid, typ string, data json.RawMessage) error {
	var field, id string
	switch typ {
	case "rate_card":
		var d RateCardData
		_ = json.Unmarshal(data, &d)
		field, id = "form_block_id", d.FormBlockID
	case "payment":
		var d PaymentData
		_ = json.Unmarshal(data, &d)
		if d.Confirm != nil && d.Confirm.Kind == "form" {
			field, id = "confirm.target", d.Confirm.Target
		}
	}
	if id == "" {
		return nil
	}
	var ok bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blocks WHERE id = $1 AND profile_id = $2
		AND type IN ('form', 'contact_form', 'email_signup') AND deleted_at IS NULL)`, id, pid).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return dataErr(ferr(field, "%s must be a form block on this page", field))
	}
	return nil
}

// EventsComputed is added to events blocks in public reads as data.computed.
// It is correct at computed_at; the FE can tick it forward on the client from
// the timestamps without redoing the schedule maths.
type EventsComputed struct {
	ComputedAt time.Time   `json:"computed_at"`
	Live       *SlotWindow `json:"live"`            // weekly: the slot on air now
	Next       *SlotWindow `json:"next"`            // weekly: the next slot to start
	PastHidden int         `json:"past_hidden"`     // dates: past dates dropped from the list
	TodayLocal string      `json:"today,omitempty"` // dates: today's date in tz
}

type SlotWindow struct {
	Index    int       `json:"index"` // into weekly[]
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
}

// computeEvents drops past dates (dates mode) and works out the live and next
// slots (weekly mode) at now, in the card's time zone.
func computeEvents(d *EventsData, now time.Time) EventsComputed {
	out := EventsComputed{ComputedAt: now.UTC().Truncate(time.Second)}
	loc, err := time.LoadLocation(d.TZ)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	switch d.Mode {
	case "dates":
		today := local.Format("2006-01-02")
		out.TodayLocal = today
		kept := make([]EventDate, 0, len(d.Dates))
		for _, e := range d.Dates {
			if e.Date >= today {
				kept = append(kept, e)
			} else {
				out.PastHidden++
			}
		}
		d.Dates = kept
	case "weekly":
		// Look from 12 h back (the longest slot) to 8 days ahead.
		midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		for i, sl := range d.Weekly {
			h, _ := time.Parse("15:04", sl.Start)
			dur := time.Duration(sl.DurationMin) * time.Minute
			for off := -1; off <= 7; off++ {
				day := midnight.AddDate(0, 0, off)
				if !containsDay(sl.Days, day.Weekday()) {
					continue
				}
				start := time.Date(day.Year(), day.Month(), day.Day(), h.Hour(), h.Minute(), 0, 0, loc)
				end := start.Add(dur)
				w := &SlotWindow{Index: i, StartsAt: start.UTC(), EndsAt: end.UTC()}
				switch {
				case !now.Before(start) && now.Before(end):
					if out.Live == nil || start.Before(out.Live.StartsAt) {
						out.Live = w
					}
				case start.After(now):
					if out.Next == nil || start.Before(out.Next.StartsAt) {
						out.Next = w
					}
				}
			}
		}
	}
	return out
}

func containsDay(days []string, wd time.Weekday) bool {
	for _, d := range days {
		if weekdayIndex[d] == wd {
			return true
		}
	}
	return false
}

// publicBlockData rewrites data for public reads where the server computes
// state (events: live now / next slot, past dates hidden).
func publicBlockData(b *Block, now time.Time) {
	if b.Type != "events" {
		return
	}
	var d EventsData
	if json.Unmarshal(b.Data, &d) != nil {
		return
	}
	c := computeEvents(&d, now)
	out, err := json.Marshal(struct {
		EventsData
		Computed EventsComputed `json:"computed"`
	}{d, c})
	if err == nil {
		b.Data = out
	}
}
