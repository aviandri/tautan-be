package storage_test

import (
	"bytes"
	"context"
	"errors"
	. "github.com/aviandri/tautan-be/internal/storage"
	"github.com/aviandri/tautan-be/internal/storage/fakes3"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func stores(t *testing.T) map[string]Store {
	srv := httptest.NewServer(fakes3.New())
	t.Cleanup(srv.Close)
	s3, err := NewS3(S3Config{Bucket: "b", Endpoint: srv.URL, AccessKeyID: "k", SecretAccessKey: "s", Prefix: "uploads/", PathStyle: true})
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Store{"local": Local{Dir: t.TempDir()}, "s3": s3}
}

func TestStores(t *testing.T) {
	ctx := context.Background()
	for name, st := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := st.Open(ctx, "missing.png"); !errors.Is(err, ErrNotExist) {
				t.Fatalf("missing: %v", err)
			}
			if _, err := st.Open(ctx, "../etc/passwd"); !errors.Is(err, ErrNotExist) {
				t.Fatalf("traversal: %v", err)
			}
			data := []byte("0123456789abcdef")
			// a plain io.Reader (not seekable), like an encoder's output
			if err := st.Put(ctx, "a.png", io.MultiReader(bytes.NewReader(data)), "image/png"); err != nil {
				t.Fatal(err)
			}
			if err := st.Put(ctx, "b.pdf", bytes.NewReader([]byte("pdf")), "application/pdf"); err != nil {
				t.Fatal(err)
			}
			got, err := ReadAll(ctx, st, "a.png", 1<<20)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("ReadAll = %q, %v", got, err)
			}
			if _, err := ReadAll(ctx, st, "a.png", 4); err == nil {
				t.Fatal("ReadAll ignored the size limit")
			}
			names, err := st.List(ctx)
			if err != nil || !slices.Equal(names, []string{"a.png", "b.pdf"}) {
				t.Fatalf("List = %v, %v", names, err)
			}
			// Range serving through http.ServeContent
			f, err := st.Open(ctx, "a.png")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if f.Size() != int64(len(data)) || f.ModTime().IsZero() {
				t.Fatalf("size %d mod %v", f.Size(), f.ModTime())
			}
			req := httptest.NewRequest("GET", "/uploads/a.png", nil)
			req.Header.Set("Range", "bytes=10-13")
			rec := httptest.NewRecorder()
			http.ServeContent(rec, req, "a.png", f.ModTime(), f)
			if rec.Code != 206 || rec.Body.String() != "abcd" {
				t.Fatalf("range: %d %q", rec.Code, rec.Body)
			}
			// overwrite is atomic and visible
			if err := st.Put(ctx, "a.png", bytes.NewReader([]byte("new")), "image/png"); err != nil {
				t.Fatal(err)
			}
			if got, _ := ReadAll(ctx, st, "a.png", 100); string(got) != "new" {
				t.Fatalf("after overwrite: %q", got)
			}
		})
	}
}
