package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5"
)

// Revalidate webhook (perf 2c). When a published page changes, the frontend's
// ISR cache is told to drop it:
//
//	POST $REVALIDATE_URL
//	X-Tautan-Timestamp: <unix seconds>
//	X-Tautan-Signature: sha256=<hex HMAC-SHA256(secret, "<timestamp>.<body>")>
//	{"usernames":["alice","old-alice"]}
//
// This matches the frontend route (tautan-fe perf/public-page): 1-20 names per
// call, fire-and-forget with a 2 s timeout and no retries, since the 60 s ISR
// refresh is the safety net. Changes are debounced so a burst of edits (drag
// reordering) sends one call.

const (
	revalidateDebounce = 750 * time.Millisecond
	revalidateBatch    = 20
)

type revalidator struct {
	mu       sync.Mutex
	pending  map[string]bool
	timer    *time.Timer
	debounce time.Duration
	client   *http.Client
}

func newRevalidator() *revalidator {
	return &revalidator{pending: map[string]bool{}, debounce: revalidateDebounce,
		client: &http.Client{Timeout: 2 * time.Second}}
}

// revalidate queues usernames for the next webhook call.
func (s *Server) revalidate(usernames ...string) {
	if s.cfg.RevalidateSecret == "" || s.cfg.RevalidateURL == "" || len(usernames) == 0 {
		return
	}
	rv := s.reval
	rv.mu.Lock()
	defer rv.mu.Unlock()
	for _, u := range usernames {
		if u != "" {
			rv.pending[u] = true
		}
	}
	if rv.timer == nil && len(rv.pending) > 0 {
		rv.timer = time.AfterFunc(rv.debounce, s.flushRevalidate)
	}
}

func (s *Server) flushRevalidate() {
	rv := s.reval
	rv.mu.Lock()
	names := make([]string, 0, len(rv.pending))
	for u := range rv.pending {
		names = append(names, u)
	}
	rv.pending, rv.timer = map[string]bool{}, nil
	rv.mu.Unlock()
	slices.Sort(names)
	if len(names) == 0 {
		return
	}
	for len(names) > 0 {
		n := min(len(names), revalidateBatch)
		body, _ := json.Marshal(map[string]any{"usernames": names[:n]})
		if status, err := s.postRevalidate(body); err != nil || status >= 300 {
			log.Printf("revalidate %v: status %d, %v", names[:n], status, err)
		}
		names = names[n:]
	}
}

func (s *Server) postRevalidate(body []byte) (int, error) {
	ts := strconv.FormatInt(s.now().Unix(), 10)
	req, err := http.NewRequest(http.MethodPost, s.cfg.RevalidateURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tautan-api/revalidate")
	req.Header.Set("X-Tautan-Timestamp", ts)
	req.Header.Set("X-Tautan-Signature", "sha256="+revalidateSignature(s.cfg.RevalidateSecret, ts, body))
	res, err := s.reval.client.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

func revalidateSignature(secret, ts string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

type pageState struct {
	username  string
	published bool
}

func (s *Server) pageState(ctx context.Context, pid string) (pageState, error) {
	var st pageState
	err := s.db.QueryRow(ctx, `SELECT username, is_published FROM profiles WHERE id = $1`, pid).Scan(&st.username, &st.published)
	return st, err
}

// revalidatesPage wraps editor mutations: after a 2xx it queues the page's
// username, plus the old one and every redirecting username on a rename, when
// the page was published before or after the change.
func (s *Server) revalidatesPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.RevalidateSecret == "" {
			next.ServeHTTP(w, r)
			return
		}
		pid := profileID(r)
		before, err := s.pageState(r.Context(), pid)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if ww.Status() >= 300 {
			return
		}
		// The request context may be cancelled once the response is written.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		after, err := s.pageState(ctx, pid)
		if err != nil || !(before.published || after.published) {
			return
		}
		names := []string{after.username}
		if before.username != after.username {
			names = append(names, before.username)
			if rows, err := s.db.Query(ctx, `SELECT old_username::text FROM username_redirects WHERE profile_id = $1`, pid); err == nil {
				if old, err := pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
					names = append(names, old...)
				}
			}
		}
		s.revalidate(names...)
	})
}

// revalidateImagePages queues published pages that reference an upload whose
// variants just became ready (their media map changed).
func (s *Server) revalidateImagePages(ctx context.Context, name string) {
	if s.cfg.RevalidateSecret == "" {
		return
	}
	like := "%/uploads/" + name + "%"
	rows, err := s.db.Query(ctx, `SELECT p.username::text FROM profiles p WHERE p.is_published AND (
			p.avatar_url LIKE $1 OR p.theme::text LIKE $1 OR EXISTS (
			SELECT 1 FROM blocks b WHERE b.profile_id = p.id AND b.deleted_at IS NULL AND b.data::text LIKE $1))`, like)
	if err != nil {
		return
	}
	if names, err := pgx.CollectRows(rows, pgx.RowTo[string]); err == nil {
		s.revalidate(names...)
	}
}
