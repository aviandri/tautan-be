package storage

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local stores files in a directory.
type Local struct{ Dir string }

type localFile struct {
	*os.File
	info os.FileInfo
}

func (f localFile) Size() int64        { return f.info.Size() }
func (f localFile) ModTime() time.Time { return f.info.ModTime() }

func (l Local) path(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", ErrNotExist
	}
	return filepath.Join(l.Dir, name), nil
}

// Put writes to a temp file in the same directory and renames it into place.
func (l Local) Put(_ context.Context, name string, r io.Reader, _ string) error {
	dst, err := l.path(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(l.Dir, ".upload-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (l Local) Open(_ context.Context, name string) (File, error) {
	p, err := l.path(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err // *PathError wraps fs.ErrNotExist
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		f.Close()
		return nil, ErrNotExist
	}
	return localFile{f, info}, nil
}

func (l Local) List(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
