package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Block data shapes; keep in sync with SPEC.md and tautan-fe/src/lib/types.ts.
type (
	LinkData struct {
		Title        string `json:"title"`
		URL          string `json:"url"`
		ThumbnailURL string `json:"thumbnail_url,omitempty"`
	}
	TextData struct {
		Content string `json:"content"`
	}
	ImageData struct {
		URL     string `json:"url"`
		Alt     string `json:"alt,omitempty"`
		LinkURL string `json:"link_url,omitempty"`
	}
	EmbedData struct {
		URL string `json:"url"`
	} // video, music
	SocialIcon struct {
		Platform string `json:"platform"`
		URL      string `json:"url"`
	}
	SocialIconsData struct {
		Items []SocialIcon `json:"items"`
	}
	FolderData struct {
		Title string `json:"title"`
	}
	FaqItem struct {
		Q string `json:"q"`
		A string `json:"a"`
	}
	FaqData struct {
		Items []FaqItem `json:"items"`
	}
	Testimonial struct {
		Quote     string `json:"quote"`
		Author    string `json:"author"`
		AvatarURL string `json:"avatar_url,omitempty"`
	}
	TestimonialData struct {
		Items []Testimonial `json:"items"`
	}
	ContactFormData struct {
		Title       string `json:"title"`
		ButtonLabel string `json:"button_label"`
	}
)

var blockTypes = map[string]bool{
	"link": true, "text": true, "image": true, "video": true, "music": true,
	"social_icons": true, "folder": true, "faq": true, "testimonial": true, "contact_form": true,
}

// validateBlockData checks raw against the shape for typ and returns a normalised
// encoding (unknown fields dropped, nil slices as []).
func validateBlockData(typ string, raw json.RawMessage) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	var v any
	var check func() error
	switch typ {
	case "link":
		d := &LinkData{}
		v, check = d, func() error {
			return firstErr(checkURL("url", d.URL, true), checkURL("thumbnail_url", d.ThumbnailURL, false))
		}
	case "text":
		v, check = &TextData{}, nil
	case "image":
		d := &ImageData{}
		v, check = d, func() error { return firstErr(checkURL("url", d.URL, true), checkURL("link_url", d.LinkURL, false)) }
	case "video", "music":
		d := &EmbedData{}
		v, check = d, func() error { return checkURL("url", d.URL, true) }
	case "social_icons":
		d := &SocialIconsData{}
		v, check = d, func() error {
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
		v, check = &FolderData{}, nil
	case "faq":
		d := &FaqData{}
		v, check = d, func() error {
			if d.Items == nil {
				d.Items = make([]FaqItem, 0)
			}
			return nil
		}
	case "testimonial":
		d := &TestimonialData{}
		v, check = d, func() error {
			if d.Items == nil {
				d.Items = make([]Testimonial, 0)
			}
			for i, it := range d.Items {
				if err := checkURL(fmt.Sprintf("items[%d].avatar_url", i), it.AvatarURL, false); err != nil {
					return err
				}
			}
			return nil
		}
	case "contact_form":
		v, check = &ContactFormData{}, nil
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
	return json.Marshal(v)
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
