package server

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// cartShop adds a shop card: a tee (ships, sizes), a sticker (ships), a cheap
// badge (ships) and an e-book (download), with a flat shipping fee.
func cartShop(t *testing.T, fx shopFixture) Block {
	t.Helper()
	tab := editor(t, fx.h, fx.tok).Tabs[0]
	ship := map[string]any{"kind": "checkout", "delivery": "ship", "download": map[string]any{"url": "https://files.example/ignored.zip"}}
	return createBlock(t, fx.h, fx.tok, map[string]any{"tab_id": tab.ID, "type": "shop", "data": map[string]any{
		"title": "Merch", "shipping_fee": 2000000, "items": []map[string]any{
			{"id": "tee", "name": "Tee", "price": 15000000, "currency": "IDR", "images": []string{}, "action": ship,
				"variants": []map[string]any{{"name": "Size", "options": []string{"S", "M"}}}},
			{"id": "sticker", "name": "Sticker", "price": 2500000, "currency": "IDR", "images": []string{}, "action": ship},
			{"id": "badge", "name": "Badge", "price": 300000, "currency": "IDR", "images": []string{}, "action": ship},
			{"id": "ebook", "name": "Zine PDF", "price": 5000000, "currency": "IDR", "images": []string{},
				"action": map[string]any{"kind": "checkout", "download": map[string]any{"url": "https://files.example/zine.pdf"}}},
		}}})
}

func address() map[string]any {
	return map[string]any{"name": "Budi Santoso", "phone": "0812-3456-7890", "address": "Jl. Melati 7", "city": "Bandung", "postal_code": "40115"}
}

func TestShopCartValidation(t *testing.T) {
	fx := newShop(t)
	tab := editor(t, fx.h, fx.tok).Tabs[0]
	shop := func(items []map[string]any, extra map[string]any) resp {
		data := map[string]any{"items": items}
		for k, v := range extra {
			data[k] = v
		}
		return do(t, fx.h, "POST", "/api/v1/blocks", map[string]any{"tab_id": tab.ID, "type": "shop", "data": data}, bearer(fx.tok))
	}
	item := func(id, cur string, price int) map[string]any {
		return map[string]any{"id": id, "name": id, "price": price, "currency": cur, "images": []string{},
			"action": map[string]any{"kind": "checkout", "delivery": "ship"}}
	}
	// One cart, one currency.
	r := shop([]map[string]any{item("a", "IDR", 100000), item("b", "USD", 500)}, nil).expect(400)
	if !strings.Contains(r.Body.String(), "items[1].currency") {
		t.Fatalf("mixed currency = %s", r.Body)
	}
	shop([]map[string]any{item("a", "IDR", 100000)}, map[string]any{"shipping_fee": 150}).expect(400) // sen
	shop([]map[string]any{item("a", "IDR", 100000)}, map[string]any{"shipping_fee": -100}).expect(400)
	bad := item("a", "IDR", 100000)
	bad["action"] = map[string]any{"kind": "checkout", "delivery": "drone"}
	shop([]map[string]any{bad}, nil).expect(400)
	// A ship item needs no download, and any download sent is dropped.
	b := cartShop(t, fx)
	if strings.Contains(string(b.Data), "ignored.zip") || !strings.Contains(string(b.Data), `"delivery":"ship"`) {
		t.Fatalf("ship item data = %s", b.Data)
	}
	// Physical goods are a shop feature; a single product card can't ship.
	r = do(t, fx.h, "POST", "/api/v1/blocks", map[string]any{"tab_id": tab.ID, "type": "product", "data": map[string]any{
		"name": "Mug", "price": 5000000, "images": []string{}, "action": map[string]any{"kind": "checkout", "delivery": "ship"}}}, bearer(fx.tok)).expect(400)
	if !strings.Contains(r.Body.String(), "action.delivery") {
		t.Fatalf("product ship = %s", r.Body)
	}
	// Free shipping (0) is stored as no fee.
	r = shop([]map[string]any{item("a", "IDR", 100000)}, map[string]any{"shipping_fee": 0}).expect(201)
	if strings.Contains(r.Body.String(), "shipping_fee") {
		t.Fatalf("free shipping = %s", r.Body)
	}
}

