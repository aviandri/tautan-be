package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	hexColorRE  = regexp.MustCompile(`^#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	funcColorRE = regexp.MustCompile(`^(rgb|rgba|hsl|hsla)\([0-9.,%\s/a-z]+\)$`)
	namedColRE  = regexp.MustCompile(`^[a-z]{3,20}$`)
	gradientRE  = regexp.MustCompile(`^(repeating-)?(linear|radial|conic)-gradient\([#0-9a-zA-Z.,%\s()/-]+\)$`)
	fontNameRE  = regexp.MustCompile(`^[A-Za-z0-9 ]{1,40}$`)
	presetRE    = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	platformRE  = regexp.MustCompile(`^[a-z0-9_]{1,30}$`)

	bgTypes       = set("color", "gradient", "image", "split", "none")
	buttonStyles  = set("fill", "outline", "shadow")
	shadowStyles  = set("soft", "solid")
	headerLayouts = set("classic", "banner", "headshot", "business")
	paletteModes  = set("cycle", "by_type", "mono")
	// Palette colours are stricter than other theme colours: hex or rgb()/rgba() only.
	paletteColorRE = regexp.MustCompile(`^(#([0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})|rgba?\(\s*[0-9.]+%?\s*[, ]\s*[0-9.]+%?\s*[, ]\s*[0-9.]+%?\s*([,/]\s*[0-9.]+%?\s*)?\))$`)
)

// Theme v2. Unknown keys are passed through untouched (validated keys only),
// so FE additions never break saves.
type themeV2 struct {
	Preset    *string  `json:"preset"`
	TextColor *string  `json:"text_color"`
	Font      *string  `json:"font"`
	BgDesktop *string  `json:"bg_desktop"`
	Spacing   *float64 `json:"spacing"`
	Bg        *struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"bg"`
	Button *struct {
		Style     string   `json:"style"`
		Radius    *float64 `json:"radius"`
		Color     string   `json:"color"`
		TextColor string   `json:"text_color"`
	} `json:"button"`
	Block  *colorPair `json:"block"`
	Label  *colorPair `json:"label"`
	Border *struct {
		Width *float64 `json:"width"`
		Color string   `json:"color"`
	} `json:"border"`
	Shadow *struct {
		Amount *float64 `json:"amount"`
		Style  string   `json:"style"`
	} `json:"shadow"`
	Fonts *struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	} `json:"fonts"`
	Header *struct {
		Layout         string `json:"layout"`
		BannerImageURL string `json:"banner_image_url"`
	} `json:"header"`
	// Multi-colour themes: cards take surfaces in turn (cycle), by card type, or one (mono).
	Palette     *themePalette `json:"palette"`
	PaletteMode *string       `json:"palette_mode"`
}

type themePalette struct {
	Surfaces    []string `json:"surfaces"`
	Accents     []string `json:"accents"`
	Ink         string   `json:"ink"`
	InkOnAccent string   `json:"ink_on_accent"`
}

func checkPaletteColor(field, v string) error {
	if !paletteColorRE.MatchString(strings.TrimSpace(v)) {
		return fmt.Errorf("%s must be a hex (#rrggbb) or rgb()/rgba() colour", field)
	}
	return nil
}

func checkPalette(p *themePalette) error {
	if p == nil {
		return nil
	}
	if n := len(p.Surfaces); n < 4 || n > 6 {
		return fmt.Errorf("palette.surfaces needs 4 to 6 colours")
	}
	if n := len(p.Accents); n < 1 || n > 2 {
		return fmt.Errorf("palette.accents needs 1 or 2 colours")
	}
	for i, c := range p.Surfaces {
		if err := checkPaletteColor(fmt.Sprintf("palette.surfaces[%d]", i), c); err != nil {
			return err
		}
	}
	for i, c := range p.Accents {
		if err := checkPaletteColor(fmt.Sprintf("palette.accents[%d]", i), c); err != nil {
			return err
		}
	}
	return firstErr(checkPaletteColor("palette.ink", p.Ink), checkPaletteColor("palette.ink_on_accent", p.InkOnAccent))
}

type colorPair struct {
	Color     string `json:"color"`
	TextColor string `json:"text_color"`
}

func checkColor(field, v string) error {
	v = strings.TrimSpace(v)
	if v == "" || hexColorRE.MatchString(v) || funcColorRE.MatchString(v) || namedColRE.MatchString(v) {
		return nil
	}
	return fmt.Errorf("%s must be a CSS colour (#hex, rgb(), hsl() or a name)", field)
}

func checkGradient(field, v string) error {
	if len(v) <= 300 && gradientRE.MatchString(strings.TrimSpace(v)) {
		return nil
	}
	return fmt.Errorf("%s must be a CSS linear/radial/conic-gradient(...)", field)
}

func checkFont(field, v string) error {
	if v == "" || fontNameRE.MatchString(v) {
		return nil
	}
	return fmt.Errorf("%s must be a font family name", field)
}

func checkRange(field string, v *float64, lo, hi float64) error {
	if v != nil && (*v < lo || *v > hi) {
		return fmt.Errorf("%s must be between %g and %g", field, lo, hi)
	}
	return nil
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// validateTheme checks the known theme keys. It returns nil if raw is acceptable.
func validateTheme(raw json.RawMessage) error {
	if !isJSONObject(raw) {
		return fmt.Errorf("theme must be an object")
	}
	var t themeV2
	if err := json.Unmarshal(raw, &t); err != nil {
		return fmt.Errorf("theme has a field of the wrong type: %v", err)
	}
	errs := []error{
		checkColor("text_color", str(t.TextColor)),
		checkColor("bg_desktop", str(t.BgDesktop)),
		checkFont("font", str(t.Font)),
		checkRange("spacing", t.Spacing, 0, 64),
	}
	if t.Preset != nil && *t.Preset != "" && !presetRE.MatchString(*t.Preset) {
		errs = append(errs, fmt.Errorf("preset must be a short lowercase id"))
	}
	if b := t.Bg; b != nil {
		errs = append(errs, oneOf("bg.type", b.Type, bgTypes))
		switch b.Type {
		case "color":
			errs = append(errs, checkColor("bg.value", b.Value))
		case "gradient", "split":
			errs = append(errs, checkGradient("bg.value", b.Value))
		case "image":
			errs = append(errs, checkURL("bg.value", b.Value, true))
		}
	}
	if b := t.Button; b != nil {
		errs = append(errs, oneOf("button.style", b.Style, buttonStyles), checkRange("button.radius", b.Radius, 0, 100),
			checkColor("button.color", b.Color), checkColor("button.text_color", b.TextColor))
	}
	for name, p := range map[string]*colorPair{"block": t.Block, "label": t.Label} {
		if p != nil {
			errs = append(errs, checkColor(name+".color", p.Color), checkColor(name+".text_color", p.TextColor))
		}
	}
	if b := t.Border; b != nil {
		errs = append(errs, checkRange("border.width", b.Width, 0, 12), checkColor("border.color", b.Color))
	}
	if s := t.Shadow; s != nil {
		errs = append(errs, checkRange("shadow.amount", s.Amount, 0, 100), oneOf("shadow.style", s.Style, shadowStyles))
	}
	if f := t.Fonts; f != nil {
		errs = append(errs, checkFont("fonts.title", f.Title), checkFont("fonts.body", f.Body))
	}
	errs = append(errs, checkPalette(t.Palette))
	if t.PaletteMode != nil && *t.PaletteMode != "" {
		errs = append(errs, oneOf("palette_mode", *t.PaletteMode, paletteModes))
	}
	if h := t.Header; h != nil {
		errs = append(errs, oneOf("header.layout", h.Layout, headerLayouts), checkURL("header.banner_image_url", h.BannerImageURL, false))
	}
	return firstErr(errs...)
}

// normalizeSocials validates the header socials row and returns its encoding.
func normalizeSocials(raw json.RawMessage) (json.RawMessage, error) {
	var items []SocialIcon
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("socials must be an array of {platform, url}")
	}
	if items == nil {
		items = []SocialIcon{}
	}
	if len(items) > 30 {
		return nil, fmt.Errorf("at most 30 socials")
	}
	for i := range items {
		it := &items[i]
		it.Platform = strings.ToLower(strings.TrimSpace(it.Platform))
		if !platformRE.MatchString(it.Platform) {
			return nil, fmt.Errorf("socials[%d].platform must be a short lowercase id", i)
		}
		if it.Platform == "divider" {
			it.URL = ""
			continue
		}
		if err := checkURL(fmt.Sprintf("socials[%d].url", i), it.URL, true); err != nil {
			return nil, err
		}
	}
	return json.Marshal(items)
}
