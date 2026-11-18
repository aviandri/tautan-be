package server

import (
	"strings"
	"testing"
	"time"
)

func TestGiveawayWinnerEmail(t *testing.T) {
	s, h, m, clock := mailServer(t)
	s.cfg.AppBaseURL = "http://app.test"
	f := setupPage(t, h, "prizes", true)
	tok := bearer(f.tok)
	card := createBlock(t, h, f.tok, map[string]any{"tab_id": f.tab.ID, "type": "giveaway", "data": map[string]any{
		"title": "Win a tee", "prize": "1 Classic Tee", "ends_at": clock.Add(time.Minute).UTC().Format(time.RFC3339)}})
	do(t, h, "POST", "/api/v1/public/prizes/blocks/"+card.ID+"/enter", map[string]any{"email": "Solo@Example.com", "name": "Solo"}).expect(200)
	*clock = clock.Add(2 * time.Minute)
	deliver(t, s)
	m.take()
	type drawResp struct {
		Winner   giveawayWinner `json:"winner"`
		Notified bool           `json:"notified"`
	}
	draw := func(body any) drawResp {
		var d drawResp
		do(t, h, "POST", "/api/v1/giveaways/"+card.ID+"/draw", body, tok).expect(200).json(&d)
		return d
	}
	setVerified := func(v bool) {
		if _, err := testPool.Exec(t.Context(), `UPDATE users SET email_verified_at = CASE WHEN $2 THEN now() END
			WHERE id = (SELECT user_id FROM profiles WHERE username = $1)`, "prizes", v); err != nil {
			t.Fatal(err)
		}
	}

	// Unconfirmed owner: drawn, not emailed.
	setVerified(false)
	if d := draw(nil); d.Notified || d.Winner.Email != "solo@example.com" {
		t.Errorf("unverified draw = %+v", d)
	}
	setVerified(true)
	// Opting out.
	if draw(map[string]any{"notify": false}).Notified {
		t.Error("notified with notify=false")
	}
	// Default: emailed.
	if !draw(nil).Notified {
		t.Fatal("not notified")
	}
	deliver(t, s)
	msgs := m.take()
	if len(msgs) != 1 || msgs[0].To != "solo@example.com" || msgs[0].Subject != "You won: Win a tee (prizes)" || msgs[0].ReplyTo != "" {
		t.Fatalf("emails = %+v", msgs)
	}
	if !strings.Contains(msgs[0].Text, "1 Classic Tee") || !strings.Contains(msgs[0].Text, "http://app.test/prizes") {
		t.Errorf("text = %q", msgs[0].Text)
	}
	// One address gets at most 2 a day; a card sends at most 5 a day.
	if !draw(map[string]any{"notify": true}).Notified {
		t.Error("2nd email refused")
	}
	if draw(nil).Notified {
		t.Error("3rd email to the same winner in a day")
	}
	s.winnerCardLimiter = newRateLimiter(1, time.Hour)
	s.winnerCardLimiter.Allow(card.ID)
	if _, err := testPool.Exec(t.Context(), `DELETE FROM email_outbox WHERE to_email = 'solo@example.com'`); err != nil {
		t.Fatal(err) // only the card cap is left in the way
	}
	if draw(nil).Notified {
		t.Error("card cap not applied")
	}
	do(t, h, "POST", "/api/v1/giveaways/"+card.ID+"/draw", map[string]any{"notify": "yes"}, tok).expect(400)
}