func TestShopCartCheckout(t *testing.T) {
	fx := newShop(t)
	h := fx.h
	mailer := &captureMailer{}
	fx.s.mailer = mailer
	_, _ = fx.s.DeliverPending(context.Background())
	mailer.take()
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	shop := cartShop(t, fx)
	post := func(body map[string]any) resp {
		return do(t, h, "POST", "/api/v1/public/seller/orders", body)
	}
	cart := func(lines ...map[string]any) map[string]any {
		return map[string]any{"block_id": shop.ID, "email": "buyer@example.com", "name": "Budi", "items": lines, "shipping": address()}
	}
	tee := func(q int, size string) map[string]any {
		return map[string]any{"item_id": "tee", "quantity": q, "options": map[string]string{"Size": size}}
	}

	// Errors, each with a field.
	cases := []struct {
		body        map[string]any
		code, field string
	}{
		{cart(tee(2, "XL")), "invalid_options", "items[0].options"},
		{cart(tee(21, "M")), "invalid_quantity", "items[0].quantity"},
		{cart(tee(15, "M"), tee(6, "M")), "invalid_quantity", "items[1].quantity"}, // merged past 20
		{cart(map[string]any{"item_id": "ebook", "quantity": 2}), "invalid_quantity", "items[0].quantity"},
		{cart(map[string]any{"item_id": "nope"}), "not_for_sale", "items[0]"},
	}
	for _, c := range cases {
		r := post(c.body).expect(400)
		if r.errCode() != c.code || !strings.Contains(r.Body.String(), `"`+c.field+`"`) {
			t.Fatalf("%v: %s", c.body["items"], r.Body)
		}
	}
	many := make([]map[string]any, 21)
	for i := range many {
		many[i] = map[string]any{"item_id": "sticker"}
	}
	if r := post(cart(many...)).expect(400); r.errCode() != "invalid_items" {
		t.Fatalf("21 lines = %s", r.Body)
	}
	both := cart(tee(1, "M"))
	both["item_id"] = "tee"
	if r := post(both).expect(400); r.errCode() != "invalid_items" {
		t.Fatalf("items + item_id = %s", r.Body)
	}
	onProduct := cart(tee(1, "M"))
	onProduct["block_id"] = fx.product.ID
	if r := post(onProduct).expect(400); r.errCode() != "invalid_items" {
		t.Fatalf("cart on a product card = %s", r.Body)
	}
	noAddr := cart(tee(1, "M"))
	delete(noAddr, "shipping")
	if r := post(noAddr).expect(400); r.errCode() != "invalid_shipping" || !strings.Contains(r.Body.String(), "shipping.phone") {
		t.Fatalf("no address = %s", r.Body)
	}
	// The single-item path ships too, so it needs the address as well.
	if r := post(map[string]any{"block_id": shop.ID, "item_id": "sticker", "email": "b@example.com"}).expect(400); r.errCode() != "invalid_shipping" {
		t.Fatalf("single ship item without address = %s", r.Body)
	}

	// A good cart: equal lines merge, the fee is added once, prices are the card's.
	var o createOrderResp
	post(cart(tee(2, "M"), tee(1, "M"), map[string]any{"item_id": "ebook"}, map[string]any{"item_id": "sticker", "quantity": 1})).expect(201).json(&o)
	want := int64(3*15000000 + 5000000 + 2500000 + 2000000)
	form, _ := url.ParseQuery(fx.f.last["stripe"])
	if form.Get("line_items[0][quantity]") != "3" || form.Get("line_items[0][price_data][product_data][name]") != "Tee (Size: M)" ||
		form.Get("line_items[3][price_data][product_data][name]") != "Shipping" || form.Get("line_items[3][price_data][unit_amount]") != "2000000" ||
		form.Get("line_items[4][quantity]") != "" {
		t.Fatalf("stripe lines = %v", form)
	}
	// The address never goes to the provider.
	for _, leak := range []string{"Melati", "Bandung", "081234567890", "40115"} {
		if strings.Contains(fx.f.last["stripe"], leak) {
			t.Fatalf("address leaked to provider: %s", fx.f.last["stripe"])
		}
	}

	// Buyer view: fee and delivery status, no address.
	raw := do(t, h, "GET", "/api/v1/public/seller/orders/"+o.OrderID+"?t="+o.Token, nil).expect(200).Body.String()
	if !strings.Contains(raw, `"amount":`+strconv.FormatInt(want, 10)) || !strings.Contains(raw, `"shipping_fee":2000000`) ||
		!strings.Contains(raw, `"fulfilment":"unfulfilled"`) || strings.Contains(raw, "Melati") || strings.Contains(raw, "0812") {
		t.Fatalf("buyer view = %s", raw)
	}
	// Can't ship before it's paid.
	do(t, h, "POST", "/api/v1/orders/"+o.OrderID+"/ship", map[string]any{}, bearer(fx.tok)).expect(409)

	ref := strings.TrimPrefix(o.CheckoutURL, "https://checkout.stripe.test/")
	stripeHook(t, h, fx.pid, "whsec_s", map[string]any{"id": "evt_cart", "type": "checkout.session.completed", "data": map[string]any{"object": map[string]any{
		"id": ref, "status": "complete", "payment_status": "paid", "client_reference_id": o.OrderID, "amount_total": want, "currency": "idr"}}}).expect(200)

	got := fx.order(t, o)
	if got.Order.Status != "paid" || len(got.Downloads) != 1 || got.Downloads[0].Name != "Zine PDF" || got.Downloads[0].Index != 1 {
		t.Fatalf("paid cart = %+v", got)
	}
	_, _ = fx.s.DeliverPending(context.Background())
	sent := mailer.take()
	if len(sent) != 1 || !strings.Contains(sent[0].Text, "3 × Tee (Size: M)") || !strings.Contains(sent[0].Text, "Shipping — Rp 20.000") ||
		!strings.Contains(sent[0].Text, "will ship your order") || !strings.Contains(sent[0].Text, "Get your download") || strings.Contains(sent[0].Text, "Melati") {
		t.Fatalf("receipt = %+v", sent)
	}

	// Owner: the address, the "to ship" count and filter, the CSV.
	var detail struct {
		Order Order `json:"order"`
	}
	do(t, h, "GET", "/api/v1/orders/"+o.OrderID, nil, bearer(fx.tok)).expect(200).json(&detail)
	if detail.Order.Shipping == nil || detail.Order.Shipping.Phone != "081234567890" || detail.Order.Shipping.Country != "ID" ||
		detail.Order.Fulfilment == nil || *detail.Order.Fulfilment != "unfulfilled" || detail.Order.ShippingFee != 2000000 {
		t.Fatalf("owner detail = %+v", detail.Order)
	}
	var list struct {
		Items  []Order        `json:"items"`
		Counts map[string]int `json:"counts"`
	}
	do(t, h, "GET", "/api/v1/orders?fulfilment=unfulfilled", nil, bearer(fx.tok)).expect(200).json(&list)
	if len(list.Items) != 1 || list.Counts["to_ship"] != 1 {
		t.Fatalf("to ship = %+v", list)
	}
	do(t, h, "GET", "/api/v1/orders?fulfilment=lost", nil, bearer(fx.tok)).expect(400)
	csv := do(t, h, "GET", "/api/v1/orders.csv", nil, bearer(fx.tok)).expect(200).Body.String()
	if !strings.Contains(csv, "Ship to,Phone,Address") || !strings.Contains(csv, "Jl. Melati 7") || !strings.Contains(csv, "3 × Tee (Size: M)") {
		t.Fatalf("csv = %s", csv)
	}

	// Ship: one email with the tracking number; updating the number sends nothing.
	do(t, h, "POST", "/api/v1/orders/"+o.OrderID+"/ship", map[string]any{"tracking": strings.Repeat("x", 101)}, bearer(fx.tok)).expect(400)
	do(t, h, "POST", "/api/v1/orders/"+o.OrderID+"/ship", map[string]any{"tracking": " JNE123 "}, bearer(fx.tok)).expect(200).json(&detail)
	if *detail.Order.Fulfilment != "shipped" || detail.Order.Tracking != "JNE123" || detail.Order.ShippedAt == nil {
		t.Fatalf("shipped = %+v", detail.Order)
	}
	do(t, h, "POST", "/api/v1/orders/"+o.OrderID+"/ship", map[string]any{"tracking": "JNE456"}, bearer(fx.tok)).expect(200)
	_, _ = fx.s.DeliverPending(context.Background())
	sent = mailer.take()
	if len(sent) != 1 || !strings.Contains(sent[0].Subject, "has shipped") || !strings.Contains(sent[0].Text, "JNE123") {
		t.Fatalf("shipped emails = %+v", sent)
	}
	if raw := do(t, h, "GET", "/api/v1/public/seller/orders/"+o.OrderID+"?t="+o.Token, nil).expect(200).Body.String(); !strings.Contains(raw, `"tracking":"JNE456"`) {
		t.Fatalf("buyer tracking = %s", raw)
	}
	do(t, h, "GET", "/api/v1/orders?fulfilment=unfulfilled", nil, bearer(fx.tok)).expect(200).json(&list)
	if len(list.Items) != 0 || list.Counts["to_ship"] != 0 {
		t.Fatalf("to ship after shipping = %+v", list)
	}
	// Digital-only orders have nothing to ship.
	var d createOrderResp
	post(map[string]any{"block_id": shop.ID, "email": "d@example.com", "items": []map[string]any{{"item_id": "ebook"}}}).expect(201).json(&d)
	do(t, h, "GET", "/api/v1/orders/"+d.OrderID, nil, bearer(fx.tok)).expect(200).json(&detail)
	if detail.Order.Fulfilment != nil || detail.Order.Shipping != nil || detail.Order.ShippingFee != 0 {
		t.Fatalf("digital order = %+v", detail.Order)
	}
}

