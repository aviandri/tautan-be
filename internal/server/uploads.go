package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const maxUploadBytes = 5 << 20

// Raster images only; SVG is excluded because it can carry script.
var uploadTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file must be at most 5 MB")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_upload", "expected multipart/form-data with a \"file\" field")
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", "missing \"file\" field")
		return
	}
	defer f.Close()
	if hdr.Size > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "file must be at most 5 MB")
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ext, ok := uploadTypes[http.DetectContentType(head[:n])]
	if !ok {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "only PNG, JPEG, GIF and WebP images are allowed")
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		s.internalError(w, err)
		return
	}

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	name := hex.EncodeToString(b) + ext
	if err := os.MkdirAll(s.cfg.UploadDir, 0o755); err != nil {
		s.internalError(w, err)
		return
	}
	dst, err := os.OpenFile(filepath.Join(s.cfg.UploadDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if _, err := io.Copy(dst, f); err != nil {
		dst.Close()
		os.Remove(dst.Name())
		s.internalError(w, err)
		return
	}
	if err := dst.Close(); err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"url": s.cfg.PublicBaseURL + "/uploads/" + name})
}

// serveUploads serves stored files without directory listings.
func (s *Server) serveUploads() http.Handler {
	fs := http.StripPrefix("/uploads/", http.FileServer(http.Dir(s.cfg.UploadDir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		fs.ServeHTTP(w, r)
	})
}
