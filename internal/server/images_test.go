package server

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sizedPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// jpegWithOrientation builds a JPEG whose APP1 Exif segment sets the orientation tag.
func jpegWithOrientation(w, h, o int) []byte {
	var j bytes.Buffer
	jpeg.Encode(&j, image.NewRGBA(image.Rect(0, 0, w, h)), nil)
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08")
	tiff = binary.BigEndian.AppendUint16(tiff, 1)         // one IFD entry
	tiff = binary.BigEndian.AppendUint16(tiff, 0x0112)    // orientation
	tiff = binary.BigEndian.AppendUint16(tiff, 3)         // SHORT
	tiff = binary.BigEndian.AppendUint32(tiff, 1)         // count
	tiff = binary.BigEndian.AppendUint16(tiff, uint16(o)) // value
	tiff = append(tiff, 0, 0, 0, 0, 0, 0)
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xFF, 0xE1}
	app1 = binary.BigEndian.AppendUint16(app1, uint16(len(seg)+2))
	app1 = append(app1, seg...)
	b := j.Bytes()
	return append(append(append([]byte{}, b[:2]...), app1...), b[2:]...)
}

func TestImageDimsAndOrientation(t *testing.T) {
	if w, h, f, err := imageDims(jpegWithOrientation(40, 20, 6)); err != nil || w != 20 || h != 40 || f != "jpeg" {
		t.Errorf("rotated jpeg dims = %d×%d %s %v", w, h, f, err)
	}
	if w, h, _, _ := imageDims(jpegWithOrientation(40, 20, 3)); w != 40 || h != 20 {
		t.Errorf("upside-down jpeg dims = %d×%d", w, h)
	}
	if o := jpegOrientation([]byte("not a jpeg")); o != 1 {
		t.Errorf("garbage orientation = %d", o)
	}
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	r := orient(img, 6) // 90° clockwise: top-left goes to top-right
	if r.Bounds().Dx() != 2 || r.Bounds().Dy() != 3 {
		t.Fatalf("orient bounds = %v", r.Bounds())
	}
	if c := color.RGBAModel.Convert(r.At(1, 0)).(color.RGBA); c.R != 255 {
		t.Errorf("orient 6 moved the pixel wrong: %v", c)
	}
	if got := plannedWidths(700, 400); len(got) != 3 || got[2] != 640 {
		t.Errorf("planned = %v", got)
	}
	if got := plannedWidths(10000, 5000); len(got) != 0 {
		t.Errorf("huge image planned = %v", got)
	}
}

func TestImageVariants(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.UploadDir = t.TempDir()
	s.cfg.PublicBaseURL = ""
	s.InlineImages = true
	h := s.Routes()
	a := signup(t, h, "pics")
	serve := func(req *http.Request) resp {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return resp{rec, t}
	}

	var out struct {
		URL           string
		Width, Height int
	}
	serve(multipartReq(t, "file", "a.png", sizedPNG(400, 300), a.AccessToken)).expect(201).json(&out)
	if out.Width != 400 || out.Height != 300 || !strings.HasPrefix(out.URL, "/uploads/") {
		t.Fatalf("upload = %+v", out)
	}
	base := strings.TrimSuffix(filepath.Base(out.URL), ".png")
	for _, n := range []string{".w160.webp", ".w160.avif", ".w320.webp", ".w320.avif"} {
		if _, err := os.Stat(filepath.Join(s.cfg.UploadDir, base+n)); err != nil {
			t.Errorf("missing variant %s", n)
		}
	}
	if _, err := os.Stat(filepath.Join(s.cfg.UploadDir, base+".w640.webp")); err == nil {
		t.Error("variant wider than the original was written")
	}

	get := func(q, accept string) resp {
		req := httptest.NewRequest("GET", out.URL+q, nil)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		return serve(req).expect(200)
	}
	r := get("?w=200", "image/avif,image/webp,*/*")
	if r.Header().Get("Content-Type") != "image/avif" || !strings.Contains(strings.Join(r.Header().Values("Vary"), ","), "Accept") || !strings.Contains(r.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("avif negotiation: %v", r.Header())
	}
	if r := get("?w=200", "image/webp,*/*"); r.Header().Get("Content-Type") != "image/webp" {
		t.Errorf("webp negotiation: %v", r.Header())
	}
	if r := get("?w=100&fm=webp", ""); r.Header().Get("Content-Type") != "image/webp" {
		t.Errorf("explicit fm: %v", r.Header())
	}
	if r := get("?w=2000", "image/avif"); r.Header().Get("Content-Type") != "image/png" {
		t.Errorf("wider than original should serve the original: %v", r.Header())
	}
	if r := get("?w=200", "text/html"); r.Header().Get("Content-Type") != "image/png" {
		t.Errorf("no modern format accepted should serve the original: %v", r.Header())
	}
	if r := get("", ""); r.Header().Get("Content-Type") != "image/png" {
		t.Error("plain original")
	}
	if r := serve(httptest.NewRequest("GET", "/uploads/"+base+".w160.avif", nil)).expect(200); r.Header().Get("Content-Type") != "image/avif" {
		t.Errorf("direct variant content type = %s", r.Header().Get("Content-Type"))
	}

	// Not yet encoded: the original, with a short cache.
	var late struct{ URL string }
	s.InlineImages = false
	s.images.once.Do(func() { s.images.queue = make(chan string, 8) }) // worker never started: stays pending
	serve(multipartReq(t, "file", "b.png", sizedPNG(400, 300), a.AccessToken)).expect(201).json(&late)
	r = serve(httptest.NewRequest("GET", late.URL+"?w=200&fm=avif", nil)).expect(200)
	if r.Header().Get("Content-Type") != "image/png" || r.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Errorf("pending variant fallback: %v", r.Header())
	}

	// The public page lists referenced images.
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"avatar_url": out.URL, "is_published": true}, bearer(a.AccessToken)).expect(200)
	tab := editor(t, h, a.AccessToken).Tabs[0]
	createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "image", "data": map[string]any{"url": "http://cdn.example" + late.URL}})
	var page struct {
		Media map[string]imageInfo `json:"media"`
	}
	do(t, h, "GET", "/api/v1/public/pics", nil).expect(200).json(&page)
	if m := page.Media[out.URL]; m.Width != 400 || m.Height != 300 || len(m.Widths) != 2 {
		t.Errorf("media[avatar] = %+v (all %v)", m, page.Media)
	}
	if m, ok := page.Media[late.URL]; !ok || len(m.Widths) != 0 || m.Widths == nil {
		t.Errorf("media[pending] = %+v", m)
	}
}