func TestCartAmountRange(t *testing.T) {
	fx := newShop(t)
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	shop := cartShop(t, fx)
	// Free shipping, so a lone badge is Rp 3.000: under Stripe's floor.
	b := shop.Data
	var d map[string]any
	_ = json.Unmarshal(b, &d)
	delete(d, "shipping_fee")
	do(t, fx.h, "PATCH", "/api/v1/blocks/"+shop.ID, map[string]any{"data": d}, bearer(fx.tok)).expect(200)
	badge := map[string]any{"block_id": shop.ID, "email": "b@example.com", "shipping": address(), "items": []map[string]any{{"item_id": "badge"}}}
	r := do(t, fx.h, "POST", "/api/v1/public/seller/orders", badge).expect(400)
	if r.errCode() != "amount_too_small" || !strings.Contains(r.Body.String(), "Rp 10.000") {
		t.Fatalf("too small = %s", r.Body)
	}
	// With Midtrans too, the provider is picked for the buyer: only it takes Rp 3.000.
	fx.connect(t, "midtrans", map[string]any{"mode": "sandbox", "secret_key": "SB-Mid-server-good"})
	do(t, fx.h, "POST", "/api/v1/public/seller/orders", badge).expect(201)
	badge["provider"] = "stripe"
	if r := do(t, fx.h, "POST", "/api/v1/public/seller/orders", badge).expect(400); r.errCode() != "amount_too_small" {
		t.Fatalf("stripe picked = %s", r.Body)
	}
	if !strings.Contains(fx.f.last["midtrans"], `"gross_amount":3000`) {
		t.Fatalf("midtrans body = %s", fx.f.last["midtrans"])
	}
}

