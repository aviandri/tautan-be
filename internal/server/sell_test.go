package server

import (
	"strings"
	"testing"
)

func TestSellBlockValidation(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "seller")
	tok := bearer(a.AccessToken)
	tab := editor(t, h, a.AccessToken).Tabs[0]
	ok := func(typ, data string) Block {
		t.Helper()
		var b Block
		do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"`+typ+`","data":`+data+`}`, tok).expect(201).json(&b)
		return b
	}
	bad := func(typ, data, wantMsg string) {
		t.Helper()
		r := do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"`+typ+`","data":`+data+`}`, tok)
		if r.Code != 400 || !strings.Contains(r.Body.String(), wantMsg) {
			t.Errorf("%s %s: got %d %s (want 400 containing %q)", typ, data, r.Code, r.Body, wantMsg)
		}
	}

	// product
	b := ok("product", `{"images":["/uploads/a.webp","https://cdn.example/b.jpg"],"name":"Kaos","price":15000000,"compare_at":20000000,
		"badge":"pre_order","variants":[{"name":"Size","options":["S","M"]}],"action":{"kind":"whatsapp","wa_number":"+62 812-3456-7890","wa_template":"Halo, mau {name}","url":"https://drop.me"}}`)
	if !strings.Contains(string(b.Data), `"wa_number":"+6281234567890"`) || strings.Contains(string(b.Data), "drop.me") || !strings.Contains(string(b.Data), `"currency":"IDR"`) {
		t.Errorf("product not normalised: %s", b.Data)
	}
	ok("product", `{"name":"Mug","currency":"USD","action":{"kind":"marketplace","marketplace":"shopee","url":"https://shopee.co.id/x"}}`)
	bad("product", `{"name":"x","price":1.5,"action":{"kind":"url","url":"https://x.example"}}`, "shape")
	bad("product", `{"name":"x","price":-1,"action":{"kind":"url","url":"https://x.example"}}`, "price")
	bad("product", `{"name":"x","price":1,"currency":"EUR","action":{"kind":"url","url":"https://x.example"}}`, "currency")
	bad("product", `{"name":"x","price":100,"compare_at":100,"action":{"kind":"url","url":"https://x.example"}}`, "compare_at")
	bad("product", `{"name":"x","compare_at":100,"action":{"kind":"url","url":"https://x.example"}}`, "compare_at")
	bad("product", `{"name":"x","badge":"hot","action":{"kind":"url","url":"https://x.example"}}`, "badge")
	bad("product", `{"name":"x","action":{"kind":"url","url":"javascript:alert(1)"}}`, "http(s)")
	bad("product", `{"name":"x","action":{"kind":"url","url":"mailto:a@b.c"}}`, "http(s)")
	bad("product", `{"name":"x","action":{"kind":"whatsapp","wa_number":"081234567890"}}`, "E.164")
	bad("product", `{"name":"x","action":{"kind":"marketplace","marketplace":"ebay","url":"https://ebay.com"}}`, "marketplace")
	bad("product", `{"name":"x","action":{"kind":"sms"}}`, "kind")
	bad("product", `{"name":"x","images":["1","2","3","4","5","6","7","8","9"],"action":{"kind":"url","url":"https://x.example"}}`, "at most 8")
	bad("product", `{"name":"x","variants":[{"name":"Size","options":[]}],"action":{"kind":"url","url":"https://x.example"}}`, "options")

	// Field paths in error details.
	r := do(t, h, "POST", "/api/v1/blocks", `{"tab_id":"`+tab.ID+`","type":"shop","data":{"items":[
		{"id":"a","name":"A","action":{"kind":"url","url":"https://x.example"}},
		{"id":"b","name":"B","price":-5,"action":{"kind":"url","url":"https://x.example"}}]}}`, tok).expect(400)
	var eb struct {
		Error apiError `json:"error"`
	}
	r.json(&eb)
	if eb.Error.Code != "invalid_data" || eb.Error.Details["items.1.price"] == "" {
		t.Errorf("details = %+v", eb.Error)
	}

	// shop
	b = ok("shop", `{"title":"Toko","categories":["Kaos","Topi"],"items":[{"id":"kaos-1","name":"A","price":100,"category":"Kaos","action":{"kind":"url","url":"https://x.example"},"schedule":{"start_at":"2030-01-01T00:00:00Z"}}]}`)
	if strings.Contains(string(b.Data), "2030") || !strings.Contains(string(b.Data), `"id":"kaos-1"`) {
		t.Errorf("shop item: %s", b.Data)
	}
	bad("shop", `{}`, "items")
	bad("shop", `{"categories":["Kaos"],"items":[{"id":"a","name":"A","category":"Sepatu","action":{"kind":"url","url":"https://x.example"}}]}`, "category")
	bad("shop", `{"categories":["Kaos","Kaos"],"items":[{"id":"a","name":"A","action":{"kind":"url","url":"https://x.example"}}]}`, "duplicate")
	bad("shop", `{"items":[{"name":"A","action":{"kind":"url","url":"https://x.example"}}]}`, "items[0].id")
	bad("shop", `{"items":[{"id":"a","name":"A","action":{"kind":"url","url":"https://x.example"}},{"id":"a","name":"B","action":{"kind":"url","url":"https://x.example"}}]}`, "items[1].id")
	bad("shop", `{"items":[{"id":"a","name":"","action":{"kind":"url","url":"https://x.example"}}]}`, "items[0].name")
	b = ok("product", `{"id":"x","category":"y","name":"P","action":{"kind":"url","url":"https://x.example"}}`)
	if strings.Contains(string(b.Data), `"category"`) {
		t.Errorf("standalone product keeps shop-only fields: %s", b.Data)
	}

	// marketplace
	ok("marketplace", `{"title":"Beli di","stores":[{"platform":"tokopedia","url":"https://tokopedia.com/x"},{"platform":"tiktok_shop","url":"https://shop.tiktok.com/x"}]}`)
	bad("marketplace", `{"stores":[]}`, "stores")
	bad("marketplace", `{"stores":[{"platform":"Shopee","url":"https://shopee.co.id"}]}`, "platform")
	bad("marketplace", `{"stores":[{"platform":"shopee","url":"https://shopee.co.id"},{"platform":"shopee","url":"https://shopee.sg"}]}`, "duplicate")
	bad("marketplace", `{"stores":[{"platform":"shopee","url":"/uploads/x"}]}`, "http(s)")

	// whatsapp
	b = ok("whatsapp", `{"label":"Order","number":"+6281234567890","message_template":"Halo {name}: {items}","order_form":{"fields":["name","items","address"],"items":[{"name":"Kaos","price":100},{"name":"Topi"}]}}`)
	if !strings.Contains(string(b.Data), `"fields":["name","items","address"]`) {
		t.Errorf("whatsapp = %s", b.Data)
	}
	bad("whatsapp", `{"label":"Order","number":"6281234567890","message_template":"x"}`, "E.164")
	bad("whatsapp", `{"label":"Order","number":"+6281234567890"}`, "message_template")
	bad("whatsapp", `{"label":"","number":"+6281234567890","message_template":"x"}`, "label")
	bad("whatsapp", `{"label":"x","number":"+6281234567890","message_template":"x","order_form":{"fields":["qty"]}}`, "order_form.fields[0]")
	bad("whatsapp", `{"label":"x","number":"+6281234567890","message_template":"x","order_form":{"fields":["name","name"]}}`, "duplicate")
	bad("whatsapp", `{"label":"x","number":"+6281234567890","message_template":"x","order_form":{"fields":["items"]}}`, "order_form.items")

	// price_list
	ok("price_list", `{"sections":[{"title":"Potong","items":[{"name":"Pria","price":5000000,"price_from":true,"image_url":"/uploads/p.webp"},{"name":"Konsultasi"}]}],"ask":{"kind":"whatsapp","target":"+6281234567890","label":"Tanya"}}`)
	bad("price_list", `{"sections":[]}`, "sections")
	bad("price_list", `{"sections":[{"title":"x","items":[{"name":"a"}]}],"ask":{"kind":"form","target":"x"}}`, "ask.kind")
	bad("price_list", `{"sections":[{"title":"x","items":[{"name":"a","price":-5}]}]}`, "price")

	// store_info
	ok("store_info", `{"tz":"Asia/Jakarta","hours":[{"day":"mon","open":"09:00","close":"17:00"},{"day":"fri","open":"18:00","close":"02:00"},{"day":"sat","open":"00:00","close":"24:00"}],"couriers":["jne","gosend"],"address":"Jl. Melati 3","area":"Bandung"}`)
	bad("store_info", `{"hours":[]}`, "tz")
	bad("store_info", `{"tz":"Mars/Base"}`, "tz")
	bad("store_info", `{"tz":"UTC","hours":[{"day":"monday","open":"09:00","close":"17:00"}]}`, "day")
	bad("store_info", `{"tz":"UTC","hours":[{"day":"mon","open":"9:00","close":"17:00"}]}`, "HH:MM")
	bad("store_info", `{"tz":"UTC","hours":[{"day":"mon","open":"24:00","close":"17:00"}]}`, "HH:MM")
	bad("store_info", `{"tz":"UTC","couriers":["JNE"]}`, "courier")

	// payment
	b = ok("payment", `{"qris_image_url":"/uploads/qris.png","accounts":[{"bank":"BCA","name":"Budi","number":"123 456 7890"}],"confirm":{"kind":"whatsapp","target":"+62 812 3456 7890"}}`)
	if !strings.Contains(string(b.Data), `"target":"+6281234567890"`) {
		t.Errorf("payment confirm not normalised: %s", b.Data)
	}
	bad("payment", `{"accounts":[{"bank":"GoPay","name":"Budi","number":"081234567890"}],"confirm":{"kind":"form","target":"`+tab.ID+`"}}`, "form block on this page")
	bad("payment", `{}`, "qris_image_url or at least one account")
	bad("payment", `{"qris_image_url":"javascript:x"}`, "http(s)")
	bad("payment", `{"qris_image_url":"/uploads/q.png","confirm":{"kind":"email","target":"x"}}`, "confirm.kind")

	// tip_jar
	ok("tip_jar", `{"title":"Traktir kopi","amounts":[1000000,2500000],"platforms":[{"platform":"saweria","url":"https://saweria.co/x"},{"platform":"kofi","url":"https://ko-fi.com/x"}]}`)
	bad("tip_jar", `{"title":"x","currency":"JPY","platforms":[{"platform":"saweria","url":"https://saweria.co/x"}]}`, "currency")
	bad("tip_jar", `{"title":"x","platforms":[]}`, "platforms")
	bad("tip_jar", `{"title":"x","platforms":[{"platform":"patreon","url":"https://patreon.com/x"}]}`, "platform")
	bad("tip_jar", `{"title":"x","amounts":[0],"platforms":[{"platform":"saweria","url":"https://saweria.co/x"}]}`, "amounts[0]")

	// reviews
	ok("reviews", `{"average":4.8,"count":120,"distribution":[100,15,3,1,1],"items":[{"stars":5,"quote":"Mantap","author":"Ani","source_url":"https://maps.google.com/x"}]}`)
	ok("reviews", `{"average":4.5}`)
	bad("reviews", `{"items":[]}`, "average or at least one item")
	bad("reviews", `{"average":5.1}`, "average")
	bad("reviews", `{"average":4,"distribution":[1,2,3]}`, "distribution")
	bad("reviews", `{"items":[{"stars":6,"quote":"x","author":"a"}]}`, "stars")
	bad("reviews", `{"items":[{"stars":5,"quote":"x"}]}`, "author")
	bad("reviews", `{"items":[{"stars":5,"quote":"x","author":"a","source_url":"ftp://x"}]}`, "http(s)")
}

