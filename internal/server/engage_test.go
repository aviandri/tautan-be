package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cardData(t *testing.T, h httpHandler, user, typ string) map[string]any {
	t.Helper()
	var page pageResponse
	do(t, h, "GET", "/api/v1/public/"+user, nil).expect(200).json(&page)
	for _, b := range page.Blocks {
		if b.Type == typ {
			var m map[string]any
			json.Unmarshal(b.Data, &m)
			return m
		}
	}
	t.Fatalf("no %s card on /%s", typ, user)
	return nil
}

func TestPoll(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "pollster", true)
	tok := bearer(f.tok)
	ends := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	poll := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "poll", "data": map[string]any{
		"question": "Next colour?", "options": []map[string]any{{"label": "Red"}, {"id": "blue", "label": "Blue"}}, "ends_at": ends}})
	var d PollData
	json.Unmarshal(poll.Data, &d)
	if d.Options[0].ID == "" || d.Options[1].ID != "blue" || d.ShowResults != "after_vote" {
		t.Fatalf("poll = %s", poll.Data)
	}
	m := cardData(t, h, "pollster", "poll")
	if _, ok := m["results"]; ok || m["closed"] != false {
		t.Errorf("after_vote poll leaks results publicly: %v", m)
	}
	vote := func(opt, ua string) resp {
		return do(t, h, "POST", "/api/v1/public/pollster/blocks/"+poll.ID+"/vote", map[string]any{"option_id": opt}, withHeader("User-Agent", ua))
	}
	var out struct {
		Voted   string      `json:"voted"`
		Results pollResults `json:"results"`
		Error   apiError    `json:"error"`
	}
	vote("blue", "ua-1").expect(200).json(&out)
	if out.Results.Total != 1 || out.Results.Counts["blue"] != 1 {
		t.Errorf("vote = %+v", out)
	}
	vote("blue", "ua-1").expect(409).json(&out)
	if out.Error.Code != "already_voted" || out.Results.Total != 1 {
		t.Errorf("second vote = %+v", out)
	}
	vote(d.Options[0].ID, "ua-2").expect(200)
	vote("green", "ua-3").expect(400)
	var res pollResults
	do(t, h, "GET", "/api/v1/polls/"+poll.ID+"/results", nil, tok).expect(200).json(&res)
	if res.Total != 2 {
		t.Errorf("owner results = %+v", res)
	}
	// show_results: always → public read has results; never → vote hides them.
	do(t, h, "PATCH", "/api/v1/blocks/"+poll.ID, map[string]any{"data": map[string]any{"question": "Next colour?", "options": d.Options, "show_results": "always", "ends_at": ends}}, tok).expect(200)
	if m := cardData(t, h, "pollster", "poll"); m["results"] == nil {
		t.Errorf("always: no public results")
	}
	do(t, h, "PATCH", "/api/v1/blocks/"+poll.ID, map[string]any{"data": map[string]any{"question": "Next colour?", "options": d.Options, "show_results": "never", "ends_at": ends}}, tok).expect(200)
	r := vote("blue", "ua-4").expect(200)
	if strings.Contains(r.Body.String(), "results") {
		t.Errorf("never: vote returned results %s", r.Body)
	}
	// Ended.
	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if c := vote("blue", "ua-5"); c.Code != 409 || c.errCode() != "poll_ended" {
		t.Errorf("ended poll vote: %d %s", c.Code, c.Body)
	}
	if m := cardData(t, h, "pollster", "poll"); m["closed"] != true {
		t.Errorf("closed = %v", m["closed"])
	}
	// Validation.
	do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": f.tab.ID, "type": "poll", "data": map[string]any{
		"question": "q", "options": []map[string]any{{"label": "only one"}}}}, tok).expect(400)
	do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": f.tab.ID, "type": "poll", "data": map[string]any{
		"question": "q", "options": []map[string]any{{"id": "a", "label": "x"}, {"id": "a", "label": "y"}}}}, tok).expect(400)
}

