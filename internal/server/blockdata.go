package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Block data shapes; keep in sync with SPEC.md and tautan-fe/src/lib/types.ts.
// Every field added after MVP is optional (omitempty) so older data stays valid.

// Schedule limits when a block is shown publicly. Either bound may be omitted.
type Schedule struct {
	StartAt *time.Time `json:"start_at,omitempty"`
	EndAt   *time.Time `json:"end_at,omitempty"`
}

// activeAt reports whether a block with this schedule is visible at t.
func (s *Schedule) activeAt(t time.Time) bool {
	if s == nil {
		return true
	}
	if s.StartAt != nil && t.Before(*s.StartAt) {
		return false
	}
	if s.EndAt != nil && !t.Before(*s.EndAt) {
		return false
	}
	return true
}

type (
	LinkData struct {
		Title        string    `json:"title"`
		URL          string    `json:"url"`
		ThumbnailURL string    `json:"thumbnail_url,omitempty"`
		Description  string    `json:"description,omitempty"`
		Layout       string    `json:"layout,omitempty"`
		Icon         string    `json:"icon,omitempty"`
		Label        string    `json:"label,omitempty"`
		TextAlign    string    `json:"text_align,omitempty"`
		DestKind     string    `json:"dest_kind,omitempty"`
		TabID        string    `json:"tab_id,omitempty"` // dest_kind = tab
		Schedule     *Schedule `json:"schedule,omitempty"`
	}
	TextData struct {
		Content  string    `json:"content"`
		Schedule *Schedule `json:"schedule,omitempty"`
	}
	ImageData struct {
		URL       string    `json:"url"`
		Alt       string    `json:"alt,omitempty"`
		LinkURL   string    `json:"link_url,omitempty"`
		FullBleed bool      `json:"full_bleed,omitempty"`
		Lightbox  bool      `json:"lightbox,omitempty"`
		Schedule  *Schedule `json:"schedule,omitempty"`
	}
	EmbedData struct { // video, music
		URL      string    `json:"url"`
		Schedule *Schedule `json:"schedule,omitempty"`
	}
	SocialIcon struct {
		Platform string `json:"platform"`
		URL      string `json:"url"`
	}
	SocialIconsData struct {
		Items    []SocialIcon `json:"items"`
		Schedule *Schedule    `json:"schedule,omitempty"`
	}
	FolderData struct {
		Title    string    `json:"title"`
		Style    string    `json:"style,omitempty"` // collapsible | inline
		Schedule *Schedule `json:"schedule,omitempty"`
	}
	FaqItem struct {
		Q string `json:"q"`
		A string `json:"a"`
	}
	FaqData struct {
		Heading     string    `json:"heading,omitempty"`
		Description string    `json:"description,omitempty"`
		Items       []FaqItem `json:"items"`
		Schedule    *Schedule `json:"schedule,omitempty"`
	}
	Testimonial struct {
		Quote     string `json:"quote"`
		Author    string `json:"author"`
		AvatarURL string `json:"avatar_url,omitempty"`
		Role      string `json:"role,omitempty"`
		URL       string `json:"url,omitempty"`
	}
	TestimonialData struct {
		Heading     string        `json:"heading,omitempty"`
		Description string        `json:"description,omitempty"`
		Items       []Testimonial `json:"items"`
		Schedule    *Schedule     `json:"schedule,omitempty"`
	}
	ContactFormData struct {
		Title       string    `json:"title"`
		ButtonLabel string    `json:"button_label"`
		Schedule    *Schedule `json:"schedule,omitempty"`
	}

	// Phase 2 types.
	HeadingData struct {
		Title     string    `json:"title"`
		TextAlign string    `json:"text_align,omitempty"`
		Schedule  *Schedule `json:"schedule,omitempty"`
	}
	DividerData struct {
		Style    string    `json:"style,omitempty"` // solid | dashed | dotted | double | none (spacer)
		Schedule *Schedule `json:"schedule,omitempty"`
	}
	FileData struct {
		Title    string    `json:"title"`
		URL      string    `json:"url"`
		FileName string    `json:"file_name,omitempty"`
		Size     int64     `json:"size,omitempty"` // bytes
		Schedule *Schedule `json:"schedule,omitempty"`
	}
	DiscountCodeData struct {
		Code        string    `json:"code"`
		Description string    `json:"description,omitempty"`
		Brand       string    `json:"brand,omitempty"`
		Discount    string    `json:"discount,omitempty"` // badge, e.g. "20% off"
		URL         string    `json:"url,omitempty"`
		ButtonLabel string    `json:"button_label,omitempty"`
		Schedule    *Schedule `json:"schedule,omitempty"`
	}
	CountdownData struct {
		EndsAt       time.Time `json:"ends_at"`
		Label        string    `json:"label,omitempty"`
		Description  string    `json:"description,omitempty"`
		EndedMessage string    `json:"ended_message,omitempty"`
		HideAfterEnd bool      `json:"hide_after_end,omitempty"`
		Schedule     *Schedule `json:"schedule,omitempty"`
	}
	BookingData struct {
		URL      string    `json:"url"`
		Schedule *Schedule `json:"schedule,omitempty"`
	}
	QuoteData struct {
		Quote    string    `json:"quote"`
		Name     string    `json:"name,omitempty"`
		Role     string    `json:"role,omitempty"`
		Heading  string    `json:"heading,omitempty"`
		Schedule *Schedule `json:"schedule,omitempty"`
	}
	EmailSignupData struct {
		Title          string    `json:"title,omitempty"`
		Description    string    `json:"description,omitempty"`
		Placeholder    string    `json:"placeholder,omitempty"`
		ButtonLabel    string    `json:"button_label,omitempty"`
		SuccessMessage string    `json:"success_message,omitempty"`
		Schedule       *Schedule `json:"schedule,omitempty"`
	}
)

