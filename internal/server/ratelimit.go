package server

import (
	"container/list"
	"sync"
	"time"
)

// rateLimiter is a fixed-window counter per key, held in memory. Fine for a
// single instance; move to Redis/Postgres if the API is scaled out.
//
// Memory is bounded (security audit 2026-11, M1): windows sit in a list
// ordered by start time, so expired ones drop off the front in O(1) amortized,
// and once maxKeys live windows exist the oldest is evicted to make room.
// Eviction can only make a limiter more lenient for the evicted key, so
// callers pair caller-chosen key parts (visitor ids) with a per-IP ceiling
// that is checked first.
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	maxKeys int
	hits    map[string]*list.Element // value: *window
	order   *list.List               // oldest window start at the front
	now     func() time.Time
}

type window struct {
	key   string
	start time.Time
	n     int
}

// defaultLimiterKeys bounds each limiter to a few MB.
const defaultLimiterKeys = 50_000

func newRateLimiter(limit int, per time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: per, maxKeys: defaultLimiterKeys,
		hits: map[string]*list.Element{}, order: list.New(), now: time.Now}
}

// prune drops expired windows from the front. Caller holds mu.
func (l *rateLimiter) prune(now time.Time) {
	for e := l.order.Front(); e != nil; e = l.order.Front() {
		w := e.Value.(*window)
		if now.Sub(w.start) < l.window {
			return
		}
		l.order.Remove(e)
		delete(l.hits, w.key)
	}
}

func (l *rateLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(now)
	if e, ok := l.hits[key]; ok {
		w := e.Value.(*window)
		if w.n >= l.limit {
			return false
		}
		w.n++
		return true
	}
	for len(l.hits) >= l.maxKeys {
		e := l.order.Front()
		l.order.Remove(e)
		delete(l.hits, e.Value.(*window).key)
	}
	l.hits[key] = l.order.PushBack(&window{key: key, start: now, n: 1})
	return true
}

// Exceeded reports whether key is at its limit in the current window, without
// counting a hit. Pair it with Allow to count only some outcomes (e.g. failed
// logins).
func (l *rateLimiter) Exceeded(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(l.now())
	e, ok := l.hits[key]
	return ok && e.Value.(*window).n >= l.limit
}

// size is the number of live windows (tests).
func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}
