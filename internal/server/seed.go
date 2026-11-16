package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"time"
)

// Heavy demo page for performance testing (perf 2d). "heavy" mirrors the
// 13-block /perfdemo page from tautan-fe docs/perf/baseline-2026-11-16.md
// (avatar 600x600, theme senja, 2 socials, heading, 3 links, YouTube, a
// 1600x1600 image, discount code, countdown, FAQ x3, testimonials x2, form with
// 3 fields, email signup, mailto link) and adds the 9 P3-A sell cards, so every
// run measures the same page. "heavy_all" adds every remaining block type, a
// folder and a second tab. Pages are built through the real HTTP handlers
// in-process, so they always pass current validation, and images get their
// variants before the call returns.

const HeavyPassword = "heavy-demo-password"

// SeedHeavy (re)creates the heavy demo page (full: heavy_all) and returns its username.
func (s *Server) SeedHeavy(ctx context.Context, full bool) (string, error) {
	username := "heavy"
	if full {
		username = "heavy_all"
	}
	email := username + "@example.com"
	if _, err := s.db.Exec(ctx, `DELETE FROM users WHERE email = $1`, email); err != nil {
		return "", err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM username_redirects WHERE old_username = $1`, username); err != nil {
		return "", err
	}
	inline := s.InlineImages
	s.InlineImages = true
	defer func() { s.InlineImages = inline }()
	sd := &seeder{h: s.Routes()}

	var auth authResponse
	if err := sd.call("POST", "/api/v1/auth/signup", map[string]any{
		"email": email, "password": HeavyPassword, "username": username}, &auth); err != nil {
		return "", err
	}
	sd.tok = auth.AccessToken

	img := func(w, h int, hue uint8) string {
		if sd.err != nil {
			return ""
		}
		u, err := sd.upload(demoJPEG(w, h, hue))
		sd.err = err
		return u
	}
	avatar := img(600, 600, 20)
	hero := img(1600, 1600, 200)
	p1, p2, p3 := img(1200, 1200, 90), img(1200, 1200, 140), img(1200, 1500, 250)
	qris := img(600, 600, 0)
	if sd.err != nil {
		return "", sd.err
	}

	sd.call("PATCH", "/api/v1/profile", map[string]any{
		"display_name": "Perf Demo", "bio": "Small-batch tees from Bandung. This page is seeded for Lighthouse runs.",
		"avatar_url": avatar,
		"theme": map[string]any{"preset": "senja",
			"bg":     map[string]any{"type": "gradient", "value": "linear-gradient(160deg, #ff7a59 0%, #ffb35c 100%)"},
			"button": map[string]any{"style": "fill", "radius": 16, "color": "#fff7ee", "text_color": "#3a1608"},
			"font":   "Outfit", "text_color": "#2a1208"},
		"socials": []map[string]any{{"platform": "instagram", "url": "https://instagram.com/perfdemo"},
			{"platform": "tiktok", "url": "https://tiktok.com/@perfdemo"}},
		"onboarding": map[string]any{"completed": true},
	}, nil)

	var ed pageResponse
	sd.call("GET", "/api/v1/profile/editor", nil, &ed)
	if sd.err != nil {
		return "", sd.err
	}
	tab := ed.Tabs[0].ID

	product := func(id, name string, price int64, image string) map[string]any {
		return map[string]any{"id": id, "name": name, "price": price, "compare_at": price + 2000000,
			"images": []string{image}, "category": "Tees",
			"action": map[string]any{"kind": "whatsapp", "wa_number": "+6281234567890", "wa_template": "Hi, I want {name}"}}
	}
	end := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	type blk struct {
		typ  string
		data map[string]any
	}
	blocks := []blk{
		// The 13 blocks of /perfdemo, in order.
		{"heading", map[string]any{"title": "New drop"}},
		{"link", map[string]any{"title": "Website", "url": "https://example.com"}},
		{"link", map[string]any{"title": "Lookbook", "url": "https://example.com/lookbook"}},
		{"link", map[string]any{"title": "Blog", "url": "https://example.com/blog"}},
		{"video", map[string]any{"url": "https://www.youtube.com/watch?v=dQw4w9WgXcQ"}},
		{"image", map[string]any{"url": hero, "alt": "Lookbook cover"}},
		{"discount_code", map[string]any{"code": "PERF10", "discount": "10% off", "description": "On your first order", "url": "https://example.com/shop"}},
		{"countdown", map[string]any{"ends_at": end, "label": "Sale ends"}},
		{"faq", map[string]any{"heading": "FAQ", "items": []map[string]any{
			{"q": "Do you ship abroad?", "a": "Yes, to SG and MY."},
			{"q": "How long does shipping take?", "a": "2-4 days in Java."},
			{"q": "Can I return a size?", "a": "Within 7 days, unworn."}}}},
		{"testimonial", map[string]any{"items": []map[string]any{
			{"quote": "Best tee I own.", "author": "Ani"}, {"quote": "Fast shipping, great fabric.", "author": "Budi"}}}},
		{"form", map[string]any{"title": "Custom order", "fields": []map[string]any{
			{"id": "name", "type": "text", "label": "Name", "required": true},
			{"id": "email", "type": "email", "label": "Email", "required": true},
			{"id": "size", "type": "select", "label": "Size", "options": []string{"S", "M", "L"}}}}},
		{"email_signup", map[string]any{"title": "Get drop alerts"}},
		{"link", map[string]any{"title": "Email us", "url": "hello@example.com", "dest_kind": "email"}},
		// The 9 P3-A sell cards.
		{"product", product("", "Classic Tee", 15000000, p1)},
		{"shop", map[string]any{"title": "Shop", "categories": []string{"Tees", "Hats"}, "items": []any{
			product("tee-1", "Classic Tee", 15000000, p1), product("tee-2", "Oversize Tee", 18000000, p2),
			product("tee-3", "Long Sleeve", 21000000, p3), product("tee-4", "Kids Tee", 9000000, p2)}}},
		{"marketplace", map[string]any{"title": "Also on", "stores": []map[string]any{
			{"platform": "shopee", "url": "https://shopee.co.id/perfdemo"}, {"platform": "tokopedia", "url": "https://tokopedia.com/perfdemo"},
			{"platform": "tiktok_shop", "url": "https://shop.tiktok.com/perfdemo"}}}},
		{"whatsapp", map[string]any{"label": "Order on WhatsApp", "number": "+6281234567890",
			"message_template": "Hi! Order: {items}. Name: {name}. Address: {address}",
			"order_form": map[string]any{"fields": []string{"items", "name", "address"},
				"items": []map[string]any{{"name": "Classic Tee", "price": 15000000}, {"name": "Oversize Tee", "price": 18000000}}}}},
		{"price_list", map[string]any{"sections": []map[string]any{{"title": "Printing", "items": []map[string]any{
			{"name": "Screen print (per tee)", "price": 2500000, "price_from": true}, {"name": "Embroidery", "price": 4000000}}}},
			"ask": map[string]any{"kind": "whatsapp", "target": "+6281234567890", "label": "Ask"}}},
		{"store_info", map[string]any{"tz": "Asia/Jakarta", "address": "Jl. Braga 10", "area": "Bandung",
			"hours":    []map[string]any{{"day": "mon", "open": "09:00", "close": "21:00"}, {"day": "sat", "open": "10:00", "close": "22:00"}},
			"couriers": []string{"jne", "gosend", "pickup"}}},
		{"payment", map[string]any{"title": "Pay", "qris_image_url": qris,
			"accounts": []map[string]any{{"bank": "BCA", "name": "Perf Demo", "number": "1234567890"}},
			"confirm":  map[string]any{"kind": "whatsapp", "target": "+6281234567890"}}},
		{"tip_jar", map[string]any{"title": "Buy us a coffee", "amounts": []int64{1000000, 2500000},
			"platforms": []map[string]any{{"platform": "saweria", "url": "https://saweria.co/perfdemo"}}}},
		{"reviews", map[string]any{"average": 4.8, "count": 212, "distribution": []int{180, 20, 6, 3, 3},
			"items": []map[string]any{{"stars": 5, "quote": "Fabric is great.", "author": "Citra", "source_url": "https://maps.google.com/?cid=1"}}}},
	}
	if full {
		blocks = append(blocks,
			blk{"text", map[string]any{"content": "We make small-batch tees in Bandung. Every order ships in 2 days."}},
			blk{"music", map[string]any{"url": "https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC"}},
			blk{"social_icons", map[string]any{"items": []map[string]any{{"platform": "instagram", "url": "https://instagram.com/perfdemo"}}}},
			blk{"contact_form", map[string]any{"title": "Say hi", "button_label": "Send"}},
			blk{"divider", map[string]any{"style": "dashed"}},
			blk{"file", map[string]any{"title": "Size guide", "url": "https://example.com/size-guide.pdf"}},
			blk{"booking", map[string]any{"url": "https://calendly.com/perfdemo/30min"}},
			blk{"quote", map[string]any{"quote": "Make it simple, but significant.", "name": "Don Draper"}},
		)
	}
	for _, b := range blocks {
		sd.call("POST", "/api/v1/blocks", map[string]any{"tab_id": tab, "type": b.typ, "data": b.data}, nil)
	}
	if full {
		var folder Block
		sd.call("POST", "/api/v1/blocks", map[string]any{"tab_id": tab, "type": "folder", "data": map[string]any{"title": "More links"}}, &folder)
		for i := 1; i <= 3; i++ {
			sd.call("POST", "/api/v1/blocks", map[string]any{"tab_id": tab, "parent_id": folder.ID, "type": "link",
				"data": map[string]any{"title": fmt.Sprintf("Link %d", i), "url": fmt.Sprintf("https://example.com/%d", i)}}, nil)
		}
		var tab2 Tab
		sd.call("POST", "/api/v1/tabs", map[string]any{"title": "About"}, &tab2)
		sd.call("POST", "/api/v1/blocks", map[string]any{"tab_id": tab2.ID, "type": "image", "data": map[string]any{"url": p3, "alt": "Studio"}}, nil)
		sd.call("PATCH", "/api/v1/profile", map[string]any{"announcement": map[string]any{"layout": "banner", "title": "Free shipping this week",
			"cta": map[string]any{"label": "Shop", "url": "https://example.com/shop"}}}, nil)
	}
	sd.call("PATCH", "/api/v1/profile", map[string]any{"is_published": true}, nil)
	return username, sd.err
}

// seeder calls the API in-process and keeps the first error.
type seeder struct {
	h   http.Handler
	tok string
	err error
}

func (sd *seeder) call(method, path string, body, out any) error {
	if sd.err != nil {
		return sd.err
	}
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	sd.send(req, out)
	if sd.err != nil {
		sd.err = fmt.Errorf("%s %s: %w", method, path, sd.err)
	}
	return sd.err
}

func (sd *seeder) upload(data []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "seed.jpg")
	fw.Write(data)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/uploads", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var out struct {
		URL string `json:"url"`
	}
	sd.send(req, &out)
	return out.URL, sd.err
}

func (sd *seeder) send(req *http.Request, out any) {
	if sd.tok != "" {
		req.Header.Set("Authorization", "Bearer "+sd.tok)
	}
	rec := httptest.NewRecorder()
	sd.h.ServeHTTP(rec, req)
	if rec.Code >= 300 {
		sd.err = fmt.Errorf("status %d: %s", rec.Code, bytes.TrimSpace(rec.Body.Bytes()))
		return
	}
	if out != nil {
		sd.err = json.Unmarshal(rec.Body.Bytes(), out)
	}
}

// demoJPEG draws a soft diagonal gradient so variants have realistic sizes.
func demoJPEG(w, h int, hue uint8) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := uint8((x + y) * 255 / (w + h))
			img.Set(x, y, color.RGBA{hue + t/2, 120 + t/3, 255 - t, 255})
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, img, &jpeg.Options{Quality: 85})
	return b.Bytes()
}
