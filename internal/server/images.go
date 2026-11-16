package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/webp"
	"github.com/jackc/pgx/v5"
	"golang.org/x/image/draw"
	xwebp "golang.org/x/image/webp"
)

// Image variants (perf budget): every uploaded raster image gets AVIF and WebP
// copies at the standard widths below its own width, written next to the
// original as <name>.w<width>.<avif|webp>. Encoding uses WASM builds of libavif
// and libwebp (no CGO, no system libraries) in one background worker, so the
// upload request returns at once. Until a variant exists, ?w= serves the original.

var variantWidths = []int{160, 320, 640, 1080}

const (
	maxVariantPixels = 40_000_000 // skip decoding anything larger (decompression bombs)
	webpQuality      = 75
	avifQuality      = 50
	avifSpeed        = 8
)

// uploadNameRE matches stored image names (storeUpload: 32 hex chars + ext).
var uploadNameRE = regexp.MustCompile(`^[0-9a-f]{32}\.(png|jpg|gif|webp)$`)

// imageInfo is what the frontend needs to avoid layout shift and build srcset.
type imageInfo struct {
	Width  int   `json:"width"`
	Height int   `json:"height"`
	Widths []int `json:"widths"` // variant widths ready to serve; [] until encoded
}

// plannedWidths lists the standard widths smaller than the original.
func plannedWidths(w, h int) []int {
	out := make([]int, 0, len(variantWidths))
	if w*h > maxVariantPixels {
		return out
	}
	for _, vw := range variantWidths {
		if vw < w {
			out = append(out, vw)
		}
	}
	return out
}

// imageDims reads the display size, swapping axes for EXIF-rotated JPEGs.
func imageDims(data []byte) (w, h int, format string, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, "", err
	}
	w, h = cfg.Width, cfg.Height
	if format == "jpeg" && jpegOrientation(data) >= 5 {
		w, h = h, w
	}
	return w, h, format, nil
}

// jpegOrientation returns the EXIF orientation tag (1-8), or 1 if absent.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xDA || marker == 0xD9 { // start of scan / end of image
			return 1
		}
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if size < 2 || i+2+size > len(b) {
			return 1
		}
		seg := b[i+4 : i+2+size]
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return tiffOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off:]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient applies an EXIF orientation so variants are upright.
func orient(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if o >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

func decodeImage(data []byte, format string) (image.Image, error) {
	r := bytes.NewReader(data)
	switch format {
	case "jpeg":
		img, err := jpeg.Decode(r)
		if err != nil {
			return nil, err
		}
		return orient(img, jpegOrientation(data)), nil
	case "png":
		return png.Decode(r)
	case "gif":
		return gif.Decode(r) // first frame
	case "webp":
		return xwebp.Decode(r)
	}
	return nil, fmt.Errorf("unsupported format %q", format)
}

// recordImage stores the dimensions of a new upload and queues its variants.
func (s *Server) recordImage(ctx context.Context, name string, data []byte) (imageInfo, error) {
	w, h, format, err := imageDims(data)
	if err != nil {
		return imageInfo{}, err
	}
	planned := plannedWidths(w, h)
	if format == "gif" {
		planned = []int{} // keep animation: GIFs are served as uploaded
	}
	if _, err := s.db.Exec(ctx, `INSERT INTO images (name, width, height, planned) VALUES ($1, $2, $3, $4)
		ON CONFLICT (name) DO NOTHING`, name, w, h, planned); err != nil {
		return imageInfo{}, err
	}
	if len(planned) > 0 {
		s.enqueueImage(name)
	}
	return imageInfo{Width: w, Height: h, Widths: []int{}}, nil
}

// imageWorker encodes variants one image at a time.
type imageWorker struct {
	once  sync.Once
	queue chan string
}

func (s *Server) enqueueImage(name string) {
	if s.InlineImages {
		if err := s.processImage(context.Background(), name); err != nil {
			log.Printf("image variants %s: %v", name, err)
		}
		return
	}
	s.images.once.Do(func() {
		s.images.queue = make(chan string, 1024)
		go func() {
			for n := range s.images.queue {
				if err := s.processImage(context.Background(), n); err != nil {
					log.Printf("image variants %s: %v", n, err)
				}
			}
		}()
	})
	select {
	case s.images.queue <- name:
	default:
		log.Printf("image queue full, %s will be encoded on next start", name)
	}
}

// processImage writes every planned variant, then marks the image ready.
func (s *Server) processImage(ctx context.Context, name string) error {
	var planned []int
	if err := s.db.QueryRow(ctx, `SELECT planned FROM images WHERE name = $1`, name).Scan(&planned); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.UploadDir, name))
	if err != nil {
		return err
	}
	if len(planned) > 0 {
		_, _, format, err := imageDims(data)
		if err != nil {
			return err
		}
		src, err := decodeImage(data, format)
		if err != nil {
			return err
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		sb := src.Bounds()
		for _, vw := range planned {
			vh := max(1, sb.Dy()*vw/sb.Dx())
			dst := image.NewRGBA(image.Rect(0, 0, vw, vh))
			draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Src, nil)
			if err := s.writeVariant(base, vw, "webp", func(w io.Writer) error {
				return webp.Encode(w, dst, webp.Options{Quality: webpQuality, Method: 4})
			}); err != nil {
				return err
			}
			if err := s.writeVariant(base, vw, "avif", func(w io.Writer) error {
				return avif.Encode(w, dst, avif.Options{Quality: avifQuality, Speed: avifSpeed})
			}); err != nil {
				return err
			}
		}
	}
	if _, err = s.db.Exec(ctx, `UPDATE images SET ready = true WHERE name = $1`, name); err != nil {
		return err
	}
	if len(planned) > 0 {
		s.revalidateImagePages(ctx, name)
	}
	return nil
}

