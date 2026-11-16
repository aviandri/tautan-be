package server

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Phase 3-D "Text and layout" cards. See "P3-C/D: implemented (BE)" in SPEC.md.

type (
	SpotlightButton struct {
		Label string `json:"label"`
		URL   string `json:"url"`
	}
	SpotlightData struct {
		ImageURL string           `json:"image_url,omitempty"`
		Eyebrow  string           `json:"eyebrow,omitempty"`
		Headline string           `json:"headline"`
		Text     string           `json:"text,omitempty"`
		Button   *SpotlightButton `json:"button,omitempty"`
		Align    string           `json:"align"` // left | center
		Schedule *Schedule        `json:"schedule,omitempty"`
	}
	MarqueeData struct {
		Text      string    `json:"text"`
		Separator string    `json:"separator"`
		Speed     string    `json:"speed"`     // slow | normal | fast
		Direction string    `json:"direction"` // left | right
		Style     string    `json:"style"`     // ribbon | plain
		Schedule  *Schedule `json:"schedule,omitempty"`
	}
	TimelineItem struct {
		Eyebrow     string `json:"eyebrow,omitempty"`
		Title       string `json:"title"`
		Description string `json:"description,omitempty"`
		Badge       string `json:"badge,omitempty"`
		Highlight   bool   `json:"highlight,omitempty"`
	}
	TimelineData struct {
		Title    string         `json:"title,omitempty"`
		Style    string         `json:"style"` // numbered | dotted
		Items    []TimelineItem `json:"items"`
		Schedule *Schedule      `json:"schedule,omitempty"`
	}
	ResumeItem struct {
		Role        string `json:"role"`
		Company     string `json:"company"`
		CompanyURL  string `json:"company_url,omitempty"`
		Start       string `json:"start"`         // YYYY-MM
		End         string `json:"end,omitempty"` // YYYY-MM; empty with current
		Current     bool   `json:"current,omitempty"`
		Description string `json:"description,omitempty"`
	}
	ResumeData struct {
		Title    string       `json:"title,omitempty"`
		Items    []ResumeItem `json:"items"`
		Schedule *Schedule    `json:"schedule,omitempty"`
	}
	BeforeAfterData struct {
		BeforeURL   string    `json:"before_url"`
		AfterURL    string    `json:"after_url"`
		BeforeLabel string    `json:"before_label,omitempty"`
		AfterLabel  string    `json:"after_label,omitempty"`
		Alt         string    `json:"alt,omitempty"`
		StartPct    *float64  `json:"start_pct,omitempty"` // initial slider position, default 50
		Schedule    *Schedule `json:"schedule,omitempty"`
	}
	MapLinks struct {
		Google string `json:"google"`
		Apple  string `json:"apple"`
		OSM    string `json:"osm"`
	}
	MapData struct {
		Label    string    `json:"label,omitempty"`
		Address  string    `json:"address,omitempty"`
		Area     string    `json:"area,omitempty"`
		Lat      float64   `json:"lat"`
		Lng      float64   `json:"lng"`
		Zoom     int       `json:"zoom"`  // 3-19, default 15
		Links    MapLinks  `json:"links"` // set by the server
		Schedule *Schedule `json:"schedule,omitempty"`
	}
)

var (
	layoutTypes = []string{"spotlight", "marquee", "timeline", "resume", "before_after", "map"}

	spotlightAligns = set("left", "center")
	marqueeSpeeds   = set("slow", "normal", "fast")
	marqueeDirs     = set("left", "right")
	marqueeStyles   = set("ribbon", "plain")
	timelineStyles  = set("numbered", "dotted")
	textStyles      = set("card", "sticky_note", "speech_bubble", "torn_paper", "chalkboard")
	socialVariants  = set("icons", "donate")
)

func init() {
	for _, t := range layoutTypes {
		blockTypes[t] = true
	}
}

func orDefault(v *string, def string) {
	if *v == "" {
		*v = def
	}
}

