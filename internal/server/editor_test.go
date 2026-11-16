package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

type httpHandler = http.Handler

func editor(t *testing.T, h httpHandler, tok string) pageResponse {
	t.Helper()
	var p pageResponse
	do(t, h, "GET", "/api/v1/profile/editor", nil, bearer(tok)).expect(200).json(&p)
	return p
}

func createBlock(t *testing.T, h httpHandler, tok string, body map[string]any) Block {
	t.Helper()
	var b Block
	do(t, h, "POST", "/api/v1/blocks", body, bearer(tok)).expect(201).json(&b)
	return b
}

func TestPatchProfile(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	signup(t, h, "bob")
	tok := bearer(a.AccessToken)

	var p Profile
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{
		"display_name": "Alice A", "bio": "hi", "avatar_url": "https://cdn.example.com/a.png",
		"theme": map[string]any{"preset": "dusk"}, "seo": map[string]any{"title": "Alice", "junk": 1},
		"is_published": true,
	}, tok).expect(200).json(&p)
	if p.DisplayName != "Alice A" || p.Bio != "hi" || p.AvatarURL == nil || !p.IsPublished {
		t.Fatalf("patch not applied: %+v", p)
	}
	if !jsonEq(p.SEO, `{"title":"Alice"}`) {
		t.Errorf("seo = %s", p.SEO)
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"avatar_url": nil}, tok).expect(200).json(&p)
	if p.AvatarURL != nil || p.DisplayName != "Alice A" {
		t.Errorf("avatar should be cleared, others untouched: %+v", p)
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"username": "Alice2"}, tok).expect(200).json(&p)
	if p.Username != "alice2" {
		t.Errorf("username = %s", p.Username)
	}
	for body, code := range map[string]string{
		`{"username":"bob"}`:                   "username_taken",
		`{"username":"api"}`:                   "invalid_username",
		`{"theme":"dark"}`:                     "invalid_theme",
		`{"avatar_url":"javascript:alert(1)"}`: "invalid_avatar_url",
		`{"is_published":"yes"}`:               "invalid_is_published",
	} {
		r := do(t, h, "PATCH", "/api/v1/profile", body, tok)
		if r.errCode() != code {
			t.Errorf("%s: got %d %s, want %s", body, r.Code, r.Body, code)
		}
	}
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"bio": "x"}).expect(401)
}

func TestTabs(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	first := editor(t, h, a.AccessToken).Tabs[0]

	var shop, shop2 Tab
	do(t, h, "POST", "/api/v1/tabs", map[string]any{"title": "My Shop!"}, tok).expect(201).json(&shop)
	if shop.Slug != "my-shop" || shop.Position != 1 {
		t.Fatalf("tab = %+v", shop)
	}
	do(t, h, "POST", "/api/v1/tabs", map[string]any{"title": "My shop"}, tok).expect(201).json(&shop2)
	if shop2.Slug != "my-shop-2" {
		t.Errorf("auto slug collision: %s", shop2.Slug)
	}
	if do(t, h, "POST", "/api/v1/tabs", map[string]any{"title": "X", "slug": "my-shop"}, tok).expect(409).errCode() != "slug_taken" {
		t.Error("want slug_taken")
	}
	do(t, h, "POST", "/api/v1/tabs", map[string]any{"title": "X", "slug": "Bad Slug"}, tok).expect(400)
	do(t, h, "POST", "/api/v1/tabs", map[string]any{"title": "  "}, tok).expect(400)

	var patched Tab
	do(t, h, "PATCH", "/api/v1/tabs/"+shop2.ID, map[string]any{"title": "Store", "slug": "store"}, tok).expect(200).json(&patched)
	if patched.Title != "Store" || patched.Slug != "store" {
		t.Errorf("patched = %+v", patched)
	}
	do(t, h, "PATCH", "/api/v1/tabs/"+shop2.ID, map[string]any{"slug": "my-shop"}, tok).expect(409)
	do(t, h, "PATCH", "/api/v1/tabs/not-a-uuid", map[string]any{"title": "x"}, tok).expect(404)

	// Another user cannot touch alice's tabs.
	b := signup(t, h, "bob")
	do(t, h, "PATCH", "/api/v1/tabs/"+shop.ID, map[string]any{"title": "pwned"}, bearer(b.AccessToken)).expect(404)
	do(t, h, "DELETE", "/api/v1/tabs/"+shop.ID, nil, bearer(b.AccessToken)).expect(404)

	// Reorder.
	do(t, h, "PUT", "/api/v1/tabs/order", map[string]any{"ids": []string{shop2.ID, first.ID}}, tok).expect(400)
	do(t, h, "PUT", "/api/v1/tabs/order", map[string]any{"ids": []string{shop2.ID, first.ID, shop.ID}}, tok).expect(204)
	tabs := editor(t, h, a.AccessToken).Tabs
	if tabs[0].ID != shop2.ID || tabs[1].ID != first.ID || tabs[2].ID != shop.ID {
		t.Errorf("order wrong: %+v", tabs)
	}

	// Delete cascades blocks; last tab is protected.
	createBlock(t, h, a.AccessToken, map[string]any{"tab_id": shop.ID, "type": "text", "data": map[string]any{"content": "hi"}})
	do(t, h, "DELETE", "/api/v1/tabs/"+shop.ID, nil, tok).expect(204)
	do(t, h, "DELETE", "/api/v1/tabs/"+shop2.ID, nil, tok).expect(204)
	if code := do(t, h, "DELETE", "/api/v1/tabs/"+first.ID, nil, tok).expect(400).errCode(); code != "last_tab" {
		t.Errorf("code = %s", code)
	}
	if p := editor(t, h, a.AccessToken); len(p.Tabs) != 1 || len(p.Blocks) != 0 {
		t.Errorf("after deletes: %d tabs %d blocks", len(p.Tabs), len(p.Blocks))
	}
}

