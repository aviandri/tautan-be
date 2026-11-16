package server

import (
	"encoding/json"
	"strings"
	"testing"
)

type unlockResp struct {
	Block       Block   `json:"block"`
	Children    []Block `json:"children"`
	UnlockToken string  `json:"unlock_token"`
}

func TestGates(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "locked", true)
	tok := bearer(f.tok)
	mk := func(body map[string]any) Block {
		body["tab_id"] = f.tab.ID
		return createBlock(t, h, f.tok, body)
	}
	secret := mk(map[string]any{"type": "link", "data": map[string]any{"title": "VIP", "url": "https://secret.example/vip"},
		"gate": map[string]any{"kind": "password", "password": "open sesame", "message": "Members only"}})
	if secret.Gate == nil || !secret.Gate.HasPassword || strings.Contains(toJSON(secret), "$2a$") {
		t.Fatalf("editor gate view = %+v", secret.Gate)
	}
	// Validation.
	for _, g := range []map[string]any{
		{"kind": "password"}, {"kind": "password", "password": "abc"}, {"kind": "magic"},
		{"kind": "follow"}, {"kind": "follow", "follow": map[string]any{"platform": "instagram", "url": "javascript:x"}},
		{"kind": "email", "subscribe": "always"},
	} {
		do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": f.tab.ID, "type": "text", "data": map[string]any{"content": "x"}, "gate": g}, tok).expect(400)
	}

	// Public read: stub only, nothing leaks.
	raw := do(t, h, "GET", "/api/v1/public/locked", nil).expect(200).Body.String()
	if strings.Contains(raw, "secret.example") || strings.Contains(raw, "has_password") || strings.Contains(raw, "$2a$") {
		t.Fatalf("public page leaks gated data: %s", raw)
	}
	var page pageResponse
	json.Unmarshal([]byte(raw), &page)
	for _, b := range page.Blocks {
		if b.ID == secret.ID && (!b.Locked || string(b.Data) != "null" || b.Gate == nil || b.Gate.Message != "Members only") {
			t.Errorf("stub = %+v", b)
		}
	}

	unlock := func(id string, body map[string]any) resp {
		return do(t, h, "POST", "/api/v1/public/locked/blocks/"+id+"/unlock", body)
	}
	if c := unlock(secret.ID, map[string]any{"password": "nope"}); c.Code != 403 || c.errCode() != "wrong_password" {
		t.Errorf("wrong password: %d", c.Code)
	}
	var u unlockResp
	unlock(secret.ID, map[string]any{"password": "open sesame"}).expect(200).json(&u)
	if !strings.Contains(string(u.Block.Data), "secret.example") || u.UnlockToken == "" || u.Block.Locked {
		t.Fatalf("unlock = %+v", u)
	}
	unlock(secret.ID, map[string]any{"token": u.UnlockToken}).expect(200)
	unlock(secret.ID, map[string]any{"token": u.UnlockToken + "x"}).expect(403)
	unlock(f.link.ID, map[string]any{}).expect(400) // not gated

	// Keeping the password while editing the message works; changing the gate kills old tokens.
	do(t, h, "PATCH", "/api/v1/blocks/"+secret.ID, map[string]any{"gate": map[string]any{"kind": "password", "message": "VIPs"}}, tok).expect(200)
	unlock(secret.ID, map[string]any{"token": u.UnlockToken}).expect(403)
	unlock(secret.ID, map[string]any{"password": "open sesame"}).expect(200)
	do(t, h, "PATCH", "/api/v1/blocks/"+secret.ID, map[string]any{"gate": nil}, tok).expect(200)
	if raw := do(t, h, "GET", "/api/v1/public/locked", nil).Body.String(); !strings.Contains(raw, "secret.example") {
		t.Error("removing the gate did not reveal the card")
	}

	// Email gate feeds Audience.
	mail := mk(map[string]any{"type": "text", "data": map[string]any{"content": "the recipe"}, "gate": map[string]any{"kind": "email", "subscribe": "everyone"}})
	unlock(mail.ID, map[string]any{"email": "bad"}).expect(400)
	unlock(mail.ID, map[string]any{"email": "Fan@Example.com", "name": "Fan"}).expect(200)
	var sub bool
	testPool.QueryRow(t.Context(), `SELECT subscribed FROM contacts WHERE email = 'fan@example.com'`).Scan(&sub)
	if !sub {
		t.Error("email gate did not add a subscribed contact")
	}
	// age_18 and follow are confirmations.
	adult := mk(map[string]any{"type": "text", "data": map[string]any{"content": "18+"}, "gate": map[string]any{"kind": "age_18"}})
	unlock(adult.ID, map[string]any{}).expect(400)
	unlock(adult.ID, map[string]any{"confirm_age": true}).expect(200)
	fol := mk(map[string]any{"type": "text", "data": map[string]any{"content": "bonus"}, "gate": map[string]any{"kind": "follow",
		"follow": map[string]any{"platform": "instagram", "url": "https://instagram.com/locked"}}})
	unlock(fol.ID, map[string]any{}).expect(400)
	unlock(fol.ID, map[string]any{"followed": true}).expect(200)

	// Gated folder: children are stubbed and come back on unlock.
	folder := mk(map[string]any{"type": "folder", "data": map[string]any{"title": "Secret links"}, "gate": map[string]any{"kind": "age_18"}})
	child := mk(map[string]any{"type": "link", "parent_id": folder.ID, "data": map[string]any{"title": "c", "url": "https://child.example"}})
	if raw := do(t, h, "GET", "/api/v1/public/locked", nil).Body.String(); strings.Contains(raw, "child.example") {
		t.Error("child of a gated folder leaks")
	}
	unlock(folder.ID, map[string]any{"confirm_age": true}).expect(200).json(&u)
	if len(u.Children) != 1 || u.Children[0].ID != child.ID || !strings.Contains(string(u.Children[0].Data), "child.example") {
		t.Errorf("folder unlock children = %+v", u.Children)
	}

	// Engage endpoints on a gated card need ?unlock=.
	poll := mk(map[string]any{"type": "poll", "data": map[string]any{"question": "q", "options": []map[string]any{{"id": "a", "label": "A"}, {"id": "b", "label": "B"}}},
		"gate": map[string]any{"kind": "age_18"}})
	vote := "/api/v1/public/locked/blocks/" + poll.ID + "/vote"
	if c := do(t, h, "POST", vote, map[string]any{"option_id": "a"}); c.Code != 403 || c.errCode() != "locked" {
		t.Errorf("vote on gated poll: %d", c.Code)
	}
	unlock(poll.ID, map[string]any{"confirm_age": true}).expect(200).json(&u)
	do(t, h, "POST", vote+"?unlock="+u.UnlockToken, map[string]any{"option_id": "a"}).expect(200)
	// Password guessing is rate limited.
	pw := mk(map[string]any{"type": "text", "data": map[string]any{"content": "x"}, "gate": map[string]any{"kind": "password", "password": "right-one"}})
	for i := 0; i < 10; i++ {
		unlock(pw.ID, map[string]any{"password": "guess"}).expect(403)
	}
	unlock(pw.ID, map[string]any{"password": "right-one"}).expect(429)
}
