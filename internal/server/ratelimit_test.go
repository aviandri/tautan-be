package server

import (
	"fmt"
	"testing"
	"time"
)

func TestRateLimiterWindowAndCap(t *testing.T) {
	clock := time.Unix(1_700_000_000, 0)
	l := newRateLimiter(2, time.Minute)
	l.now = func() time.Time { return clock }
	if !l.Allow("a") || !l.Allow("a") || l.Allow("a") {
		t.Fatal("limit of 2 not applied")
	}
	if !l.Exceeded("a") || l.Exceeded("b") {
		t.Fatal("Exceeded")
	}
	clock = clock.Add(time.Minute)
	if !l.Allow("a") || l.size() != 1 {
		t.Fatalf("window didn't reset or expired key kept: size %d", l.size())
	}

	// Rotating keys can't grow the map past maxKeys; the oldest goes first.
	l = newRateLimiter(1, time.Hour)
	l.now = func() time.Time { return clock }
	l.maxKeys = 100
	l.Allow("first")
	for i := 0; i < 10_000; i++ {
		clock = clock.Add(time.Millisecond)
		l.Allow(fmt.Sprintf("k%d", i))
	}
	if l.size() != 100 {
		t.Fatalf("size = %d, want 100", l.size())
	}
	if !l.Allow("first") {
		t.Error("evicted key should start a fresh window")
	}
	if l.Allow("k9999") {
		t.Error("newest key lost its window")
	}
}

// One IP rotating visitor ids is stopped by the per-IP ceiling, and the
// per-visitor limiter doesn't keep growing once it is (audit M1).
func TestVoteVisitorRotationBounded(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "rotator", true)
	card := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "poll", "data": map[string]any{
		"question": "Q?", "options": []map[string]any{{"id": "a", "label": "A"}, {"id": "b", "label": "B"}}}})
	s.engageIPLimiter = newRateLimiter(5, 10*time.Minute)
	codes := map[int]int{}
	for i := 0; i < 50; i++ {
		r := do(t, h, "POST", "/api/v1/public/rotator/blocks/"+card.ID+"/vote", map[string]any{"option_id": "a", "visitor_id": fmt.Sprintf("v%030d", i)})
		codes[r.Code]++
	}
	if codes[429] != 45 {
		t.Errorf("codes = %v", codes)
	}
	if n := s.engageLimiter.size(); n > 5 {
		t.Errorf("per-visitor limiter grew to %d keys past the IP ceiling", n)
	}
}
