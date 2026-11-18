package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func mustNew(t *testing.T, name, mode string, c Credentials, o Options) Provider {
	t.Helper()
	p, err := New(name, mode, c, o)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func basicUser(t *testing.T, h http.Header) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h.Get("Authorization"), "Basic "))
	if err != nil {
		t.Fatalf("auth header %q", h.Get("Authorization"))
	}
	u, _, _ := strings.Cut(string(raw), ":")
	return u
}

// ---------- Xendit ----------

func TestXenditCreateCheckout(t *testing.T) {
	f := newFakeAPI(t)
	f.on("POST /v2/invoices", 200, `{"id":"inv_1","external_id":"x","status":"PENDING","amount":170000,"invoice_url":"https://checkout-staging.xendit.co/web/inv_1","expiry_date":"2026-11-18T11:00:00.000Z"}`)
	p := mustNew(t, Xendit, Sandbox, Credentials{SecretKey: "xnd_development_k"}, opts(f))
	s, err := p.CreateCheckout(ctx, sampleCheckout("IDR"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref != "inv_1" || s.URL != "https://checkout-staging.xendit.co/web/inv_1" || !s.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("session = %+v", s)
	}
	r := f.last()
	if basicUser(t, r.Header) != "xnd_development_k" || r.Header.Get("X-IDEMPOTENCY-KEY") == "" {
		t.Fatalf("headers = %v", r.Header)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(r.Body), &body)
	// Xendit takes whole rupiah.
	if body["amount"].(float64) != 170000 || body["currency"] != "IDR" || body["external_id"] != sampleCheckout("IDR").OrderID ||
		body["invoice_duration"].(float64) != 3600 || body["payer_email"] != "buyer@example.com" {
		t.Fatalf("body = %s", r.Body)
	}
	items := body["items"].([]any)
	if it := items[0].(map[string]any); it["price"].(float64) != 85000 || it["quantity"].(float64) != 2 {
		t.Fatalf("items = %v", items)
	}
}

func TestXenditUSDAndErrors(t *testing.T) {
	f := newFakeAPI(t)
	f.on("POST /v2/invoices", 200, `{"id":"inv_2","invoice_url":"https://x/inv_2"}`)
	p := mustNew(t, Xendit, Sandbox, Credentials{SecretKey: "xnd_development_k"}, opts(f))
	c := sampleCheckout("USD")
	c.Amount, c.Items = 1999, []Item{{Name: "Ebook", UnitAmount: 1999, Quantity: 1}}
	if _, err := p.CreateCheckout(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.last().Body, `"amount":19.99`) {
		t.Fatalf("USD body = %s", f.last().Body)
	}
	c = sampleCheckout("IDR")
	c.Amount, c.Items = 150, []Item{{Name: "x", UnitAmount: 150, Quantity: 1}}
	if _, err := p.CreateCheckout(ctx, c); !IsCode(err, "invalid_amount") {
		t.Fatalf("sen amount err = %v", err)
	}
	f.on("POST /v2/invoices", 401, `{"error_code":"INVALID_API_KEY","message":"API key is invalid"}`)
	if _, err := p.CreateCheckout(ctx, sampleCheckout("IDR")); !IsCode(err, "invalid_credentials") {
		t.Fatalf("bad key err = %v", err)
	}
}

func TestXenditWebhook(t *testing.T) {
	p := mustNew(t, Xendit, Sandbox, Credentials{SecretKey: "xnd_development_k", WebhookSecret: "cbtoken"}, Options{})
	body := []byte(`{"id":"inv_1","external_id":"order-1","status":"PAID","amount":170000,"paid_amount":170000,"currency":"IDR"}`)
	req := httptest.NewRequest("POST", "/", nil)
	if _, err := p.VerifyWebhook(req, body); err != ErrBadSignature {
		t.Fatalf("missing token err = %v", err)
	}
	req.Header.Set("x-callback-token", "wrong")
	if _, err := p.VerifyWebhook(req, body); err != ErrBadSignature {
		t.Fatalf("wrong token err = %v", err)
	}
	req.Header.Set("x-callback-token", "cbtoken")
	ev, err := p.VerifyWebhook(req, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Status != StatusPaid || ev.OrderID != "order-1" || ev.Ref != "inv_1" || ev.Amount != 17000000 || ev.Currency != "IDR" || ev.Key != "inv:inv_1" || !ev.Confirm {
		t.Fatalf("event = %+v", ev)
	}
	req.Header.Set("webhook-id", "wh_9")
	ev, _ = p.VerifyWebhook(req, []byte(`{"id":"inv_1","external_id":"order-1","status":"EXPIRED","amount":170000}`))
	if ev.Status != StatusExpired || ev.Key != "inv:inv_1" {
		t.Fatalf("expired event = %+v", ev)
	}
	// No stored token: every delivery is rejected.
	noTok := mustNew(t, Xendit, Sandbox, Credentials{SecretKey: "xnd_development_k"}, Options{})
	req.Header.Set("x-callback-token", "")
	if _, err := noTok.VerifyWebhook(req, body); err != ErrBadSignature {
		t.Fatalf("no token configured err = %v", err)
	}
}

func TestXenditStatusAndTest(t *testing.T) {
	f := newFakeAPI(t)
	f.on("GET /v2/invoices/inv_1", 200, `{"id":"inv_1","external_id":"o","status":"SETTLED","amount":50000,"currency":"IDR"}`)
	f.on("GET /v2/invoices", 200, `[]`)
	p := mustNew(t, Xendit, Live, Credentials{SecretKey: "xnd_production_k"}, opts(f))
	ev, err := p.GetStatus(ctx, "inv_1")
	if err != nil || ev.Status != StatusPaid || ev.Amount != 5000000 {
		t.Fatalf("status = %+v %v", ev, err)
	}
	if _, err := p.GetStatus(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("missing invoice err = %v", err)
	}
	if err := p.TestConnection(ctx); err != nil {
		t.Fatal(err)
	}
	if r := f.last(); r.Path != "/v2/invoices" || r.Query != "limit=1" {
		t.Fatalf("test request = %+v", r)
	}
	f.on("GET /v2/invoices", 403, `{"error_code":"REQUEST_FORBIDDEN_ERROR","message":"The API key is forbidden to perform this request"}`)
	if err := p.TestConnection(ctx); !IsCode(err, "invalid_credentials") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("forbidden err = %v", err)
	}
}

// ---------- Midtrans ----------

func midtransSig(orderID, code, gross, key string) string {
	s := sha512.Sum512([]byte(orderID + code + gross + key))
	return hex.EncodeToString(s[:])
}

func TestMidtransCreateCheckout(t *testing.T) {
	f := newFakeAPI(t)
	f.on("POST /snap/v1/transactions", 201, `{"token":"tok","redirect_url":"https://app.sandbox.midtrans.com/snap/v4/redirection/tok"}`)
	p := mustNew(t, Midtrans, Sandbox, Credentials{SecretKey: "SB-Mid-server-k"}, opts(f))
	c := sampleCheckout("IDR")
	c.Items[0].Name = strings.Repeat("n", 80)
	s, err := p.CreateCheckout(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref != c.OrderID || !strings.HasSuffix(s.URL, "/tok") {
		t.Fatalf("session = %+v", s)
	}
	r := f.last()
	if basicUser(t, r.Header) != "SB-Mid-server-k" || r.Header.Get("X-Override-Notification") != c.NotifyURL {
		t.Fatalf("headers = %v", r.Header)
	}
	var body struct {
		TD    map[string]any   `json:"transaction_details"`
		Items []map[string]any `json:"item_details"`
		Exp   map[string]any   `json:"expiry"`
		CB    map[string]any   `json:"callbacks"`
	}
	_ = json.Unmarshal([]byte(r.Body), &body)
	if body.TD["gross_amount"].(float64) != 170000 || body.TD["order_id"] != c.OrderID {
		t.Fatalf("transaction_details = %v", body.TD)
	}
	if body.Items[0]["price"].(float64) != 85000 || len(body.Items[0]["name"].(string)) != 50 {
		t.Fatalf("items = %v", body.Items)
	}
	if body.Exp["duration"].(float64) != 60 || body.CB["finish"] != c.SuccessURL {
		t.Fatalf("expiry/callbacks = %v %v", body.Exp, body.CB)
	}
	if _, err := p.CreateCheckout(ctx, sampleCheckout("USD")); !IsCode(err, "unsupported_currency") {
		t.Fatalf("USD err = %v", err)
	}
	f.on("POST /snap/v1/transactions", 401, `{"error_messages":["Access denied due to unauthorized transaction, please check client or server key"]}`)
	if _, err := p.CreateCheckout(ctx, c); !IsCode(err, "invalid_credentials") {
		t.Fatalf("bad key err = %v", err)
	}
}

func TestMidtransWebhook(t *testing.T) {
	key := "SB-Mid-server-k"
	p := mustNew(t, Midtrans, Sandbox, Credentials{SecretKey: key}, Options{})
	mk := func(status, fraud, code string, sig string) []byte {
		if sig == "" {
			sig = midtransSig("order-1", code, "170000.00", key)
		}
		b, _ := json.Marshal(map[string]string{
			"order_id": "order-1", "status_code": code, "gross_amount": "170000.00", "currency": "IDR",
			"transaction_status": status, "fraud_status": fraud, "transaction_id": "tx-9", "signature_key": sig,
		})
		return b
	}
	req := httptest.NewRequest("POST", "/", nil)
	cases := []struct {
		status, fraud, code string
		want                Status
	}{
		{"settlement", "", "200", StatusPaid},
		{"capture", "accept", "200", StatusPaid},
		{"capture", "challenge", "200", StatusPending},
		{"pending", "", "201", StatusPending},
		{"expire", "", "407", StatusExpired},
		{"deny", "", "202", StatusFailed},
		{"cancel", "", "200", StatusFailed},
		{"refund", "", "200", StatusRefunded},
	}
	for _, c := range cases {
		ev, err := p.VerifyWebhook(req, mk(c.status, c.fraud, c.code, ""))
		if err != nil {
			t.Fatalf("%s: %v", c.status, err)
		}
		// The key uses only signed fields; the status still needs GetStatus.
		if ev.Status != c.want || ev.OrderID != "order-1" || ev.Amount != 17000000 || ev.Key != "order-1:"+c.code+":170000.00" || !ev.Confirm {
			t.Fatalf("%s: event = %+v", c.status, ev)
		}
	}
	// A genuine 201 "pending" notification re-labelled as settlement: the
	// signature still verifies (status isn't signed) but status_code 201 can't be paid.
	if ev, err := p.VerifyWebhook(req, mk("settlement", "", "201", "")); err != nil || ev.Status == StatusPaid {
		t.Fatalf("relabelled 201 = %+v %v", ev, err)
	}
	if ev, err := p.VerifyWebhook(req, mk("capture", "accept", "202", "")); err != nil || ev.Status == StatusPaid {
		t.Fatalf("relabelled 202 capture = %+v %v", ev, err)
	}
	if _, err := p.VerifyWebhook(req, mk("settlement", "", "200", midtransSig("order-1", "200", "170000.00", "other-key"))); err != ErrBadSignature {
		t.Fatalf("forged signature err = %v", err)
	}
	// Tampering with the amount breaks the signature.
	b := mk("settlement", "", "200", "")
	b = []byte(strings.Replace(string(b), "170000.00", "1.00", 1))
	if _, err := p.VerifyWebhook(req, b); err != ErrBadSignature {
		t.Fatalf("tampered amount err = %v", err)
	}
}

func TestMidtransStatusAndTest(t *testing.T) {
	f := newFakeAPI(t)
	f.on("GET /v2/order-1/status", 200, `{"status_code":"200","order_id":"order-1","gross_amount":"50000.00","transaction_status":"settlement"}`)
	f.on("GET /v2/order-x/status", 200, `{"status_code":"407","order_id":"order-x","gross_amount":"50000.00","transaction_status":"expire"}`)
	if ev, err := mustNew(t, Midtrans, Sandbox, Credentials{SecretKey: "SB-Mid-server-k"}, opts(f)).GetStatus(ctx, "order-x"); err != nil || ev.Status != StatusExpired {
		t.Fatalf("expired status = %+v %v", ev, err)
	}
	f.on("GET /v2/tautan-connection-test/status", 404, `{"status_code":"404","status_message":"Transaction doesn't exist."}`)
	p := mustNew(t, Midtrans, Sandbox, Credentials{SecretKey: "SB-Mid-server-k"}, opts(f))
	ev, err := p.GetStatus(ctx, "order-1")
	if err != nil || ev.Status != StatusPaid || ev.Amount != 5000000 {
		t.Fatalf("status = %+v %v", ev, err)
	}
	if err := p.TestConnection(ctx); err != nil {
		t.Fatalf("valid key: %v", err)
	}
	// Midtrans also reports errors as HTTP 200 with status_code in the body.
	f.on("GET /v2/tautan-connection-test/status", 200, `{"status_code":"401","status_message":"Unknown Merchant server_key/id"}`)
	if err := p.TestConnection(ctx); !IsCode(err, "invalid_credentials") {
		t.Fatalf("bad key err = %v", err)
	}
}

func TestMidtransHosts(t *testing.T) {
	sb := &midtrans{mode: Sandbox}
	live := &midtrans{mode: Live}
	if sb.snapBase() != "https://app.sandbox.midtrans.com" || sb.apiBase() != "https://api.sandbox.midtrans.com" ||
		live.snapBase() != "https://app.midtrans.com" || live.apiBase() != "https://api.midtrans.com" {
		t.Fatal("wrong Midtrans hosts")
	}
}

// ---------- Stripe ----------

func stripeSign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	return "t=" + strconv.FormatInt(ts, 10) + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

func TestStripeCreateCheckout(t *testing.T) {
	f := newFakeAPI(t)
	f.on("POST /v1/checkout/sessions", 200, `{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1","status":"open","payment_status":"unpaid","expires_at":`+strconv.FormatInt(fixedNow.Add(time.Hour).Unix(), 10)+`}`)
	p := mustNew(t, Stripe, Sandbox, Credentials{SecretKey: "sk_test_k"}, opts(f))
	c := sampleCheckout("IDR")
	s, err := p.CreateCheckout(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.Ref != "cs_test_1" || !strings.Contains(s.URL, "checkout.stripe.com") {
		t.Fatalf("session = %+v", s)
	}
	r := f.last()
	if r.Header.Get("Authorization") != "Bearer sk_test_k" || r.Header.Get("Idempotency-Key") != "tautan-checkout-"+c.OrderID {
		t.Fatalf("headers = %v", r.Header)
	}
	form, _ := url.ParseQuery(r.Body)
	want := map[string]string{
		"mode": "payment", "client_reference_id": c.OrderID, "metadata[order_id]": c.OrderID,
		"customer_email":                                "buyer@example.com",
		"line_items[0][price_data][currency]":           "idr",
		"line_items[0][price_data][unit_amount]":        "8500000", // minor units pass through
		"line_items[0][quantity]":                       "2",
		"line_items[0][price_data][product_data][name]": "Preset pack",
		"success_url":                                   c.SuccessURL, "cancel_url": c.CancelURL,
		"expires_at": strconv.FormatInt(fixedNow.Add(time.Hour).Unix(), 10),
	}
	for k, v := range want {
		if form.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, form.Get(k), v)
		}
	}
	// Expiry is clamped to Stripe's 30 min minimum.
	c.ExpiresAt = fixedNow.Add(10 * time.Minute)
	_, _ = p.CreateCheckout(ctx, c)
	form, _ = url.ParseQuery(f.last().Body)
	if form.Get("expires_at") != strconv.FormatInt(fixedNow.Add(31*time.Minute).Unix(), 10) {
		t.Fatalf("expires_at = %s", form.Get("expires_at"))
	}
	f.on("POST /v1/checkout/sessions", 401, `{"error":{"type":"invalid_request_error","message":"Invalid API Key provided: sk_test_****k"}}`)
	if _, err := p.CreateCheckout(ctx, sampleCheckout("USD")); !IsCode(err, "invalid_credentials") {
		t.Fatalf("bad key err = %v", err)
	}
}

func TestStripeWebhook(t *testing.T) {
	secret := "whsec_test"
	p := mustNew(t, Stripe, Sandbox, Credentials{SecretKey: "sk_test_k", WebhookSecret: secret}, Options{Now: func() time.Time { return fixedNow }})
	completed := []byte(`{"id":"evt_1","type":"checkout.session.completed","livemode":false,"data":{"object":{"id":"cs_test_1","object":"checkout.session","status":"complete","payment_status":"paid","client_reference_id":"order-1","amount_total":17000000,"currency":"idr","payment_intent":"pi_1"}}}`)
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Stripe-Signature", stripeSign(secret, fixedNow.Unix(), completed))
	ev, err := p.VerifyWebhook(req, completed)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Key != "evt_1" || ev.Status != StatusPaid || ev.OrderID != "order-1" || ev.Ref != "cs_test_1" ||
		ev.PaymentRef != "pi_1" || ev.Amount != 17000000 || ev.Currency != "IDR" || ev.Live == nil || *ev.Live {
		t.Fatalf("event = %+v", ev)
	}
	// Wrong secret, tampered body, stale timestamp, missing header.
	for name, hdr := range map[string]string{
		"wrong secret": stripeSign("whsec_other", fixedNow.Unix(), completed),
		"stale":        stripeSign(secret, fixedNow.Add(-6*time.Minute).Unix(), completed),
		"empty":        "",
		"garbage":      "t=abc,v1=zz",
	} {
		req.Header.Set("Stripe-Signature", hdr)
		if _, err := p.VerifyWebhook(req, completed); err != ErrBadSignature {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	req.Header.Set("Stripe-Signature", stripeSign(secret, fixedNow.Unix(), completed))
	if _, err := p.VerifyWebhook(req, append([]byte(" "), completed...)); err != ErrBadSignature {
		t.Error("tampered body accepted")
	}
	// A rolled secret: one of several v1 entries matches.
	req.Header.Set("Stripe-Signature", stripeSign(secret, fixedNow.Unix(), completed)+",v1=00ff")
	if _, err := p.VerifyWebhook(req, completed); err != nil {
		t.Errorf("multiple v1: %v", err)
	}

	for typ, want := range map[string]Status{
		"checkout.session.async_payment_failed":    StatusFailed,
		"checkout.session.async_payment_succeeded": StatusPaid,
	} {
		paid := "unpaid"
		if want == StatusPaid {
			paid = "paid"
		}
		b := []byte(`{"id":"evt_x","type":"` + typ + `","data":{"object":{"id":"cs_1","status":"complete","payment_status":"` + paid + `","client_reference_id":"order-1"}}}`)
		req.Header.Set("Stripe-Signature", stripeSign(secret, fixedNow.Unix(), b))
		if ev, err := p.VerifyWebhook(req, b); err != nil || ev.Status != want {
			t.Errorf("%s: %+v %v", typ, ev, err)
		}
	}
	expired := []byte(`{"id":"evt_2","type":"checkout.session.expired","data":{"object":{"id":"cs_1","status":"expired","payment_status":"unpaid","metadata":{"order_id":"order-1"}}}}`)
	req.Header.Set("Stripe-Signature", stripeSign(secret, fixedNow.Unix(), expired))
	if ev, err := p.VerifyWebhook(req, expired); err != nil || ev.Status != StatusExpired || ev.OrderID != "order-1" {
		t.Fatalf("expired = %+v %v", ev, err)
	}
	refund := []byte(`{"id":"evt_3","type":"charge.refunded","data":{"object":{"payment_intent":"pi_1","refunded":true,"amount":17000000,"currency":"idr"}}}`)
	req.Header.Set("Stripe-Signature", stripeSign(secret, fixedNow.Unix(), refund))
	if ev, err := p.VerifyWebhook(req, refund); err != nil || ev.Status != StatusRefunded || ev.PaymentRef != "pi_1" {
		t.Fatalf("refund = %+v %v", ev, err)
	}
	partial := []byte(`{"id":"evt_4","type":"charge.refunded","data":{"object":{"payment_intent":"pi_1","refunded":false}}}`)
	req.Header.Set("Stripe-Signature", stripeSign(secret, fixedNow.Unix(), partial))
	if ev, err := p.VerifyWebhook(req, partial); err != nil || ev.Status != "" {
		t.Fatalf("partial refund should not change status: %+v %v", ev, err)
	}
	other := []byte(`{"id":"evt_5","type":"customer.created","data":{"object":{}}}`)
	req.Header.Set("Stripe-Signature", stripeSign(secret, fixedNow.Unix(), other))
	if ev, err := p.VerifyWebhook(req, other); err != nil || ev.Status != "" || ev.Key != "evt_5" {
		t.Fatalf("ignored event = %+v %v", ev, err)
	}
}

func TestStripeStatusAndTest(t *testing.T) {
	f := newFakeAPI(t)
	f.on("GET /v1/checkout/sessions/cs_1", 200, `{"id":"cs_1","status":"complete","payment_status":"paid","client_reference_id":"order-1","amount_total":500,"currency":"usd","payment_intent":{"id":"pi_9"}}`)
	f.on("GET /v1/checkout/sessions/cs_gone", 404, `{"error":{"message":"No such checkout.session"}}`)
	f.on("GET /v1/checkout/sessions", 200, `{"object":"list","data":[]}`)
	p := mustNew(t, Stripe, Sandbox, Credentials{SecretKey: "rk_test_k"}, opts(f))
	ev, err := p.GetStatus(ctx, "cs_1")
	if err != nil || ev.Status != StatusPaid || ev.PaymentRef != "pi_9" || ev.Currency != "USD" || ev.Amount != 500 {
		t.Fatalf("status = %+v %v", ev, err)
	}
	if _, err := p.GetStatus(ctx, "cs_gone"); err != ErrNotFound {
		t.Fatalf("missing err = %v", err)
	}
	if err := p.TestConnection(ctx); err != nil {
		t.Fatal(err)
	}
	f.on("GET /v1/checkout/sessions", 403, `{"error":{"message":"The provided key does not have the required permissions"}}`)
	if err := p.TestConnection(ctx); !IsCode(err, "invalid_credentials") {
		t.Fatalf("restricted key err = %v", err)
	}
}

func TestUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	u := srv.URL
	srv.Close() // nothing listens any more
	for _, name := range Names {
		key := map[string]string{Xendit: "xnd_development_k", Midtrans: "SB-Mid-server-k", Stripe: "sk_test_k"}[name]
		p := mustNew(t, name, Sandbox, Credentials{SecretKey: key},
			Options{BaseURLs: map[string]string{Xendit: u, Midtrans: u, "midtrans_api": u, Stripe: u}})
		if err := p.TestConnection(ctx); !IsCode(err, "provider_unreachable") {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	var pe *Error
	if !errors.As(&Error{Code: "x"}, &pe) {
		t.Fatal("Error should satisfy errors.As")
	}
}