func TestSellClickActions(t *testing.T) {
	_, h := newTestServer(t)
	f := setupPage(t, h, "toko", true)
	tok := bearer(f.tok)
	var p Block
	do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": f.tab.ID, "type": "product", "data": map[string]any{
		"name": "Kaos", "price": 100, "action": map[string]any{"kind": "url", "url": "https://x.example"}}}, tok).expect(201).json(&p)

	// Public read returns the card.
	r := do(t, h, "GET", "/api/v1/public/toko", nil).expect(200)
	if !strings.Contains(r.Body.String(), `"type":"product"`) {
		t.Fatalf("product missing from public page: %s", r.Body)
	}

	ev := func(body map[string]any) resp {
		return do(t, h, "POST", "/api/v1/public/toko/events", body, withHeader("User-Agent", "Mozilla/5.0 test"))
	}
	ev(map[string]any{"kind": "click", "block_id": p.ID, "meta": map[string]any{"action": "buy"}}).expect(204)
	ev(map[string]any{"kind": "click", "block_id": p.ID, "meta": map[string]any{"action": "buy"}}).expect(204)
	ev(map[string]any{"kind": "click", "block_id": p.ID, "meta": map[string]any{"action": "marketplace:shopee"}}).expect(204)
	ev(map[string]any{"kind": "click", "block_id": p.ID}).expect(204)
	if c := ev(map[string]any{"kind": "view", "meta": map[string]any{"action": "buy"}}); c.Code != 400 || c.errCode() != "invalid_action" {
		t.Errorf("view with action: %d %s", c.Code, c.Body)
	}
	for _, a := range []string{"BUY", "buy now", "x:" + strings.Repeat("a", 40), "<b>"} {
		if c := ev(map[string]any{"kind": "click", "block_id": p.ID, "meta": map[string]any{"action": a}}); c.Code != 400 {
			t.Errorf("action %q accepted", a)
		}
	}

	var out struct {
		Items []blockStat `json:"items"`
	}
	do(t, h, "GET", "/api/v1/analytics/blocks?range=7d", nil, tok).expect(200).json(&out)
	var got *blockStat
	for i := range out.Items {
		if out.Items[i].BlockID == p.ID {
			got = &out.Items[i]
		}
		if out.Items[i].Actions == nil {
			t.Errorf("actions should be {} not null for %s", out.Items[i].BlockID)
		}
	}
	if got == nil || got.Clicks != 4 || got.Title != "Kaos" || got.Actions["buy"] != 2 || got.Actions["marketplace:shopee"] != 1 || len(got.Actions) != 2 {
		t.Fatalf("block stat = %+v", got)
	}

	var series struct {
		Clicks int `json:"clicks"`
	}
	do(t, h, "GET", "/api/v1/analytics/blocks/"+p.ID+"/series?range=7d&action=buy", nil, tok).expect(200).json(&series)
	if series.Clicks != 2 {
		t.Errorf("series with action=buy clicks = %d", series.Clicks)
	}
}