func TestCartFormPost(t *testing.T) {
	fx := newShop(t)
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	shop := cartShop(t, fx)
	v := url.Values{"block_id": {shop.ID}, "email": {"f@example.com"},
		"line.0.item": {"tee"}, "line.0.qty": {"2"}, "line.0.option.Size": {"S"},
		"line.1.item": {"ebook"}, "line.1.qty": {"1"},
		"shipping.name": {"Fia"}, "shipping.phone": {"+62 812 3456 7890"}, "shipping.address": {"Jl. Mawar 1"},
		"shipping.city": {"Depok"}, "shipping.postal_code": {"16424"}}
	r := formPost(t, fx, "/api/v1/public/seller/orders", v).expect(303)
	if loc := r.Header().Get("Location"); !strings.HasPrefix(loc, "https://checkout.stripe.test/") {
		t.Fatalf("location = %q", loc)
	}
	var amount int64
	var qty string
	_ = testPool.QueryRow(t.Context(), `SELECT amount, items->0->>'quantity' FROM orders WHERE buyer_email = 'f@example.com'`).Scan(&amount, &qty)
	if amount != 2*15000000+5000000+2000000 || qty != "2" {
		t.Fatalf("amount = %d, qty = %s", amount, qty)
	}
	// An error comes back with the cart, the failing field and no personal data.
	v.Set("shipping.phone", "call me")
	r = formPost(t, fx, "/api/v1/public/seller/orders", v).expect(303)
	loc, _ := url.Parse(r.Header().Get("Location"))
	q := loc.Query()
	if loc.Path != "/seller/checkout" || q.Get("error") != "invalid_shipping" || q.Get("line.0.item") != "tee" || q.Get("line.0.qty") != "2" ||
		q.Get("line.0.option.Size") != "S" || q.Get("line.1.item") != "ebook" || q.Get("field") != "shipping.phone" {
		t.Fatalf("error redirect = %s", loc)
	}
	for _, leak := range []string{"Mawar", "Depok", "f%40example", "Fia"} {
		if strings.Contains(loc.String(), leak) {
			t.Fatalf("personal data in redirect: %s", loc)
		}
	}
	// A junk quantity is a quantity error, not a crash.
	v.Set("shipping.phone", "0812345678")
	v.Set("line.0.qty", "lots")
	r = formPost(t, fx, "/api/v1/public/seller/orders", v).expect(303)
	if !strings.Contains(r.Header().Get("Location"), "error=invalid_quantity") {
		t.Fatalf("junk qty = %s", r.Header().Get("Location"))
	}
}