func TestBlocksCRUD(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	tab := editor(t, h, a.AccessToken).Tabs[0]

	l1 := createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "link", "data": map[string]any{"title": "Site", "url": "https://a.example", "extra": true}})
	if l1.Position != 0 || !l1.IsVisible || l1.ParentID != nil || !jsonEq(l1.Data, `{"title":"Site","url":"https://a.example"}`) {
		t.Fatalf("block = %+v data=%s", l1, l1.Data)
	}
	txt := createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "text", "data": map[string]any{"content": "hello"}})
	if txt.Position != 1 {
		t.Errorf("position = %d", txt.Position)
	}
	faq := createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "faq", "data": map[string]any{}})
	if !jsonEq(faq.Data, `{"items":[]}`) {
		t.Errorf("faq data = %s", faq.Data)
	}

	for name, body := range map[string]map[string]any{
		"unknown type":   {"tab_id": tab.ID, "type": "carousel", "data": map[string]any{}},
		"js url":         {"tab_id": tab.ID, "type": "link", "data": map[string]any{"title": "x", "url": "javascript:alert(1)"}},
		"missing url":    {"tab_id": tab.ID, "type": "video", "data": map[string]any{}},
		"wrong shape":    {"tab_id": tab.ID, "type": "faq", "data": map[string]any{"items": "nope"}},
		"bad tab":        {"tab_id": "00000000-0000-0000-0000-000000000000", "type": "text", "data": map[string]any{"content": "x"}},
		"malformed tab":  {"tab_id": "zzz", "type": "text", "data": map[string]any{"content": "x"}},
		"parent not dir": {"tab_id": tab.ID, "type": "link", "parent_id": txt.ID, "data": map[string]any{"title": "x", "url": "https://x.example"}},
	} {
		if r := do(t, h, "POST", "/api/v1/blocks", body, tok); r.Code != 400 {
			t.Errorf("%s: status %d %s", name, r.Code, r.Body)
		}
	}

	var upd Block
	do(t, h, "PATCH", "/api/v1/blocks/"+l1.ID, map[string]any{"is_visible": false, "data": map[string]any{"title": "New", "url": "mailto:a@b.c"}}, tok).expect(200).json(&upd)
	if upd.IsVisible || !jsonEq(upd.Data, `{"title":"New","url":"mailto:a@b.c"}`) || upd.Position != 0 {
		t.Errorf("patched = %+v %s", upd, upd.Data)
	}
	do(t, h, "PATCH", "/api/v1/blocks/"+l1.ID, map[string]any{"data": map[string]any{"title": "x"}}, tok).expect(400)

	b := signup(t, h, "bob")
	do(t, h, "PATCH", "/api/v1/blocks/"+l1.ID, map[string]any{"is_visible": true}, bearer(b.AccessToken)).expect(404)
	do(t, h, "DELETE", "/api/v1/blocks/"+l1.ID, nil, bearer(b.AccessToken)).expect(404)

	do(t, h, "DELETE", "/api/v1/blocks/"+l1.ID, nil, tok).expect(204)
	do(t, h, "DELETE", "/api/v1/blocks/"+l1.ID, nil, tok).expect(404)
	if n := len(editor(t, h, a.AccessToken).Blocks); n != 2 {
		t.Errorf("blocks = %d", n)
	}
}

