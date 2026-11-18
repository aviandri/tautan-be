// Package storage keeps uploaded files and their image variants, either on
// local disk (development, single VPS) or in an S3-compatible bucket
// (Cloudflare R2, AWS S3, MinIO). The API always serves them itself at
// /uploads/<name>, so stored URLs, the ?w= variant negotiation and the
// page-level media map don't depend on the backend.
package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"time"
)

// ErrNotExist is returned (wrapped) when a name isn't stored.
var ErrNotExist = fs.ErrNotExist

// File is an open stored object. It is seekable so http.ServeContent can
// answer Range requests (MP4 seeking, resumed downloads).
type File interface {
	io.ReadSeekCloser
	Size() int64
	ModTime() time.Time
}

// Store is a flat namespace of files. Names are plain file names (no slashes);
// callers validate them before use.
type Store interface {
	// Put stores r under name, replacing any existing object atomically.
	Put(ctx context.Context, name string, r io.Reader, contentType string) error
	// Open returns the object, or an error wrapping ErrNotExist.
	Open(ctx context.Context, name string) (File, error)
	// List returns every stored name.
	List(ctx context.Context) ([]string, error)
}

// ReadAll reads a whole object, refusing ones larger than max bytes.
func ReadAll(ctx context.Context, s Store, name string, max int64) ([]byte, error) {
	f, err := s.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if f.Size() > max {
		return nil, fmt.Errorf("%s: %d bytes is over the %d byte limit", name, f.Size(), max)
	}
	return io.ReadAll(io.LimitReader(f, max))
}
