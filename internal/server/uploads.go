package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	maxUploadBytes = 5 << 20  // images (default kind)
	maxFileBytes   = 25 << 20 // kind=file, for the file block
)

// Raster images only; SVG is excluded because it can carry script.
var uploadTypes = map[string]string{
	"image/png":  ".png",
	"image/jpeg": ".jpg",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Extra types accepted with ?kind=file. They are served as downloads.
var fileTypes = map[string]string{
	"application/pdf":           ".pdf",
	"application/zip":           ".zip",
	"audio/mpeg":                ".mp3",
	"video/mp4":                 ".mp4",
	"text/plain; charset=utf-8": ".txt",
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	isFile := r.URL.Query().Get("kind") == "file"
	limit := int64(maxUploadBytes)
	if isFile {
		limit = maxFileBytes
	}
	tooLarge := func() {
		writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", fmt.Sprintf("file must be at most %d MB", limit>>20))
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			tooLarge()
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
	if hdr.Size > limit {
		tooLarge()
		return
	}
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	ctype := http.DetectContentType(head[:n])
	ext, ok := uploadTypes[ctype]
	if !ok && isFile {
		ext, ok = fileTypes[ctype]
	}
	if !ok {
		msg := "only PNG, JPEG, GIF and WebP images are allowed"
		if isFile {
			msg = "allowed files: images, PDF, ZIP, MP3, MP4 and plain text"
		}
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", msg)
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		s.internalError(w, err)
		return
	}

	var data []byte
	var src io.Reader = f
	_, isImage := uploadTypes[ctype]
	if isImage {
		if data, err = io.ReadAll(io.LimitReader(f, limit+1)); err != nil {
			s.internalError(w, err)
			return
		}
		if _, _, _, err := imageDims(data); err != nil {
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_type", "the image could not be read")
			return
		}
		src = bytes.NewReader(data)
	}
	fileURL, err := s.storeUpload(src, ext)
	if err != nil {
		s.internalError(w, err)
		return
	}
	out := map[string]any{
		"url":          fileURL,
		"file_name":    safeFileName(hdr.Filename),
		"size":         hdr.Size,
		"content_type": strings.SplitN(ctype, ";", 2)[0],
	}
	if isImage {
		info, err := s.recordImage(r.Context(), path.Base(fileURL), data)
		if err != nil {
			s.internalError(w, err)
			return
		}
		out["width"], out["height"] = info.Width, info.Height
	}
	writeJSON(w, http.StatusCreated, out)
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
		if r.URL.Query().Has("w") && s.serveVariant(w, r, strings.TrimPrefix(r.URL.Path, "/uploads/")) {
			return
		}
		switch strings.ToLower(filepath.Ext(r.URL.Path)) {
		case ".png", ".jpg", ".gif", ".webp":
		case ".avif":
			w.Header().Set("Content-Type", "image/avif")
		default:
			w.Header().Set("Content-Disposition", "attachment")
		}
		if w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fs.ServeHTTP(w, r)
	})
}

// safeFileName keeps a display name for the uploaded file (never used on disk).
func safeFileName(n string) string {
	n = filepath.Base(strings.ReplaceAll(n, "\\", "/"))
	n = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`"<>|`, r) {
			return -1
		}
		return r
	}, n)
	if len([]rune(n)) > 200 {
		n = string([]rune(n)[:200])
	}
	if n == "." || n == "/" {
		return ""
	}
	return n
}

// storeUpload writes r to a new randomly named file and returns its public URL.
func (s *Server) storeUpload(r io.Reader, ext string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	name := hex.EncodeToString(b) + ext
	if err := os.MkdirAll(s.cfg.UploadDir, 0o755); err != nil {
		return "", err
	}
	dst, err := os.OpenFile(filepath.Join(s.cfg.UploadDir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(dst, r); err != nil {
		dst.Close()
		os.Remove(dst.Name())
		return "", err
	}
	if err := dst.Close(); err != nil {
		return "", err
	}
	return s.cfg.PublicBaseURL + "/uploads/" + name, nil
}
