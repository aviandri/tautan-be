package server

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/aviandri/tautan-be/internal/payments"
	"github.com/aviandri/tautan-be/internal/testutil"
)

// fakeOrder creates an order on the seller's file card with the fake provider
// and returns it with the checkout path (relative to the API).
func fakeOrder(t *testing.T, fx shopFixture, email string) (createOrderResp, string) {
	t.Helper()
	var o createOrderResp
	do(t, fx.h, "POST", "/api/v1/public/seller/orders", map[string]any{
		"block_id": fx.file.ID, "email": email, "provider": "fake"}).expect(201).json(&o)
	u, err := url.Parse(o.CheckoutURL)
	if err != nil || u.Host != "api.tautan.test" || !strings.HasPrefix(u.Path, "/dev/fake-checkout/fk_") {
		t.Fatalf("checkout_url = %q", o.CheckoutURL)
	}
	return o, u.Path
}

func TestFakeProviderFlow(t *testing.T) {
	fx := newShop(t)
	h := fx.h

	var list struct {
		Providers []struct {
			Provider        string   `json:"provider"`
			NeedsWebhookSec bool     `json:"needs_webhook_secret"`
			Currencies      []string `json:"currencies"`
		} `json:"providers"`
	}
	do(t, h, "GET", "/api/v1/payments/providers", nil, bearer(fx.tok)).expect(200).json(&list)
	if n := len(list.Providers); n != 4 || list.Providers[3].Provider != "fake" || list.Providers[3].NeedsWebhookSec {
		t.Fatalf("providers = %+v", list.Providers)
	}

	// Sandbox only, keys start with fake_, no webhook secret.
	do(t, h, "PUT", "/api/v1/payments/providers/fake", map[string]any{"mode": "live", "secret_key": "fake_x", "enabled": true}, bearer(fx.tok)).expect(400)
	do(t, h, "PUT", "/api/v1/payments/providers/fake", map[string]any{"mode": "sandbox", "secret_key": "sk_test_x", "enabled": true}, bearer(fx.tok)).expect(400)
	fx.connect(t, "fake", map[string]any{"mode": "sandbox", "secret_key": "fake_dev"})

	// Pay: the checkout page posts the outcome, the webhook path confirms it,
	// and the buyer lands on the order page with a download.
	o, path := fakeOrder(t, fx, "buyer@example.com")
	page := do(t, h, "GET", path, nil).expect(200).Body.String()
	if !strings.Contains(page, "Rp 85.000") || !strings.Contains(page, "Lightroom presets") || !strings.Contains(page, `data-testid="fake-pay"`) {
		t.Fatalf("checkout page:\n%s", page)
	}
	r := do(t, h, "POST", path+"/paid", nil).expect(303)
	if loc := r.Header().Get("Location"); !strings.HasPrefix(loc, "https://tautan.test/seller/orders/"+o.OrderID+"?t=") || strings.Contains(loc, "cancelled") {
		t.Fatalf("paid redirect = %q", loc)
	}
	got := fx.order(t, o)
	if got.Order.Status != "paid" || len(got.Downloads) != 1 {
		t.Fatalf("after pay: %+v", got)
	}
	// The page now shows the outcome; repeating the post changes nothing.
	if !strings.Contains(do(t, h, "GET", path, nil).expect(200).Body.String(), "<strong>paid</strong>") {
		t.Fatal("settled page should show the status")
	}
	do(t, h, "POST", path+"/failed", nil).expect(303)
	if st := fx.order(t, o).Order.Status; st != "paid" {
		t.Fatalf("a paid order moved to %s", st)
	}

	// Fail: redirected to the cancel URL, order failed.
	o2, path2 := fakeOrder(t, fx, "b2@example.com")
	if loc := do(t, h, "POST", path2+"/failed", nil).expect(303).Header().Get("Location"); !strings.Contains(loc, "cancelled=1") {
		t.Fatalf("fail redirect = %q", loc)
	}
	if st := fx.order(t, o2).Order.Status; st != "failed" {
		t.Fatalf("failed order status = %s", st)
	}

	// Expire.
	o3, path3 := fakeOrder(t, fx, "b3@example.com")
	do(t, h, "POST", path3+"/expired", nil).expect(303)
	if st := fx.order(t, o3).Order.Status; st != "expired" {
		t.Fatalf("expired order status = %s", st)
	}

	// Paying half is refused by the amount check: the order stays unpaid.
	o4, path4 := fakeOrder(t, fx, "b4@example.com")
	do(t, h, "POST", path4+"/mismatch", nil).expect(303)
	if got := fx.order(t, o4); got.Order.Status == "paid" || len(got.Downloads) != 0 {
		t.Fatalf("underpaid order: %+v", got)
	}
	var note string
	_ = testPool.QueryRow(context.Background(), `SELECT status FROM payment_events WHERE order_id = $1 ORDER BY id DESC LIMIT 1`, o4.OrderID).Scan(&note)
	if note != "amount_mismatch" {
		t.Fatalf("underpaid event status = %q", note)
	}

	// A notification can't set a status: it only triggers the store lookup.
	do(t, h, "POST", "/webhooks/fake/"+fx.pid, `{"ref":"fk_nope","order_id":"`+o4.OrderID+`","status":"paid"}`).expect(200)
	if st := fx.order(t, o4).Order.Status; st == "paid" {
		t.Fatal("a forged notification paid the order")
	}
	do(t, h, "POST", "/webhooks/fake/"+fx.pid, `{"status":"paid"}`).expect(400)

	// Unknown refs and actions.
	do(t, h, "GET", "/dev/fake-checkout/fk_unknown", nil).expect(404)
	do(t, h, "POST", path4+"/refund", nil).expect(404)
}

func TestFakeProviderOffInProduction(t *testing.T) {
	testutil.Truncate(t, testPool)
	cfg := testConfig()
	cfg.Env = "production"
	cfg.PaymentsEncryptionKey = testPaymentsKey
	cfg.PublicBaseURL = "https://api.tautan.test"
	s := New(cfg, testPool)
	s.Quiet = true
	h := s.Routes()
	a := signup(t, h, "prodseller")

	var list struct {
		Providers []struct {
			Provider string `json:"provider"`
		} `json:"providers"`
	}
	do(t, h, "GET", "/api/v1/payments/providers", nil, bearer(a.AccessToken)).expect(200).json(&list)
	for _, p := range list.Providers {
		if p.Provider == "fake" {
			t.Fatal("production lists the fake provider")
		}
	}
	do(t, h, "PUT", "/api/v1/payments/providers/fake", map[string]any{"mode": "sandbox", "secret_key": "fake_x", "enabled": true}, bearer(a.AccessToken)).expect(404)
	do(t, h, "POST", "/api/v1/payments/providers/fake/test", nil, bearer(a.AccessToken)).expect(404)
	do(t, h, "GET", "/dev/fake-checkout/fk_x", nil).expect(404)
	do(t, h, "POST", "/dev/fake-checkout/fk_x/paid", nil).expect(404)
	do(t, h, "POST", "/webhooks/fake/"+a.Profile.ID, `{"ref":"fk_x"}`).expect(404)
	if _, err := s.provider("fake", "sandbox", payments.Credentials{SecretKey: "fake_x"}); err == nil {
		t.Fatal("production built a fake provider")
	}
}