var blockTypes = map[string]bool{
	"link": true, "text": true, "image": true, "video": true, "music": true,
	"social_icons": true, "folder": true, "faq": true, "testimonial": true, "contact_form": true,
	"heading": true, "divider": true, "file": true, "discount_code": true, "countdown": true,
	"booking": true, "quote": true, "email_signup": true,
}

var (
	linkLayouts  = set("button", "thumbnail", "thumbnail_highlight", "grid2", "grid3", "image_background", "carousel")
	textAligns   = set("left", "center", "right")
	destKinds    = set("url", "email", "file", "tab")
	folderStyles = set("collapsible", "inline")
	dividerStyle = set("solid", "dashed", "dotted", "double", "none")
	uuidRE       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	bookingHosts = set("calendly.com", "cal.com", "app.cal.com")
)

func set(vals ...string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

func oneOf(field, v string, allowed map[string]bool) error {
	if v == "" || allowed[v] {
		return nil
	}
	return fmt.Errorf("%s has an unsupported value %q", field, v)
}

func maxLen(field, v string, n int) error {
	if utf8.RuneCountInString(v) > n {
		return fmt.Errorf("%s must be at most %d characters", field, n)
	}
	return nil
}

func required(field, v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", field)
	}
	return nil
}

func checkSchedule(s *Schedule) error {
	if s != nil && s.StartAt != nil && s.EndAt != nil && !s.EndAt.After(*s.StartAt) {
		return fmt.Errorf("schedule.end_at must be after schedule.start_at")
	}
	return nil
}

// checkIcon accepts an image URL or a short emoji/text glyph.
func checkIcon(v string) error {
	if v == "" {
		return nil
	}
	if strings.Contains(v, "/") {
		return checkURL("icon", v, true)
	}
	return maxLen("icon", v, 8)
}

func checkLink(d *LinkData) error {
	kind := d.DestKind
	if kind == "" {
		kind = "url"
	}
	var destErr error
	switch kind {
	case "url", "file":
		destErr = checkURL("url", d.URL, true)
	case "email":
		addr := strings.TrimPrefix(strings.TrimSpace(d.URL), "mailto:")
		if a, err := mail.ParseAddress(addr); err != nil || a.Address != addr {
			destErr = fmt.Errorf("url must be an email address for dest_kind email")
		} else {
			d.URL = "mailto:" + addr
		}
	case "tab":
		if !uuidRE.MatchString(d.TabID) {
			destErr = fmt.Errorf("tab_id is required for dest_kind tab")
		}
		destErr = firstErr(destErr, checkURL("url", d.URL, false))
	}
	return firstErr(
		oneOf("dest_kind", d.DestKind, destKinds), destErr,
		checkURL("thumbnail_url", d.ThumbnailURL, false),
		oneOf("layout", d.Layout, linkLayouts),
		oneOf("text_align", d.TextAlign, textAligns),
		checkIcon(d.Icon),
		maxLen("label", d.Label, 24),
		maxLen("description", d.Description, 300),
	)
}

