package server

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A tiny GIF that declares huge dimensions is refused (audit L6); a normal
// image is served with a sandboxing CSP (audit L5).
func TestUploadImageCapsAndHeaders(t *testing.T) {
	s, h := newTestServer(t)
	s.cfg.UploadDir = t.TempDir()
	h = s.Routes()
	a := signup(t, h, "uploader")
	gifOf := func(w, hh uint16) []byte {
		var b bytes.Buffer
		if err := gif.Encode(&b, image.NewPaletted(image.Rect(0, 0, 1, 1), color.Palette{color.Black}), nil); err != nil {
			t.Fatal(err)
		}
		out := b.Bytes()
		binary.LittleEndian.PutUint16(out[6:], w) // logical screen width
		binary.LittleEndian.PutUint16(out[8:], hh)
		return out
	}
	send := func(data []byte) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, multipartReq(t, "file", "x.gif", data, a.AccessToken))
		return rec
	}
	if rec := send(gifOf(20000, 10)); rec.Code != 400 || !strings.Contains(rec.Body.String(), "image_too_large") {
		t.Errorf("20000px wide = %d %s", rec.Code, rec.Body)
	}
	if rec := send(gifOf(8000, 8000)); rec.Code != 400 { // 64 MP
		t.Errorf("64 MP = %d", rec.Code)
	}
	rec := send(gifOf(1, 1))
	if rec.Code != 201 {
		t.Fatalf("1x1 = %d %s", rec.Code, rec.Body)
	}
	var up struct {
		URL string `json:"url"`
	}
	resp{rec, t}.json(&up)
	served := httptest.NewRecorder()
	h.ServeHTTP(served, httptest.NewRequest("GET", up.URL[strings.Index(up.URL, "/uploads/"):], nil))
	if served.Code != 200 || !strings.Contains(served.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("served = %d, CSP %q", served.Code, served.Header().Get("Content-Security-Policy"))
	}
}

// refresh, username-available and the Google endpoints share a per-IP cap (audit L9).
func TestAuthMiscRateLimit(t *testing.T) {
	s, h := newTestServer(t)
	s.authMiscLimiter = newRateLimiter(3, time.Minute)
	h = s.Routes() // limitIP binds its limiter when the routes are built
	for i := 0; i < 3; i++ {
		do(t, h, "GET", "/api/v1/auth/username-available?u=someone", nil).expect(200)
	}
	do(t, h, "GET", "/api/v1/auth/username-available?u=someone", nil).expect(429)
	do(t, h, "POST", "/api/v1/auth/refresh", nil).expect(429)
}