func TestAskMe(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "asker", true)
	tok := bearer(f.tok)
	card := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "ask_me", "data": map[string]any{"prompt": "Ask me anything", "public_answers": true}})
	ask := func(q string) resp {
		return do(t, h, "POST", "/api/v1/public/asker/blocks/"+card.ID+"/ask", map[string]any{"question": q})
	}
	ask("What camera do you use?").expect(204)
	ask("Where are you from?").expect(204)
	ask("  ").expect(400)
	for i := 0; i < 3; i++ {
		ask("more").expect(204)
	}
	ask("too many").expect(429)

	var list struct {
		Items []Question `json:"items"`
	}
	do(t, h, "GET", "/api/v1/questions?status=unanswered&block_id="+card.ID, nil, tok).expect(200).json(&list)
	if len(list.Items) != 5 {
		t.Fatalf("questions = %d", len(list.Items))
	}
	var cam Question
	for _, q := range list.Items {
		if strings.HasPrefix(q.Question, "What camera") {
			cam = q
		}
	}
	do(t, h, "PATCH", "/api/v1/questions/"+cam.ID, map[string]any{"pinned": true}, tok).expect(400)
	var q Question
	do(t, h, "PATCH", "/api/v1/questions/"+cam.ID, map[string]any{"answer": "A Fuji X100", "pinned": true}, tok).expect(200).json(&q)
	if q.AnsweredAt == nil || !q.Pinned {
		t.Errorf("answered = %+v", q)
	}
	m := cardData(t, h, "asker", "ask_me")
	answers, _ := m["answers"].([]any)
	if len(answers) != 1 || !strings.Contains(toJSON(answers), "Fuji") {
		t.Errorf("public answers = %v", m["answers"])
	}
	do(t, h, "PATCH", "/api/v1/blocks/"+card.ID, map[string]any{"data": map[string]any{"prompt": "Ask", "public_answers": false}}, tok).expect(200)
	if m := cardData(t, h, "asker", "ask_me"); m["answers"] != nil {
		t.Errorf("answers shown with public_answers=false")
	}
	// Clearing the answer unpins; delete works and is owner-scoped.
	do(t, h, "PATCH", "/api/v1/questions/"+cam.ID, map[string]any{"answer": ""}, tok).expect(200).json(&q)
	if q.Pinned || q.AnsweredAt != nil {
		t.Errorf("cleared = %+v", q)
	}
	other := signup(t, h, "someone")
	do(t, h, "DELETE", "/api/v1/questions/"+cam.ID, nil, bearer(other.AccessToken)).expect(404)
	do(t, h, "DELETE", "/api/v1/questions/"+cam.ID, nil, tok).expect(204)
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestLeadMagnet(t *testing.T) {
	s, _ := newTestServer(t)
	s.cfg.UploadDir, s.cfg.PublicBaseURL = t.TempDir(), "http://api.test"
	h := s.Routes()
	os.WriteFile(filepath.Join(s.cfg.UploadDir, "0123456789abcdef0123456789abcdef.pdf"), []byte("%PDF-guide"), 0o644)
	f := setupPage(t, h, "magnet", true)
	card := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "lead_magnet", "data": map[string]any{
		"title": "Free guide", "file_url": "http://api.test/uploads/0123456789abcdef0123456789abcdef.pdf"}})
	page := do(t, h, "GET", "/api/v1/public/magnet", nil).expect(200).Body.String()
	if strings.Contains(page, "0123456789abcdef") {
		t.Fatal("public read exposes file_url")
	}
	do(t, h, "POST", "/api/v1/public/magnet/blocks/"+card.ID+"/lead", map[string]any{"email": "nope"}).expect(400)
	var out struct {
		DownloadURL string    `json:"download_url"`
		ExpiresAt   time.Time `json:"expires_at"`
	}
	do(t, h, "POST", "/api/v1/public/magnet/blocks/"+card.ID+"/lead", map[string]any{"email": "Reader@Example.com", "name": "Rina", "subscribe": true}).expect(200).json(&out)
	if !strings.HasPrefix(out.DownloadURL, "/api/v1/public/magnet/download/") || out.ExpiresAt.Before(time.Now()) {
		t.Fatalf("lead = %+v", out)
	}
	r := do(t, h, "GET", out.DownloadURL, nil).expect(200)
	if r.Body.String() != "%PDF-guide" || !strings.Contains(r.Header().Get("Content-Disposition"), "attachment") || r.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("download = %v %q", r.Header(), r.Body)
	}
	var sub bool
	testPool.QueryRow(t.Context(), `SELECT subscribed FROM contacts WHERE email = 'reader@example.com'`).Scan(&sub)
	if !sub {
		t.Error("lead not added to Audience as subscribed")
	}
	tampered := out.DownloadURL[:len(out.DownloadURL)-3] + "abc"
	do(t, h, "GET", tampered, nil).expect(403)
	s.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	do(t, h, "GET", out.DownloadURL, nil).expect(410)
	s.now = time.Now
	// External file: redirect after unlock.
	do(t, h, "PATCH", "/api/v1/blocks/"+card.ID, map[string]any{"data": map[string]any{"title": "Guide", "file_url": "https://files.example/guide.pdf"}}, bearer(f.tok)).expect(200)
	do(t, h, "POST", "/api/v1/public/magnet/blocks/"+card.ID+"/lead", map[string]any{"email": "b@example.com"}).expect(200).json(&out)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", out.DownloadURL, nil))
	if rec.Code != 302 || rec.Header().Get("Location") != "https://files.example/guide.pdf" {
		t.Errorf("external download = %d %v", rec.Code, rec.Header())
	}
}