// validateBlockData checks raw against the shape for typ and returns a normalised
// encoding (unknown fields dropped, nil slices as []).
func validateBlockData(typ string, raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	var v any
	var check func() error
	var sched func() *Schedule
	switch typ {
	case "link":
		d := &LinkData{}
		v, check, sched = d, func() error { return checkLink(d) }, func() *Schedule { return d.Schedule }
	case "text":
		d := &TextData{}
		v, sched = d, func() *Schedule { return d.Schedule }
	case "image":
		d := &ImageData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error { return firstErr(checkURL("url", d.URL, true), checkURL("link_url", d.LinkURL, false)) }
	case "video", "music":
		d := &EmbedData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error { return checkURL("url", d.URL, true) }
	case "social_icons":
		d := &SocialIconsData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error {
			if d.Items == nil {
				d.Items = make([]SocialIcon, 0)
			}
			for i, it := range d.Items {
				if it.Platform == "" {
					return fmt.Errorf("items[%d].platform is required", i)
				}
				if err := checkURL(fmt.Sprintf("items[%d].url", i), it.URL, true); err != nil {
					return err
				}
			}
			return nil
		}
	case "folder":
		d := &FolderData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error { return oneOf("style", d.Style, folderStyles) }
	case "faq":
		d := &FaqData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error {
			if d.Items == nil {
				d.Items = make([]FaqItem, 0)
			}
			return nil
		}
	case "testimonial":
		d := &TestimonialData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error {
			if d.Items == nil {
				d.Items = make([]Testimonial, 0)
			}
			for i, it := range d.Items {
				if err := firstErr(
					checkURL(fmt.Sprintf("items[%d].avatar_url", i), it.AvatarURL, false),
					checkURL(fmt.Sprintf("items[%d].url", i), it.URL, false),
				); err != nil {
					return err
				}
			}
			return nil
		}
	case "contact_form":
		d := &ContactFormData{}
		v, sched = d, func() *Schedule { return d.Schedule }
	case "heading":
		d := &HeadingData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error {
			return firstErr(maxLen("title", d.Title, 120), oneOf("text_align", d.TextAlign, textAligns))
		}
	case "divider":
		d := &DividerData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error { return oneOf("style", d.Style, dividerStyle) }
	case "file":
		d := &FileData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error {
			if d.Size < 0 {
				return fmt.Errorf("size must not be negative")
			}
			return firstErr(required("title", d.Title), checkURL("url", d.URL, true), maxLen("file_name", d.FileName, 200))
		}
	case "discount_code":
		d := &DiscountCodeData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error {
			return firstErr(maxLen("code", d.Code, 40), maxLen("description", d.Description, 300),
				maxLen("discount", d.Discount, 24), checkURL("url", d.URL, false))
		}
	case "countdown":
		d := &CountdownData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error {
			if d.EndsAt.IsZero() {
				return fmt.Errorf("ends_at is required (RFC 3339)")
			}
			return maxLen("label", d.Label, 120)
		}
	case "booking":
		d := &BookingData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error {
			if err := checkURL("url", d.URL, true); err != nil {
				return err
			}
			u, _ := url.Parse(d.URL)
			if u.Scheme != "https" || !bookingHosts[strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")] {
				return fmt.Errorf("url must be an https calendly.com or cal.com booking link")
			}
			return nil
		}
	case "quote":
		d := &QuoteData{}
		v, sched = d, func() *Schedule { return d.Schedule }
		check = func() error { return firstErr(required("quote", d.Quote), maxLen("quote", d.Quote, 1000)) }
	case "email_signup":
		d := &EmailSignupData{}
		v, sched = d, func() *Schedule { return d.Schedule }
	default:
		return nil, fmt.Errorf("unknown block type %q", typ)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, fmt.Errorf("data does not match %s shape: %v", typ, err)
	}
	if check != nil {
		if err := check(); err != nil {
			return nil, err
		}
	}
	if err := checkSchedule(sched()); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// blockSchedule extracts data.schedule from any block type.
func blockSchedule(data json.RawMessage) *Schedule {
	var d struct {
		Schedule *Schedule `json:"schedule"`
	}
	_ = json.Unmarshal(data, &d)
	return d.Schedule
}

// checkURL allows http(s), mailto and tel links, plus root-relative upload paths.
func checkURL(field, s string, required bool) error {
	s = strings.TrimSpace(s)
	if s == "" {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	if strings.HasPrefix(s, "/uploads/") {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL", field)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return fmt.Errorf("%s is not a valid URL", field)
		}
		return nil
	case "mailto", "tel":
		return nil
	}
	return fmt.Errorf("%s must be an http(s), mailto or tel URL", field)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
