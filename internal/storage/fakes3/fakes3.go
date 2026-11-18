// Package fakes3 is an in-memory S3 server for tests: path-style PUT, HEAD,
// GET (with "bytes=N-" ranges) and ListObjectsV2.
package fakes3

import (
	"encoding/xml"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// New returns an empty fake bucket server.
func New() *Server { return &Server{objs: map[string][]byte{}, ct: map[string]string{}} }

// Keys lists stored object keys.
func (f *Server) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var k []string
	for key := range f.objs {
		k = append(k, key)
	}
	slices.Sort(k)
	return k
}

// Server is an in-memory fake of the subset of the S3 API the store uses (path-style).
type Server struct {
	mu   sync.Mutex
	objs map[string][]byte
	ct   map[string]string
	gets int
}

func (f *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) == 1 || parts[1] == "" { // bucket-level: ListObjectsV2
		prefix := r.URL.Query().Get("prefix")
		type obj struct{ Key string }
		var res struct {
			XMLName  xml.Name `xml:"ListBucketResult"`
			Contents []obj
		}
		var keys []string
		for k := range f.objs {
			if strings.HasPrefix(k, prefix) {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			res.Contents = append(res.Contents, obj{k})
		}
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(res)
		return
	}
	key := parts[1]
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objs[key] = b
		f.ct[key] = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	case http.MethodHead, http.MethodGet:
		b, ok := f.objs[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Last-Modified", time.Unix(1700000000, 0).UTC().Format(http.TimeFormat))
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(b)))
			return
		}
		f.gets++
		if rg := r.Header.Get("Range"); rg != "" {
			start, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rg, "bytes="), "-"))
			w.Header().Set("Content-Range", "bytes "+strconv.Itoa(start)+"-"+strconv.Itoa(len(b)-1)+"/"+strconv.Itoa(len(b)))
			w.WriteHeader(206)
			w.Write(b[start:])
			return
		}
		w.Write(b)
	}
}
