package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func multipartReq(t *testing.T, field, filename string, content []byte, tok string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile(field, filename)
	fw.Write(content)
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/uploads", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req
}

func pngBytes() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestUploads(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.UploadDir = t.TempDir()
	s.cfg.PublicBaseURL = "http://api.test"
	h := s.Routes()
	a := signup(t, h, "alice")

	serve := func(req *http.Request) resp {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return resp{rec, t}
	}

	img := pngBytes()
	var out struct{ URL string }
	serve(multipartReq(t, "file", "a.png", img, a.AccessToken)).expect(201).json(&out)
	if !strings.HasPrefix(out.URL, "http://api.test/uploads/") || !strings.HasSuffix(out.URL, ".png") {
		t.Fatalf("url = %s", out.URL)
	}

	// Served back with the right bytes and headers.
	path := strings.TrimPrefix(out.URL, "http://api.test")
	r := serve(httptest.NewRequest("GET", path, nil)).expect(200)
	if !bytes.Equal(r.Body.Bytes(), img) || r.Header().Get("Content-Type") != "image/png" || r.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("served wrong: ct=%s", r.Header().Get("Content-Type"))
	}
	serve(httptest.NewRequest("GET", "/uploads/", nil)).expect(404)

	// Rejections.
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	if c := serve(multipartReq(t, "file", "x.png", svg, a.AccessToken)).expect(415).errCode(); c != "unsupported_type" {
		t.Errorf("svg code = %s", c)
	}
	serve(multipartReq(t, "image", "a.png", img, a.AccessToken)).expect(400)
	serve(multipartReq(t, "file", "a.png", img, "")).expect(401)
	big := append(pngBytes(), make([]byte, maxUploadBytes+10)...)
	serve(multipartReq(t, "file", "big.png", big, a.AccessToken)).expect(413)
}