func TestGiveaway(t *testing.T) {
	s, h := newTestServer(t)
	f := setupPage(t, h, "gifts", true)
	tok := bearer(f.tok)
	ends := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	card := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "giveaway", "data": map[string]any{
		"title": "Win a tee", "prize": "1 Classic Tee", "ends_at": ends, "steps": []map[string]any{{"label": "Follow us", "url": "https://instagram.com/gifts"}}}})
	enter := func(email, name string) resp {
		return do(t, h, "POST", "/api/v1/public/gifts/blocks/"+card.ID+"/enter", map[string]any{"email": email, "name": name})
	}
	var e struct {
		Already bool `json:"already_entered"`
	}
	enter("budi@example.com", "Budi Santoso").expect(200).json(&e)
	enter("BUDI@example.com", "").expect(200).json(&e)
	if !e.Already {
		t.Error("duplicate entry not detected")
	}
	enter("ani@example.com", "").expect(200)
	do(t, h, "POST", "/api/v1/giveaways/"+card.ID+"/draw", nil, tok).expect(409)
	if m := cardData(t, h, "gifts", "giveaway"); m["entry_count"] != float64(2) || m["ended"] != false || m["winner"] != nil {
		t.Errorf("open giveaway card = %v", m)
	}

	s.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if c := enter("late@example.com", ""); c.Code != 409 || c.errCode() != "giveaway_ended" {
		t.Errorf("late entry: %d", c.Code)
	}
	var draw struct {
		Entries int            `json:"entries"`
		Winner  giveawayWinner `json:"winner"`
	}
	do(t, h, "POST", "/api/v1/giveaways/"+card.ID+"/draw", nil, tok).expect(200).json(&draw)
	if draw.Entries != 2 || (draw.Winner.Display != "Budi S." && draw.Winner.Display != "an***@example.com") {
		t.Errorf("draw = %+v", draw)
	}
	m := cardData(t, h, "gifts", "giveaway")
	w, _ := m["winner"].(map[string]any)
	if m["ended"] != true || w["display"] != draw.Winner.Display || w["email"] != nil {
		t.Errorf("ended card = %v", m)
	}
	if winnerDisplay("", "x@y.z") != "x***@y.z" || winnerDisplay("Cher", "") != "Cher" {
		t.Error("winnerDisplay")
	}
}

// E2E 2026-11-18 #5: two people on one carrier-NAT IP with the same phone UA
// can each vote when the page sends its first-party visitor id, and a returning
// visitor gets their voted state from GET .../vote.
func TestPollVisitorID(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "natpoll", true)
	poll := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "poll", "data": map[string]any{
		"question": "Which colour?", "options": []map[string]any{{"id": "red", "label": "Red"}, {"id": "blue", "label": "Blue"}}}})
	path := "/api/v1/public/natpoll/blocks/" + poll.ID + "/vote"
	const ua = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)"
	vote := func(opt, vid string) resp {
		return do(t, h, "POST", path, map[string]any{"option_id": opt, "visitor_id": vid}, withHeader("User-Agent", ua))
	}
	vote("red", "visitorAAAAAAAAAAAAAAAA").expect(200)
	vote("blue", "visitorBBBBBBBBBBBBBBBB").expect(200) // same IP + UA, different person
	if c := vote("blue", "visitorAAAAAAAAAAAAAAAA"); c.Code != 409 || c.errCode() != "already_voted" {
		t.Fatalf("repeat vote by the same visitor id: %d %s", c.Code, c.Body)
	}
	// No (or a malformed) id: the old IP+UA key, so a second id-less vote is refused.
	vote("red", "").expect(200)
	if c := vote("red", "bad id!"); c.Code != 409 {
		t.Fatalf("malformed id should fall back to IP+UA: %d", c.Code)
	}

	var st struct {
		Voted   *string      `json:"voted"`
		Results *pollResults `json:"results"`
	}
	do(t, h, "GET", path+"?visitor_id=visitorAAAAAAAAAAAAAAAA", nil).expect(200).json(&st)
	if st.Voted == nil || *st.Voted != "red" || st.Results == nil || st.Results.Total != 3 {
		t.Fatalf("status for a voter = %+v", st)
	}
	st.Voted, st.Results = nil, nil
	r := do(t, h, "GET", path+"?visitor_id=visitorCCCCCCCCCCCCCCCC", nil).expect(200)
	r.json(&st)
	if st.Voted != nil || st.Results != nil { // after_vote: no results before voting
		t.Fatalf("status for a non-voter = %+v", st)
	}
	if cc := r.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("status Cache-Control = %q", cc)
	}
}

func TestAskLimitPerVisitorID(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "natask", true)
	ask := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "ask_me", "data": map[string]any{"prompt": "Ask me"}})
	path := "/api/v1/public/natask/blocks/" + ask.ID + "/ask"
	q := func(vid string) int {
		return do(t, h, "POST", path, map[string]any{"question": "Hi?", "visitor_id": vid}).Code
	}
	for i := 0; i < 5; i++ {
		if c := q("visitorAAAAAAAAAAAAAAAA"); c != 204 {
			t.Fatalf("question %d: %d", i, c)
		}
	}
	if c := q("visitorAAAAAAAAAAAAAAAA"); c != 429 {
		t.Fatalf("6th question from one visitor = %d, want 429", c)
	}
	if c := q("visitorBBBBBBBBBBBBBBBB"); c != 204 { // a neighbour on the same IP
		t.Fatalf("other visitor on the same IP = %d, want 204", c)
	}
}