func checkSpotlight(d *SpotlightData) error {
	orDefault(&d.Align, "left")
	if err := firstErr(checkImageURL("image_url", d.ImageURL, false), maxLen("eyebrow", d.Eyebrow, 40),
		checkText("headline", d.Headline, 100), maxLen("text", d.Text, 300), oneOf("align", d.Align, spotlightAligns)); err != nil {
		return err
	}
	if b := d.Button; b != nil {
		return firstErr(checkText("button.label", b.Label, 40), checkWebURL("button.url", b.URL, true))
	}
	return nil
}

func checkMarquee(d *MarqueeData) error {
	orDefault(&d.Separator, "•")
	orDefault(&d.Speed, "normal")
	orDefault(&d.Direction, "left")
	orDefault(&d.Style, "ribbon")
	return firstErr(checkText("text", d.Text, 200), maxLen("separator", d.Separator, 8),
		oneOf("speed", d.Speed, marqueeSpeeds), oneOf("direction", d.Direction, marqueeDirs), oneOf("style", d.Style, marqueeStyles))
}

func checkTimeline(d *TimelineData) error {
	if d.Items == nil {
		d.Items = make([]TimelineItem, 0)
	}
	orDefault(&d.Style, "numbered")
	if err := firstErr(maxLen("title", d.Title, 60), oneOf("style", d.Style, timelineStyles), checkCount("items", len(d.Items), 1, 20)); err != nil {
		return err
	}
	highlighted := 0
	for i, it := range d.Items {
		f := fmt.Sprintf("items[%d]", i)
		if err := firstErr(maxLen(f+".eyebrow", it.Eyebrow, 40), checkText(f+".title", it.Title, 100),
			maxLen(f+".description", it.Description, 300), maxLen(f+".badge", it.Badge, 24)); err != nil {
			return err
		}
		if it.Highlight {
			if highlighted++; highlighted > 1 {
				return ferr(f+".highlight", "only one timeline item can be highlighted")
			}
		}
	}
	return nil
}

func checkMonth(field, v string, required bool) error {
	if v == "" && !required {
		return nil
	}
	if _, err := time.Parse("2006-01", v); err != nil {
		return ferr(field, "%s must be a month as YYYY-MM", field)
	}
	return nil
}

func checkResume(d *ResumeData) error {
	if d.Items == nil {
		d.Items = make([]ResumeItem, 0)
	}
	if err := firstErr(maxLen("title", d.Title, 60), checkCount("items", len(d.Items), 1, 30)); err != nil {
		return err
	}
	for i := range d.Items {
		it := &d.Items[i]
		f := fmt.Sprintf("items[%d]", i)
		if it.Current {
			it.End = ""
		}
		if err := firstErr(checkText(f+".role", it.Role, 80), checkText(f+".company", it.Company, 80),
			checkWebURL(f+".company_url", it.CompanyURL, false), checkMonth(f+".start", it.Start, true),
			checkMonth(f+".end", it.End, false), maxLen(f+".description", it.Description, 500)); err != nil {
			return err
		}
		if it.End != "" && it.End < it.Start { // YYYY-MM compares lexically
			return ferr(f+".end", "%s.end must not be before start", f)
		}
	}
	return nil
}

func checkBeforeAfter(d *BeforeAfterData) error {
	if err := firstErr(checkImageURL("before_url", d.BeforeURL, true), checkImageURL("after_url", d.AfterURL, true),
		maxLen("before_label", d.BeforeLabel, 20), maxLen("after_label", d.AfterLabel, 20), maxLen("alt", d.Alt, 200)); err != nil {
		return err
	}
	if d.StartPct != nil {
		return checkPct("start_pct", *d.StartPct)
	}
	return nil
}