func variantName(base string, w int, ext string) string {
	return base + ".w" + strconv.Itoa(w) + "." + ext
}

// writeVariant encodes to a temp file and renames it into place.
func (s *Server) writeVariant(base string, w int, ext string, enc func(io.Writer) error) error {
	final := filepath.Join(s.cfg.UploadDir, variantName(base, w, ext))
	tmp, err := os.CreateTemp(s.cfg.UploadDir, ".variant-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := enc(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), final)
}

// ResumeImages records uploads that predate the images table and re-queues
// images whose variants were never finished (e.g. the process restarted).
func (s *Server) ResumeImages(ctx context.Context) error {
	entries, err := os.ReadDir(s.cfg.UploadDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	known := map[string]bool{}
	rows, err := s.db.Query(ctx, `SELECT name FROM images`)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, n := range names {
		known[n] = true
	}
	for _, e := range entries {
		if n := e.Name(); uploadNameRE.MatchString(n) && !known[n] {
			data, err := os.ReadFile(filepath.Join(s.cfg.UploadDir, n))
			if err != nil {
				continue
			}
			if _, err := s.recordImage(ctx, n, data); err != nil {
				log.Printf("image %s: %v", n, err)
			}
		}
	}
	rows, err = s.db.Query(ctx, `SELECT name FROM images WHERE NOT ready AND cardinality(planned) > 0`)
	if err != nil {
		return err
	}
	pending, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, n := range pending {
		s.enqueueImage(n)
	}
	return nil
}

// imageInfos looks up dimensions and ready widths for stored image names.
func (s *Server) imageInfos(ctx context.Context, names []string) (map[string]imageInfo, error) {
	out := map[string]imageInfo{}
	if len(names) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT name, width, height, CASE WHEN ready THEN planned ELSE '{}' END
		FROM images WHERE name = ANY($1)`, names)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		var in imageInfo
		if err := rows.Scan(&n, &in.Width, &in.Height, &in.Widths); err != nil {
			return nil, err
		}
		if in.Widths == nil {
			in.Widths = []int{}
		}
		out["/uploads/"+n] = in
	}
	return out, rows.Err()
}

var uploadRefRE = regexp.MustCompile(`/uploads/([0-9a-f]{32}\.(?:png|jpg|gif|webp))`)

// pageMedia maps every image upload the page references ("/uploads/<name>")
// to its size and ready variant widths.
func (s *Server) pageMedia(ctx context.Context, page pageResponse) (map[string]imageInfo, error) {
	raw, err := json.Marshal(struct {
		P Profile
		B []Block
	}{page.Profile, page.Blocks})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range uploadRefRE.FindAllSubmatch(raw, -1) {
		if n := string(m[1]); !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	return s.imageInfos(ctx, names)
}

// serveVariant answers /uploads/<image>?w=N[&fm=avif|webp] with the smallest
// ready variant at least N wide, in the requested or best accepted format.
// It reports false when the request should fall through to the original.
func (s *Server) serveVariant(w http.ResponseWriter, r *http.Request, name string) bool {
	q := r.URL.Query()
	want, err := strconv.Atoi(q.Get("w"))
	if err != nil || want <= 0 || !uploadNameRE.MatchString(name) || strings.HasSuffix(name, ".gif") {
		return false
	}
	format := q.Get("fm")
	negotiated := format == ""
	if negotiated {
		w.Header().Add("Vary", "Accept")
		accept := r.Header.Get("Accept")
		switch {
		case strings.Contains(accept, "image/avif"):
			format = "avif"
		case strings.Contains(accept, "image/webp"):
			format = "webp"
		default:
			return false
		}
	} else if format != "avif" && format != "webp" {
		return false
	}
	var planned []int
	var ready bool
	if err := s.db.QueryRow(r.Context(), `SELECT planned, ready FROM images WHERE name = $1`, name).Scan(&planned, &ready); err != nil || !ready {
		w.Header().Set("Cache-Control", "public, max-age=60") // variants may appear shortly
		return false
	}
	pick := 0
	for _, vw := range planned {
		if vw >= want {
			pick = vw
			break
		}
	}
	if pick == 0 {
		return false // asked for at least the original's width
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	f, err := os.Open(filepath.Join(s.cfg.UploadDir, variantName(base, pick, format)))
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "image/"+format)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", st.ModTime(), f)
	return true
}
