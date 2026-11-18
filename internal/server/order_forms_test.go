package server

import (
	"net/url"
	"strings"
	"testing"
)

func formPost(t *testing.T, fx shopFixture, path string, v url.Values) resp {
	t.Helper()
	return do(t, fx.h, "POST", path, v.Encode(), withHeader("Content-Type", "application/x-www-form-urlencoded"))
}

func TestOrderFormPosts(t *testing.T) {
	fx := newShop(t)
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})

	// Success: 303 to the provider's hosted checkout, no referrer.
	r := formPost(t, fx, "/api/v1/public/seller/orders", url.Values{"block_id": {fx.product.ID}, "email": {"f@example.com"}, "name": {"Fia"}, "option.Format": {"EPUB"}}).expect(303)
	if loc := r.Header().Get("Location"); !strings.HasPrefix(loc, "https://checkout.stripe.test/cs_test_") {
		t.Fatalf("location = %q", loc)
	}
	if r.Header().Get("Referrer-Policy") != "no-referrer" || r.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("headers = %v", r.Header())
	}
	var variant string
	_ = testPool.QueryRow(t.Context(), `SELECT items->0->>'variant' FROM orders WHERE buyer_email = 'f@example.com'`).Scan(&variant)
	if variant != "Format: EPUB" {
		t.Fatalf("variant = %q", variant)
	}
	// Errors: back to the FE checkout page with a code (and the item kept).
	r = formPost(t, fx, "/api/v1/public/seller/orders", url.Values{"block_id": {fx.product.ID}, "email": {"nope"}, "item_id": {"x1"}}).expect(303)
	loc, _ := url.Parse(r.Header().Get("Location"))
	if loc.Host != "tautan.test" || loc.Path != "/seller/checkout" || loc.Query().Get("error") != "invalid_email" ||
		loc.Query().Get("block") != fx.product.ID || loc.Query().Get("item") != "x1" {
		t.Fatalf("error redirect = %s", loc)
	}
	r = formPost(t, fx, "/api/v1/public/seller/orders", url.Values{"block_id": {fx.product.ID}, "email": {"f@example.com"}}).expect(303)
	if !strings.Contains(r.Header().Get("Location"), "error=invalid_options") {
		t.Fatalf("missing option = %s", r.Header().Get("Location"))
	}

	// Lookup: 303 to the order page, or back with an error.
	var code string
	_ = testPool.QueryRow(t.Context(), `SELECT code FROM orders WHERE buyer_email = 'f@example.com'`).Scan(&code)
	r = formPost(t, fx, "/api/v1/public/seller/orders/lookup", url.Values{"email": {"F@example.com"}, "code": {strings.ToLower(code)}}).expect(303)
	if loc := r.Header().Get("Location"); !strings.HasPrefix(loc, "https://tautan.test/seller/orders/") || !strings.Contains(loc, "?t=") {
		t.Fatalf("lookup location = %q", loc)
	}
	r = formPost(t, fx, "/api/v1/public/seller/orders/lookup", url.Values{"email": {"x@example.com"}, "code": {code}}).expect(303)
	if r.Header().Get("Location") != "https://tautan.test/seller/orders?error=not_found" {
		t.Fatalf("lookup miss = %q", r.Header().Get("Location"))
	}
	// JSON clients are unchanged.
	do(t, fx.h, "POST", "/api/v1/public/seller/orders/lookup", map[string]string{"email": "x@example.com", "code": code}).expect(404)
}

func TestFreeFileRejectsPrivateUpload(t *testing.T) {
	fx := newShop(t)
	var b Block
	do(t, fx.h, "GET", "/api/v1/profile/editor", nil, bearer(fx.tok)).expect(200)
	do(t, fx.h, "PATCH", "/api/v1/blocks/"+fx.file.ID, map[string]any{"data": map[string]any{"title": "x", "url": "https://files.example/a.zip", "price": 100}}, bearer(fx.tok)).expect(200).json(&b)
	priv := upload(t, fx, "private")
	r := do(t, fx.h, "PATCH", "/api/v1/blocks/"+fx.file.ID, map[string]any{"data": map[string]any{"title": "x", "url": priv}}, bearer(fx.tok)).expect(400)
	if !strings.Contains(r.Body.String(), "give it away for free") {
		t.Fatalf("free private file: %s", r.Body)
	}
}