func TestFoldersAndOrdering(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "alice")
	tok := bearer(a.AccessToken)
	tab := editor(t, h, a.AccessToken).Tabs[0]
	var tab2 Tab
	do(t, h, "POST", "/api/v1/tabs", map[string]any{"title": "Two"}, tok).expect(201).json(&tab2)

	link := func(title string, parent string) Block {
		body := map[string]any{"tab_id": tab.ID, "type": "link", "data": map[string]any{"title": title, "url": "https://x.example/" + title}}
		if parent != "" {
			body["parent_id"] = parent
		}
		return createBlock(t, h, a.AccessToken, body)
	}
	top := link("top", "")
	folder := createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "folder", "data": map[string]any{"title": "F"}})
	c1, c2 := link("c1", folder.ID), link("c2", folder.ID)
	if c1.Position != 0 || c2.Position != 1 || *c1.ParentID != folder.ID {
		t.Fatalf("children positions: %d %d", c1.Position, c2.Position)
	}
	// Only links can go in folders; folders don't nest.
	do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": tab.ID, "type": "folder", "parent_id": folder.ID, "data": map[string]any{"title": "x"}}, tok).expect(400)

	// Reorder top level and folder children separately.
	do(t, h, "PUT", "/api/v1/blocks/order", map[string]any{"tab_id": tab.ID, "ids": []string{folder.ID, top.ID}}, tok).expect(204)
	do(t, h, "PUT", "/api/v1/blocks/order", map[string]any{"tab_id": tab.ID, "ids": []string{c2.ID, c1.ID}}, tok).expect(204)
	pos := map[string]int{}
	for _, b := range editor(t, h, a.AccessToken).Blocks {
		pos[b.ID] = b.Position
	}
	if pos[folder.ID] != 0 || pos[top.ID] != 1 || pos[c2.ID] != 0 || pos[c1.ID] != 1 {
		t.Errorf("positions = %v", pos)
	}
	// Mixed groups / incomplete sets are rejected.
	do(t, h, "PUT", "/api/v1/blocks/order", map[string]any{"tab_id": tab.ID, "ids": []string{folder.ID, c1.ID}}, tok).expect(400)
	do(t, h, "PUT", "/api/v1/blocks/order", map[string]any{"tab_id": tab.ID, "ids": []string{folder.ID}}, tok).expect(400)
	do(t, h, "PUT", "/api/v1/blocks/order", map[string]any{"tab_id": tab2.ID, "ids": []string{folder.ID, top.ID}}, tok).expect(400)

	// Move top-level link into the folder: goes to the end of the children.
	var moved Block
	do(t, h, "PATCH", "/api/v1/blocks/"+top.ID, map[string]any{"parent_id": folder.ID}, tok).expect(200).json(&moved)
	if moved.ParentID == nil || moved.Position != 2 {
		t.Errorf("moved = %+v", moved)
	}
	// And back out to top level.
	do(t, h, "PATCH", "/api/v1/blocks/"+top.ID, map[string]any{"parent_id": nil}, tok).expect(200).json(&moved)
	if moved.ParentID != nil || moved.Position != 1 {
		t.Errorf("moved out = %+v", moved)
	}
	// A folder can't be put inside a folder or itself.
	do(t, h, "PATCH", "/api/v1/blocks/"+folder.ID, map[string]any{"parent_id": folder.ID}, tok).expect(400)

	// Moving a folder to another tab moves its children.
	do(t, h, "PATCH", "/api/v1/blocks/"+folder.ID, map[string]any{"tab_id": tab2.ID}, tok).expect(200)
	for _, b := range editor(t, h, a.AccessToken).Blocks {
		if (b.ID == c1.ID || b.ID == c2.ID || b.ID == folder.ID) && b.TabID != tab2.ID {
			t.Errorf("block %s still in old tab", b.ID)
		}
	}
	// Moving a child to another tab without parent_id detaches it.
	do(t, h, "PATCH", "/api/v1/blocks/"+c1.ID, map[string]any{"tab_id": tab.ID}, tok).expect(200).json(&moved)
	if moved.ParentID != nil || moved.TabID != tab.ID {
		t.Errorf("detached = %+v", moved)
	}

	// Deleting a folder deletes its children.
	do(t, h, "DELETE", "/api/v1/blocks/"+folder.ID, nil, tok).expect(204)
	for _, b := range editor(t, h, a.AccessToken).Blocks {
		if b.ID == c2.ID {
			t.Error("child survived folder delete")
		}
	}
}

func jsonEq(a json.RawMessage, b string) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