func TestNormShipping(t *testing.T) {
	ok := []ShippingAddress{
		{Name: "A", Phone: "0812-3456-7890", Address: "x", City: "y", PostalCode: "40115"},
		{Name: "A", Phone: "+62 812 3456 7890", Address: "x", City: "y", PostalCode: "40115"},
		{Name: "A", Phone: "(021) 555 1234", Address: "x", City: "y", PostalCode: "10110"},
		{Name: "A", Phone: "+65 6123 4567", Address: "x", City: "y", PostalCode: "018956", Country: "sg"},
		{Name: "A", Phone: "+44 20 7946 0000", Address: "x", City: "y", Country: "GB", PostalCode: "SW1A 1AA"},
	}
	for _, a := range ok {
		if errs := normShipping(&a); len(errs) > 0 {
			t.Fatalf("%+v: %v", a, errs)
		}
	}
	a := ShippingAddress{Name: "A", Phone: "0812 3456 7890", Address: "x", City: "y", PostalCode: "40115"}
	normShipping(&a)
	if a.Country != "ID" || a.Phone != "081234567890" {
		t.Fatalf("normalised = %+v", a)
	}
	bad := map[string]ShippingAddress{
		"shipping.phone":       {Name: "A", Phone: "call me", Address: "x", City: "y", PostalCode: "40115"},
		"shipping.postal_code": {Name: "A", Phone: "0812345678", Address: "x", City: "y", PostalCode: "4011"},
		"shipping.country":     {Name: "A", Phone: "0812345678", Address: "x", City: "y", PostalCode: "40115", Country: "Indonesia"},
		"shipping.name":        {Phone: "0812345678", Address: "x", City: "y", PostalCode: "40115"},
		"shipping.notes":       {Name: "A", Phone: "0812345678", Address: "x", City: "y", PostalCode: "40115", Notes: strings.Repeat("n", 301)},
	}
	for field, a := range bad {
		if errs := normShipping(&a); errs[field] == "" {
			t.Fatalf("%s not flagged: %v", field, errs)
		}
	}
}