func checkMap(d *MapData) error {
	if d.Zoom == 0 {
		d.Zoom = 15
	}
	if err := firstErr(maxLen("label", d.Label, 80), maxLen("address", d.Address, 200), maxLen("area", d.Area, 100)); err != nil {
		return err
	}
	if math.IsNaN(d.Lat) || d.Lat < -90 || d.Lat > 90 {
		return ferr("lat", "lat must be between -90 and 90")
	}
	if math.IsNaN(d.Lng) || d.Lng < -180 || d.Lng > 180 {
		return ferr("lng", "lng must be between -180 and 180")
	}
	if d.Lat == 0 && d.Lng == 0 {
		return ferr("lat", "lat and lng are required")
	}
	if d.Zoom < 3 || d.Zoom > 19 {
		return ferr("zoom", "zoom must be 3 to 19")
	}
	// Round to ~1 m and build the directions links ourselves (never trusted from input).
	d.Lat, d.Lng = math.Round(d.Lat*1e5)/1e5, math.Round(d.Lng*1e5)/1e5
	ll := strconv.FormatFloat(d.Lat, 'f', -1, 64) + "," + strconv.FormatFloat(d.Lng, 'f', -1, 64)
	d.Links = MapLinks{
		Google: "https://www.google.com/maps/dir/?api=1&destination=" + ll,
		Apple:  "https://maps.apple.com/?daddr=" + ll,
		OSM: fmt.Sprintf("https://www.openstreetmap.org/?mlat=%s&mlon=%s#map=%d/%s/%s",
			strconv.FormatFloat(d.Lat, 'f', -1, 64), strconv.FormatFloat(d.Lng, 'f', -1, 64), d.Zoom,
			strconv.FormatFloat(d.Lat, 'f', -1, 64), strconv.FormatFloat(d.Lng, 'f', -1, 64)),
	}
	return nil
}

func layoutBlock(typ string) (v any, check func() error, sched func() *Schedule, ok bool) {
	switch typ {
	case "spotlight":
		d := &SpotlightData{}
		return d, func() error { return checkSpotlight(d) }, func() *Schedule { return d.Schedule }, true
	case "marquee":
		d := &MarqueeData{}
		return d, func() error { return checkMarquee(d) }, func() *Schedule { return d.Schedule }, true
	case "timeline":
		d := &TimelineData{}
		return d, func() error { return checkTimeline(d) }, func() *Schedule { return d.Schedule }, true
	case "resume":
		d := &ResumeData{}
		return d, func() error { return checkResume(d) }, func() *Schedule { return d.Schedule }, true
	case "before_after":
		d := &BeforeAfterData{}
		return d, func() error { return checkBeforeAfter(d) }, func() *Schedule { return d.Schedule }, true
	case "map":
		d := &MapData{}
		return d, func() error { return checkMap(d) }, func() *Schedule { return d.Schedule }, true
	}
	return nil, nil, nil, false
}

// ---- P3-D additions to existing types ----

// DiscountCode is one entry of discount_code.codes (several codes, swiped).
type DiscountCode struct {
	Code        string     `json:"code"`
	Description string     `json:"description,omitempty"`
	Discount    string     `json:"discount,omitempty"`
	URL         string     `json:"url,omitempty"`
	EndsAt      *time.Time `json:"ends_at,omitempty"`
}

func checkDiscountCodes(codes []DiscountCode) error {
	if err := checkCount("codes", len(codes), 0, 10); err != nil {
		return err
	}
	for i, c := range codes {
		f := fmt.Sprintf("codes[%d]", i)
		if err := firstErr(checkText(f+".code", c.Code, 40), maxLen(f+".description", c.Description, 300),
			maxLen(f+".discount", c.Discount, 24), checkURL(f+".url", c.URL, false)); err != nil {
			return err
		}
	}
	return nil
}

func checkSocialVariant(d *SocialIconsData) error {
	if err := oneOf("variant", d.Variant, socialVariants); err != nil {
		return err
	}
	if d.Variant == "donate" {
		for i, it := range d.Items {
			if !tipPlatforms[it.Platform] {
				f := fmt.Sprintf("items[%d].platform", i)
				return ferr(f, "%s must be saweria, trakteer, kofi, paypal or buymeacoffee for the donate variant", f)
			}
		}
	}
	return nil
}
