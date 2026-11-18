package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aviandri/tautan-be/internal/payments"
	"github.com/aviandri/tautan-be/internal/testutil"
)

// fakeCheckout plays Xendit, Midtrans and Stripe for the order flow: test
// connection (keys containing "good" pass), create checkout and status, with
// per-ref status the test controls. Nothing here talks to a real provider.
type fakeCheckout struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	status map[string]string // provider ref → provider-native status
	amount map[string]string // provider ref → raw JSON amount override (Xendit) / gross_amount (Midtrans)
	last   map[string]string // provider → last create body
	n      int
}

func newFakeCheckout(t *testing.T) *fakeCheckout {
	f := &fakeCheckout{t: t, status: map[string]string{}, amount: map[string]string{}, last: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCheckout) set(ref, st string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[ref] = st
}

func (f *fakeCheckout) setAmount(ref, amt string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.amount[ref] = amt
}

func (f *fakeCheckout) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	user, _, _ := r.BasicAuth()
	if !strings.Contains(user+r.Header.Get("Authorization"), "good") {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"status_code":"401","message":"bad key","error":{"message":"bad key"}}`)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	// Xendit
	case r.Method == "POST" && p == "/v2/invoices":
		f.n++
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		f.last["xendit"] = string(body)
		id := "inv_" + strconv.Itoa(f.n)
		f.status[id] = "PENDING"
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "external_id": in["external_id"], "status": "PENDING",
			"invoice_url": "https://checkout.xendit.test/" + id, "amount": in["amount"]})
	case r.Method == "GET" && p == "/v2/invoices":
		_, _ = io.WriteString(w, `[]`)
	case r.Method == "GET" && strings.HasPrefix(p, "/v2/invoices/"):
		id := strings.TrimPrefix(p, "/v2/invoices/")
		st, ok := f.status[id]
		if !ok {
			w.WriteHeader(404)
			return
		}
		amt := json.RawMessage(`85000`)
		cur := "IDR"
		if a, ok := f.amount[id]; ok {
			amt, cur = json.RawMessage(a), ""
			if a != "0" {
				cur = "IDR"
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": st, "amount": amt, "currency": cur})
	// Midtrans
	case r.Method == "POST" && p == "/snap/v1/transactions":
		var in struct {
			TD struct {
				OrderID string `json:"order_id"`
			} `json:"transaction_details"`
		}
		_ = json.Unmarshal(body, &in)
		f.last["midtrans"] = string(body)
		f.last["midtrans_notify"] = r.Header.Get("X-Override-Notification")
		f.status[in.TD.OrderID] = "pending"
		w.WriteHeader(201)
		_, _ = io.WriteString(w, `{"token":"t","redirect_url":"https://app.sandbox.midtrans.test/snap/t"}`)
	case r.Method == "GET" && strings.HasSuffix(p, "/status"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/v2/"), "/status")
		st, ok := f.status[id]
		if !ok {
			_, _ = io.WriteString(w, `{"status_code":"404","status_message":"Transaction doesn't exist."}`)
			return
		}
		gross := "85000.00"
		if a, ok := f.amount[id]; ok {
			gross = a
		}
		code := map[string]string{"settlement": "200", "capture": "200", "pending": "201", "expire": "407", "deny": "202", "cancel": "200", "refund": "200"}[st]
		_ = json.NewEncoder(w).Encode(map[string]any{"status_code": code, "order_id": id, "gross_amount": gross, "transaction_status": st})
	// Stripe
	case r.Method == "POST" && p == "/v1/checkout/sessions":
		f.n++
		form, _ := url.ParseQuery(string(body))
		f.last["stripe"] = string(body)
		id := "cs_test_" + strconv.Itoa(f.n)
		f.status[id] = "open"
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "url": "https://checkout.stripe.test/" + id,
			"status": "open", "payment_status": "unpaid", "client_reference_id": form.Get("client_reference_id"),
			"expires_at": time.Now().Add(time.Hour).Unix()})
	case r.Method == "GET" && p == "/v1/checkout/sessions":
		_, _ = io.WriteString(w, `{"data":[]}`)
	case r.Method == "GET" && strings.HasPrefix(p, "/v1/checkout/sessions/"):
		id := strings.TrimPrefix(p, "/v1/checkout/sessions/")
		st := f.status[id]
		pay := "unpaid"
		if st == "complete" {
			pay = "paid"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "status": st, "payment_status": pay, "amount_total": 8500000, "currency": "idr"})
	default:
		w.WriteHeader(404)
	}
}

type shopFixture struct {
	h       http.Handler
	s       *Server
	f       *fakeCheckout
	tok     string
	pid     string
	file    Block
	product Block
}

func newShop(t *testing.T) shopFixture {
	t.Helper()
	testutil.Truncate(t, testPool)
	cfg := testConfig()
	cfg.PaymentsEncryptionKey = testPaymentsKey
	cfg.PublicBaseURL = "https://api.tautan.test"
	cfg.AppBaseURL = "https://tautan.test"
	s := New(cfg, testPool)
	s.Quiet = true
	s.cfg.UploadDir = t.TempDir()
	f := newFakeCheckout(t)
	u := f.srv.URL
	s.PaymentOptions = payments.Options{BaseURLs: map[string]string{payments.Xendit: u, payments.Midtrans: u, "midtrans_api": u, payments.Stripe: u}}
	h := s.Routes()
	a := signup(t, h, "seller")
	tab := editor(t, h, a.AccessToken).Tabs[0]
	fx := shopFixture{h: h, s: s, f: f, tok: a.AccessToken, pid: a.Profile.ID}
	fx.file = createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "file", "data": map[string]any{
		"title": "Lightroom presets", "url": "https://files.example/presets.zip", "file_name": "presets.zip", "price": 8500000,
	}})
	fx.product = createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "product", "data": map[string]any{
		"name": "E-book", "price": 1999, "currency": "USD", "images": []string{},
		"variants": []map[string]any{{"name": "Format", "options": []string{"PDF", "EPUB"}}},
		"action":   map[string]any{"kind": "checkout", "download": map[string]any{"url": "https://files.example/book.pdf"}},
	}})
	do(t, h, "PATCH", "/api/v1/profile", map[string]any{"is_published": true}, bearer(a.AccessToken)).expect(200)
	return fx
}

func (fx shopFixture) connect(t *testing.T, provider string, body map[string]any) {
	t.Helper()
	body["enabled"] = true
	var res putResp
	do(t, fx.h, "PUT", "/api/v1/payments/providers/"+provider, body, bearer(fx.tok)).expect(200).json(&res)
	if !res.Provider.Enabled {
		t.Fatalf("connect %s: %+v %+v", provider, res.Provider, res.Test)
	}
}

type createOrderResp struct {
	OrderID     string `json:"order_id"`
	Code        string `json:"code"`
	Token       string `json:"token"`
	CheckoutURL string `json:"checkout_url"`
}

type buyerOrderResp struct {
	Order struct {
		ID          string  `json:"id"`
		Code        string  `json:"code"`
		Status      string  `json:"status"`
		Amount      int64   `json:"amount"`
		CheckoutURL *string `json:"checkout_url"`
		Items       []struct {
			Name     string `json:"name"`
			FileName string `json:"file_name"`
			Variant  string `json:"variant"`
		} `json:"items"`
	} `json:"order"`
	Downloads []publicDownload `json:"downloads"`
}

func (fx shopFixture) order(t *testing.T, o createOrderResp) buyerOrderResp {
	t.Helper()
	var out buyerOrderResp
	do(t, fx.h, "GET", "/api/v1/public/seller/orders/"+o.OrderID+"?t="+o.Token, nil).expect(200).json(&out)
	return out
}

func stripeHook(t *testing.T, h http.Handler, pid, secret string, evt map[string]any) resp {
	t.Helper()
	b, _ := json.Marshal(evt)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "." + string(b)))
	return do(t, h, "POST", "/webhooks/stripe/"+pid, string(b), withHeader("Stripe-Signature", "t="+ts+",v1="+hex.EncodeToString(m.Sum(nil))))
}

func midtransHook(t *testing.T, h http.Handler, pid, key, orderID, status, code, gross, txID string) resp {
	t.Helper()
	sum := sha512.Sum512([]byte(orderID + code + gross + key))
	b, _ := json.Marshal(map[string]string{"order_id": orderID, "status_code": code, "gross_amount": gross,
		"transaction_status": status, "transaction_id": txID, "signature_key": hex.EncodeToString(sum[:])})
	return do(t, h, "POST", "/webhooks/midtrans/"+pid, string(b))
}

func TestOrderStripeFlow(t *testing.T) {
	fx := newShop(t)
	h := fx.h
	mailer := &captureMailer{}
	fx.s.mailer = mailer
	_, _ = fx.s.DeliverPending(context.Background()) // signup emails (verification) aren't under test here
	mailer.take()
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	fx.connect(t, "midtrans", map[string]any{"mode": "sandbox", "secret_key": "SB-Mid-server-good"})

	// Public read: the deliverables are gone, the price and providers are there.
	var page struct {
		Blocks   []Block       `json:"blocks"`
		Payments *pagePayments `json:"payments"`
	}
	raw := do(t, h, "GET", "/api/v1/public/seller", nil).expect(200).Body.String()
	_ = json.Unmarshal([]byte(raw), &page)
	if strings.Contains(raw, "files.example") {
		t.Fatalf("deliverable leaked in public read: %s", raw)
	}
	if page.Payments == nil || len(page.Payments.Providers) != 2 || page.Payments.Providers[0].Provider != "midtrans" ||
		strings.Join(page.Payments.Currencies, ",") != "IDR,USD" {
		t.Fatalf("payments = %+v", page.Payments)
	}
	if !strings.Contains(raw, `"price":8500000`) {
		t.Fatal("price missing from the public file card")
	}
	// The editor keeps them.
	if ed := do(t, h, "GET", "/api/v1/profile/editor", nil, bearer(fx.tok)).expect(200).Body.String(); !strings.Contains(ed, "presets.zip") || !strings.Contains(ed, "book.pdf") {
		t.Fatal("editor lost the deliverables")
	}

	buy := map[string]any{"block_id": fx.file.ID, "email": "Buyer@Example.com", "name": "Budi"}
	r := do(t, h, "POST", "/api/v1/public/seller/orders", buy).expect(400)
	if r.errCode() != "provider_required" || !strings.Contains(r.Body.String(), `"providers":["midtrans","stripe"]`) {
		t.Fatalf("no provider: %s", r.Body)
	}
	buy["provider"] = "paypal"
	do(t, h, "POST", "/api/v1/public/seller/orders", buy).expect(400)
	buy["provider"] = "stripe"
	var o createOrderResp
	do(t, h, "POST", "/api/v1/public/seller/orders", buy).expect(201).json(&o)
	if !strings.HasPrefix(o.CheckoutURL, "https://checkout.stripe.test/cs_test_") || len(o.Code) != 9 || o.Token == "" {
		t.Fatalf("order = %+v", o)
	}
	form, _ := url.ParseQuery(fx.f.last["stripe"])
	if form.Get("line_items[0][price_data][unit_amount]") != "8500000" || form.Get("client_reference_id") != o.OrderID ||
		form.Get("success_url") != "https://tautan.test/seller/orders/"+o.OrderID+"?t="+o.Token ||
		form.Get("customer_email") != "buyer@example.com" {
		t.Fatalf("stripe request = %v", form)
	}

	// Pending: the page offers the checkout link, no downloads yet.
	got := fx.order(t, o)
	if got.Order.Status != "pending" || got.Order.CheckoutURL == nil || len(got.Downloads) != 0 || got.Order.Items[0].FileName != "presets.zip" {
		t.Fatalf("pending order = %+v", got)
	}
	do(t, h, "GET", "/api/v1/public/seller/orders/"+o.OrderID+"?t=wrong", nil).expect(404)

	// Webhook: bad signature, then the real one, then a duplicate delivery.
	ref := strings.TrimPrefix(o.CheckoutURL, "https://checkout.stripe.test/")
	completed := map[string]any{"id": "evt_1", "type": "checkout.session.completed", "livemode": false, "data": map[string]any{"object": map[string]any{
		"id": ref, "status": "complete", "payment_status": "paid", "client_reference_id": o.OrderID,
		"amount_total": 8500000, "currency": "idr", "payment_intent": "pi_1"}}}
	stripeHook(t, h, fx.pid, "whsec_wrong", completed).expect(401)
	if res := stripeHook(t, h, fx.pid, "whsec_s", completed).expect(200).Body.String(); !strings.Contains(res, `"paid"`) {
		t.Fatalf("webhook = %s", res)
	}
	if res := stripeHook(t, h, fx.pid, "whsec_s", completed).expect(200).Body.String(); !strings.Contains(res, "duplicate") {
		t.Fatalf("duplicate = %s", res)
	}
	// A live-mode event against sandbox keys is ignored.
	live := map[string]any{"id": "evt_live", "type": "checkout.session.expired", "livemode": true, "data": map[string]any{"object": map[string]any{"id": ref, "status": "expired"}}}
	if res := stripeHook(t, h, fx.pid, "whsec_s", live).expect(200).Body.String(); !strings.Contains(res, "mode_mismatch") {
		t.Fatalf("mode mismatch = %s", res)
	}

	got = fx.order(t, o)
	if got.Order.Status != "paid" || got.Order.CheckoutURL != nil || len(got.Downloads) != 1 {
		t.Fatalf("paid order = %+v", got)
	}
	// One receipt, to the buyer, with the order link; the duplicate sent nothing.
	if _, err := fx.s.DeliverPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	sent := mailer.take()
	if len(sent) != 1 || sent[0].To != "buyer@example.com" || !strings.Contains(sent[0].Subject, o.Code) ||
		!strings.Contains(sent[0].Text, "https://tautan.test/seller/orders/"+o.OrderID+"?t="+o.Token) || !strings.Contains(sent[0].Text, "Rp 85.000") ||
		strings.Contains(sent[0].Text, "files.example") {
		t.Fatalf("receipt = %+v", sent)
	}
	dl := got.Downloads[0]
	path := strings.TrimPrefix(dl.URL, "https://api.tautan.test")
	rr := do(t, h, "GET", path, nil).expect(302)
	if rr.Header().Get("Location") != "https://files.example/presets.zip" {
		t.Fatalf("download redirect = %q", rr.Header().Get("Location"))
	}
	// Tampered or forged tokens fail.
	do(t, h, "GET", path[:len(path)-2]+"xx", nil).expect(403)
	do(t, h, "GET", "/api/v1/public/seller/orders/download/1."+o.OrderID+".0.sig", nil).expect(403)

	// The buyer joined Audience (not subscribed).
	var aud AudienceListTest
	do(t, h, "GET", "/api/v1/audience", nil, bearer(fx.tok)).expect(200).json(&aud)
	if len(aud.Items) != 1 || aud.Items[0].Email != "buyer@example.com" || aud.Items[0].Subscribed {
		t.Fatalf("audience = %+v", aud)
	}

	// Refund by payment_intent: downloads stop.
	refund := map[string]any{"id": "evt_2", "type": "charge.refunded", "data": map[string]any{"object": map[string]any{"payment_intent": "pi_1", "refunded": true, "amount": 8500000, "currency": "idr"}}}
	stripeHook(t, h, fx.pid, "whsec_s", refund).expect(200)
	if got = fx.order(t, o); got.Order.Status != "refunded" || len(got.Downloads) != 0 {
		t.Fatalf("refunded order = %+v", got)
	}
	if c := do(t, h, "GET", path, nil).expect(403).errCode(); c != "not_paid" {
		t.Fatalf("download after refund: %s", c)
	}
	// A late "completed" can't un-refund.
	completed["id"] = "evt_3"
	stripeHook(t, h, fx.pid, "whsec_s", completed).expect(200)
	if got = fx.order(t, o); got.Order.Status != "refunded" {
		t.Fatalf("status went backwards: %s", got.Order.Status)
	}
	_, _ = fx.s.DeliverPending(context.Background())
	if extra := mailer.take(); len(extra) != 0 {
		t.Fatalf("extra emails: %+v", extra)
	}

	// Lookup by email + code, typed loosely.
	var lk struct {
		OrderID string `json:"order_id"`
		Token   string `json:"token"`
	}
	code := strings.ToLower(strings.ReplaceAll(o.Code, "-", ""))
	do(t, h, "POST", "/api/v1/public/seller/orders/lookup", map[string]any{"email": " BUYER@example.com", "code": code}).expect(200).json(&lk)
	if lk.OrderID != o.OrderID || lk.Token != o.Token {
		t.Fatalf("lookup = %+v", lk)
	}
	do(t, h, "POST", "/api/v1/public/seller/orders/lookup", map[string]any{"email": "other@example.com", "code": code}).expect(404)
}

// AudienceListTest decodes GET /audience for these tests.
type AudienceListTest struct {
	Items []struct {
		Email      string `json:"email"`
		Subscribed bool   `json:"subscribed"`
	} `json:"items"`
}

func TestOrderMidtransWebhooks(t *testing.T) {
	fx := newShop(t)
	h := fx.h
	key := "SB-Mid-server-good"
	fx.connect(t, "midtrans", map[string]any{"mode": "sandbox", "secret_key": key})

	var o createOrderResp
	do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "a@example.com"}).expect(201).json(&o)
	if fx.f.last["midtrans_notify"] != "https://api.tautan.test/webhooks/midtrans/"+fx.pid || !strings.Contains(fx.f.last["midtrans"], `"gross_amount":85000`) {
		t.Fatalf("midtrans request: %s / %s", fx.f.last["midtrans_notify"], fx.f.last["midtrans"])
	}
	// Forged signature.
	midtransHook(t, h, fx.pid, "SB-Mid-server-other", o.OrderID, "settlement", "200", "85000.00", "tx1").expect(401)
	// Blocker: a genuinely signed notification with its (unsigned)
	// transaction_status edited to settlement. Midtrans's API says pending.
	midtransHook(t, h, fx.pid, key, o.OrderID, "settlement", "201", "85000.00", "tx1").expect(200)
	midtransHook(t, h, fx.pid, key, o.OrderID, "settlement", "200", "85000.00", "tx1").expect(200)
	if st := fx.order(t, o).Order.Status; st != "pending" {
		t.Fatalf("tampered status marked %s", st)
	}
	// The provider reports a different amount: refused.
	fx.f.set(o.OrderID, "settlement")
	fx.f.setAmount(o.OrderID, "1000.00")
	midtransHook(t, h, fx.pid, key, o.OrderID, "settlement", "200", "85000.00", "tx1").expect(200)
	if st := fx.order(t, o).Order.Status; st != "pending" {
		t.Fatalf("mismatched amount marked %s", st)
	}
	var note string
	_ = testPool.QueryRow(context.Background(), `SELECT status FROM payment_events WHERE order_id = $1 ORDER BY id DESC LIMIT 1`, o.OrderID).Scan(&note)
	if note != "amount_mismatch" {
		t.Fatalf("event note = %q", note)
	}
	// Expired, then a late payment wins.
	fx.f.setAmount(o.OrderID, "85000.00")
	fx.f.set(o.OrderID, "expire")
	midtransHook(t, h, fx.pid, key, o.OrderID, "expire", "407", "85000.00", "tx2").expect(200)
	if st := fx.order(t, o).Order.Status; st != "expired" {
		t.Fatalf("status = %s", st)
	}
	fx.f.set(o.OrderID, "settlement")
	midtransHook(t, h, fx.pid, key, o.OrderID, "settlement", "200", "85000.00", "tx2").expect(200)
	if st := fx.order(t, o).Order.Status; st != "paid" {
		t.Fatalf("late payment status = %s", st)
	}
	// Replaying it is a duplicate; an expiry after payment is ignored.
	if res := midtransHook(t, h, fx.pid, key, o.OrderID, "settlement", "200", "85000.00", "tx2").expect(200).Body.String(); !strings.Contains(res, "duplicate") {
		t.Fatalf("replay = %s", res)
	}
	fx.f.set(o.OrderID, "expire")
	midtransHook(t, h, fx.pid, key, o.OrderID, "expire", "407", "85000.00", "tx3").expect(200)
	if st := fx.order(t, o).Order.Status; st != "paid" {
		t.Fatalf("status went backwards: %s", st)
	}
	// Unknown order (Midtrans has never heard of it): acknowledged.
	if res := midtransHook(t, h, fx.pid, key, "11111111-1111-4111-8111-111111111111", "settlement", "200", "85000.00", "tx9").expect(200).Body.String(); !strings.Contains(res, "unknown_order") {
		t.Fatalf("unknown = %s", res)
	}
	// The provider can't be reached: 502 so Midtrans retries.
	fx.s.PaymentOptions.BaseURLs["midtrans_api"] = "http://127.0.0.1:1"
	midtransHook(t, h, fx.pid, key, o.OrderID, "settlement", "200", "85000.00", "tx4").expect(502)
	fx.s.PaymentOptions.BaseURLs["midtrans_api"] = fx.f.srv.URL
	// Webhook for a provider the page hasn't connected, or a bad profile id.
	do(t, h, "POST", "/webhooks/xendit/"+fx.pid, `{}`).expect(404)
	do(t, h, "POST", "/webhooks/xendit/not-a-uuid", `{}`).expect(404)
	do(t, h, "POST", "/webhooks/paypal/"+fx.pid, `{}`).expect(404)
	// USD can't go through Midtrans.
	if c := do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.product.ID, "options": map[string]string{"Format": "PDF"}, "email": "a@example.com"}).expect(409).errCode(); c != "payments_unavailable" {
		t.Fatalf("USD on midtrans: %s", c)
	}
}

func TestOrderXenditReconcileAndProduct(t *testing.T) {
	fx := newShop(t)
	h := fx.h
	fx.connect(t, "xendit", map[string]any{"mode": "sandbox", "secret_key": "xnd_development_good", "webhook_secret": "cb"})

	// Product with variants: the choice is required and checked.
	buy := map[string]any{"block_id": fx.product.ID, "email": "b@example.com"}
	do(t, h, "POST", "/api/v1/public/seller/orders", buy).expect(400)
	buy["options"] = map[string]string{"Format": "DOCX"}
	do(t, h, "POST", "/api/v1/public/seller/orders", buy).expect(400)
	buy["options"] = map[string]string{"Format": "EPUB"}
	var o createOrderResp
	do(t, h, "POST", "/api/v1/public/seller/orders", buy).expect(201).json(&o)
	if !strings.Contains(fx.f.last["xendit"], `"amount":19.99`) || !strings.Contains(fx.f.last["xendit"], `"currency":"USD"`) {
		t.Fatalf("xendit body = %s", fx.f.last["xendit"])
	}
	got := fx.order(t, o)
	if got.Order.Items[0].Variant != "Format: EPUB" || got.Order.Amount != 1999 {
		t.Fatalf("order = %+v", got.Order)
	}
	// No webhook arrives; the order page asks Xendit (rate limited per order).
	ref := strings.TrimPrefix(*got.Order.CheckoutURL, "https://checkout.xendit.test/")
	fx.f.set(ref, "PAID")
	if st := fx.order(t, o).Order.Status; st != "pending" {
		t.Fatalf("reconciled too soon: %s", st) // checked a moment ago
	}
	_, _ = testPool.Exec(context.Background(), `UPDATE orders SET last_checked_at = now() - interval '1 minute' WHERE id = $1`, o.OrderID)
	// The fake reports 85000 IDR for every invoice: a mismatch, so it stays pending.
	if st := fx.order(t, o).Order.Status; st != "pending" {
		t.Fatalf("mismatched reconcile: %s", st)
	}

	// IDR file through Xendit: reconcile marks it paid.
	var o2 createOrderResp
	do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "c@example.com"}).expect(201).json(&o2)
	got = fx.order(t, o2)
	fx.f.set(strings.TrimPrefix(*got.Order.CheckoutURL, "https://checkout.xendit.test/"), "SETTLED")
	_, _ = testPool.Exec(context.Background(), `UPDATE orders SET last_checked_at = NULL WHERE id = $1`, o2.OrderID)
	if got = fx.order(t, o2); got.Order.Status != "paid" || len(got.Downloads) != 1 {
		t.Fatalf("reconciled = %+v", got)
	}

	// Xendit webhook: token checked.
	do(t, h, "POST", "/webhooks/xendit/"+fx.pid, `{"id":"inv_x","external_id":"`+o.OrderID+`","status":"PAID"}`, withHeader("x-callback-token", "nope")).expect(401)

	// Owner views.
	var list struct {
		Items   []Order          `json:"items"`
		Counts  map[string]int   `json:"counts"`
		Revenue map[string]int64 `json:"revenue"`
	}
	do(t, h, "GET", "/api/v1/orders", nil, bearer(fx.tok)).expect(200).json(&list)
	if len(list.Items) != 2 || list.Counts["all"] != 2 || list.Counts["paid"] != 1 || list.Counts["pending"] != 1 || list.Revenue["IDR"] != 8500000 {
		t.Fatalf("list = %+v", list)
	}
	do(t, h, "GET", "/api/v1/orders?status=paid&q=c@", nil, bearer(fx.tok)).expect(200).json(&list)
	if len(list.Items) != 1 || list.Items[0].BuyerEmail != "c@example.com" {
		t.Fatalf("filtered = %+v", list.Items)
	}
	do(t, h, "GET", "/api/v1/orders?status=bogus", nil, bearer(fx.tok)).expect(400)
	csv := do(t, h, "GET", "/api/v1/orders.csv", nil, bearer(fx.tok)).expect(200).Body.String()
	if !strings.Contains(csv, "Created at,Order,Status") || !strings.Contains(csv, "E-book (Format: EPUB)") || !strings.Contains(csv, "85000.00,IDR,xendit,sandbox") {
		t.Fatalf("csv = %s", csv)
	}
	var detail struct {
		Order        Order        `json:"order"`
		Events       []orderEvent `json:"events"`
		OrderPageURL string       `json:"order_page_url"`
	}
	do(t, h, "GET", "/api/v1/orders/"+o2.OrderID, nil, bearer(fx.tok)).expect(200).json(&detail)
	if detail.Order.Status != "paid" || !strings.HasPrefix(detail.OrderPageURL, "https://tautan.test/seller/orders/"+o2.OrderID) {
		t.Fatalf("detail = %+v", detail)
	}
	if strings.Contains(do(t, h, "GET", "/api/v1/orders/"+o2.OrderID, nil, bearer(fx.tok)).Body.String(), "files.example") {
		t.Fatal("owner view should carry file names, not URLs")
	}
	do(t, h, "POST", "/api/v1/orders/"+o.OrderID+"/refresh", nil, bearer(fx.tok)).expect(200)
	other := signup(t, h, "other")
	do(t, h, "GET", "/api/v1/orders/"+o2.OrderID, nil, bearer(other.AccessToken)).expect(404)
}

func TestOrderGuards(t *testing.T) {
	fx := newShop(t)
	h := fx.h
	// No provider on: buy is refused.
	if c := do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "a@example.com"}).expect(409).errCode(); c != "payments_unavailable" {
		t.Fatalf("no providers: %s", c)
	}
	if p := do(t, h, "GET", "/api/v1/public/seller", nil).expect(200).Body.String(); !strings.Contains(p, `"payments":null`) {
		t.Fatal("payments should be null with nothing on")
	}
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "nope"}).expect(400)
	do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": "not-a-uuid", "email": "a@example.com"}).expect(404)
	// A card without a price isn't for sale.
	tab := editor(t, h, fx.tok).Tabs[0]
	free := createBlock(t, h, fx.tok, map[string]any{"tab_id": tab.ID, "type": "file", "data": map[string]any{"title": "Free", "url": "https://files.example/free.pdf"}})
	if c := do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": free.ID, "email": "a@example.com"}).expect(400).errCode(); c != "not_for_sale" {
		t.Fatalf("free file: %s", c)
	}
	if p := do(t, h, "GET", "/api/v1/public/seller", nil).Body.String(); !strings.Contains(p, "free.pdf") {
		t.Fatal("free file url should stay public")
	}
	// A gated priced card needs unlocking first.
	do(t, h, "PATCH", "/api/v1/blocks/"+fx.file.ID, map[string]any{"gate": map[string]any{"kind": "age_18"}}, bearer(fx.tok)).expect(200)
	if c := do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "a@example.com"}).expect(403).errCode(); c != "locked" {
		t.Fatalf("gated: %s", c)
	}
	// Unlocking a gated priced card still doesn't reveal the file.
	if u := do(t, h, "POST", "/api/v1/public/seller/blocks/"+fx.file.ID+"/unlock", map[string]any{"confirm_age": true}).expect(200).Body.String(); strings.Contains(u, "files.example/presets.zip") {
		t.Fatalf("unlock leaked the file: %s", u)
	}
	// Provider failure: the order fails and the creator sees why in Settings.
	fx.s.PaymentOptions.BaseURLs[payments.Stripe] = "http://127.0.0.1:1"
	if c := do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.product.ID, "options": map[string]string{"Format": "PDF"}, "email": "a@example.com"}).expect(502).errCode(); c != "checkout_failed" {
		t.Fatalf("provider down: %s", c)
	}
	var list providersResp
	do(t, h, "GET", "/api/v1/payments/providers", nil, bearer(fx.tok)).expect(200).json(&list)
	if le := list.Providers[2].LastError; le == nil || !strings.Contains(le.Message, "checkout failed") {
		t.Fatalf("last_error = %+v", le)
	}
}

func TestPaidCardValidation(t *testing.T) {
	_, h := newTestServer(t)
	a := signup(t, h, "val")
	tab := editor(t, h, a.AccessToken).Tabs[0]
	bad := func(typ string, data map[string]any, field string) {
		t.Helper()
		r := do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": tab.ID, "type": typ, "data": data}, bearer(a.AccessToken)).expect(400)
		if !strings.Contains(r.Body.String(), field) {
			t.Errorf("%s %v: want error on %s, got %s", typ, data, field, r.Body)
		}
	}
	bad("file", map[string]any{"title": "x", "url": "https://f.example/a", "price": 0}, "price")
	bad("file", map[string]any{"title": "x", "url": "https://f.example/a", "price": 150}, "price") // sen in IDR
	bad("file", map[string]any{"title": "x", "url": "https://f.example/a", "price": 100, "currency": "SGD"}, "currency")
	bad("product", map[string]any{"name": "x", "action": map[string]any{"kind": "checkout", "download": map[string]any{"url": "https://f.example/a"}}}, "price")
	bad("product", map[string]any{"name": "x", "price": 1000, "action": map[string]any{"kind": "checkout"}}, "download")
	ok := createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "product", "data": map[string]any{
		"name": "x", "price": 1000, "action": map[string]any{"kind": "url", "url": "https://x.example", "download": map[string]any{"url": "https://f.example/a"}}}})
	if strings.Contains(string(ok.Data), "download") {
		t.Fatalf("download kept on a url action: %s", ok.Data)
	}
	f := createBlock(t, h, a.AccessToken, map[string]any{"tab_id": tab.ID, "type": "file", "data": map[string]any{"title": "x", "url": "https://f.example/a", "price": 100}})
	if !strings.Contains(string(f.Data), `"currency":"IDR"`) {
		t.Fatalf("currency not defaulted: %s", f.Data)
	}
}

func TestOrderLocalDownload(t *testing.T) {
	fx := newShop(t)
	h := fx.h
	pdf := []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF")
	req := multipartReq(t, "file", "Guide.pdf", pdf, fx.tok)
	req.URL.RawQuery = "kind=file"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var up struct {
		URL string `json:"url"`
	}
	resp{rec, t}.expect(201).json(&up)
	do(t, h, "PATCH", "/api/v1/blocks/"+fx.file.ID, map[string]any{"data": map[string]any{
		"title": "Guide", "url": up.URL, "file_name": "Guide.pdf", "price": 500000}}, bearer(fx.tok)).expect(200)
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	var o createOrderResp
	do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "d@example.com"}).expect(201).json(&o)
	ref := strings.TrimPrefix(o.CheckoutURL, "https://checkout.stripe.test/")
	stripeHook(t, h, fx.pid, "whsec_s", map[string]any{"id": "evt_l", "type": "checkout.session.completed", "data": map[string]any{"object": map[string]any{
		"id": ref, "status": "complete", "payment_status": "paid", "client_reference_id": o.OrderID, "amount_total": 500000, "currency": "idr"}}}).expect(200)
	got := fx.order(t, o)
	if len(got.Downloads) != 1 {
		t.Fatalf("downloads = %+v", got)
	}
	r := do(t, h, "GET", strings.TrimPrefix(got.Downloads[0].URL, "https://api.tautan.test"), nil).expect(200)
	if r.Body.String() != string(pdf) || !strings.Contains(r.Header().Get("Content-Disposition"), `filename="Guide.pdf"`) {
		t.Fatalf("download = %q %q", r.Header().Get("Content-Disposition"), r.Body.String())
	}
	// The limit is per order.
	_, _ = testPool.Exec(context.Background(), `UPDATE orders SET downloads = $2 WHERE id = $1`, o.OrderID, maxOrderDownloads)
	if c := do(t, h, "GET", strings.TrimPrefix(got.Downloads[0].URL, "https://api.tautan.test"), nil).expect(429).errCode(); c != "download_limit" {
		t.Fatalf("limit: %s", c)
	}
	// Expired links.
	fx.s.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	do(t, h, "GET", strings.TrimPrefix(got.Downloads[0].URL, "https://api.tautan.test"), nil).expect(410)
}

func TestOrderCodes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		c := newOrderCode()
		if len(c) != 9 || c[4] != '-' || strings.ContainsAny(c, "ILOU") || seen[c] {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if normCode("k7q2 m9xd") != "K7Q2-M9XD" || normCode("K7Q2-M9XO") != "K7Q2-M9X0" || normCode("short") != "" {
		t.Fatal("normCode")
	}
	for _, c := range []struct {
		cur, want payments.Status
		ok        bool
	}{
		{"pending", "paid", true}, {"pending", "expired", true}, {"pending", "refunded", false},
		{"expired", "paid", true}, {"failed", "paid", true}, {"expired", "failed", false},
		{"paid", "refunded", true}, {"paid", "expired", false}, {"refunded", "paid", false}, {"paid", "", false},
	} {
		if nextStatus(c.cur, c.want) != c.ok {
			t.Errorf("%s → %s = %v", c.cur, c.want, !c.ok)
		}
	}
}

func TestOrderXenditWebhookConfirms(t *testing.T) {
	fx := newShop(t)
	h := fx.h
	fx.connect(t, "xendit", map[string]any{"mode": "sandbox", "secret_key": "xnd_development_good", "webhook_secret": "cbtok"})
	var o createOrderResp
	do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "x@example.com"}).expect(201).json(&o)
	ref := strings.TrimPrefix(o.CheckoutURL, "https://checkout.xendit.test/")
	hook := func(body string) string {
		t.Helper()
		return do(t, h, "POST", "/webhooks/xendit/"+fx.pid, body, withHeader("x-callback-token", "cbtok")).expect(200).Body.String()
	}
	paidNoAmount := `{"id":"` + ref + `","external_id":"` + o.OrderID + `","status":"PAID"}`
	// Blocker: a PAID callback (token only, no amounts) while Xendit says pending.
	hook(paidNoAmount)
	if st := fx.order(t, o).Order.Status; st != "pending" {
		t.Fatalf("unconfirmed PAID marked %s", st)
	}
	// Xendit says PAID but reports no amount: still not paid.
	fx.f.set(ref, "PAID")
	fx.f.setAmount(ref, "0")
	hook(paidNoAmount)
	if st := fx.order(t, o).Order.Status; st != "pending" {
		t.Fatalf("PAID without amount marked %s", st)
	}
	// Confirmed with the right amount and currency: paid.
	fx.f.setAmount(ref, "85000")
	hook(paidNoAmount)
	if st := fx.order(t, o).Order.Status; st != "paid" {
		t.Fatalf("confirmed PAID = %s", st)
	}
	if res := hook(paidNoAmount); !strings.Contains(res, "duplicate") {
		t.Fatalf("replay = %s", res)
	}
}

func TestOrderStrictAmounts(t *testing.T) {
	fx := newShop(t)
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	var o createOrderResp
	do(t, fx.h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "s@example.com"}).expect(201).json(&o)
	ref := strings.TrimPrefix(o.CheckoutURL, "https://checkout.stripe.test/")
	for i, obj := range []map[string]any{
		{"payment_status": "paid", "client_reference_id": o.OrderID},                                             // no amount at all
		{"payment_status": "paid", "client_reference_id": o.OrderID, "amount_total": 8500000},                    // no currency
		{"payment_status": "paid", "client_reference_id": o.OrderID, "amount_total": 8500000, "currency": "usd"}, // wrong currency
		{"payment_status": "paid", "client_reference_id": o.OrderID, "amount_total": 100, "currency": "idr"},     // wrong amount
	} {
		obj["id"], obj["status"] = ref, "complete"
		stripeHook(t, fx.h, fx.pid, "whsec_s", map[string]any{"id": "evt_s" + strconv.Itoa(i), "type": "checkout.session.completed", "data": map[string]any{"object": obj}}).expect(200)
		if st := fx.order(t, o).Order.Status; st != "pending" {
			t.Fatalf("case %d marked %s", i, st)
		}
	}
}

func TestPrivateDeliverables(t *testing.T) {
	fx := newShop(t)
	h := fx.h
	upload := func(tok, kind string) string {
		t.Helper()
		req := multipartReq(t, "file", "Guide.pdf", []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF"), tok)
		req.URL.RawQuery = "kind=" + kind
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var up struct {
			URL string `json:"url"`
		}
		resp{rec, t}.expect(201).json(&up)
		return up.URL
	}
	pathOf := func(u string) string { return strings.TrimPrefix(u, "https://api.tautan.test") }

	// A public upload gets a price: the card now points at a private copy.
	pub := upload(fx.tok, "file")
	var b Block
	do(t, h, "PATCH", "/api/v1/blocks/"+fx.file.ID, map[string]any{"data": map[string]any{"title": "Guide", "url": pub, "price": 500000}}, bearer(fx.tok)).expect(200).json(&b)
	var d FileData
	_ = json.Unmarshal(b.Data, &d)
	if d.URL == pub || !strings.Contains(d.URL, "/uploads/p_") {
		t.Fatalf("not privatized: %s", d.URL)
	}
	do(t, h, "GET", pathOf(d.URL), nil).expect(404) // never served publicly
	// Saving again keeps the same private file.
	do(t, h, "PATCH", "/api/v1/blocks/"+fx.file.ID, map[string]any{"data": map[string]any{"title": "Guide 2", "url": d.URL, "price": 500000}}, bearer(fx.tok)).expect(200).json(&b)
	if !strings.Contains(string(b.Data), d.URL) {
		t.Fatalf("private url changed on re-save: %s", b.Data)
	}
	// A direct private upload works for checkout products, and isn't public.
	priv := upload(fx.tok, "private")
	if !strings.Contains(priv, "/uploads/p_") {
		t.Fatalf("private upload url = %s", priv)
	}
	do(t, h, "GET", pathOf(priv), nil).expect(404)
	do(t, h, "PATCH", "/api/v1/blocks/"+fx.product.ID, map[string]any{"data": map[string]any{
		"name": "E-book", "price": 1999, "currency": "USD", "images": []string{},
		"action": map[string]any{"kind": "checkout", "download": map[string]any{"url": priv}}}}, bearer(fx.tok)).expect(200)
	// Another seller can't reference it.
	other := signup(t, h, "thief")
	tab := editor(t, h, other.AccessToken).Tabs[0]
	r := do(t, h, "POST", "/api/v1/blocks", map[string]any{"tab_id": tab.ID, "type": "file", "data": map[string]any{"title": "x", "url": priv, "price": 100}}, bearer(other.AccessToken)).expect(400)
	if !strings.Contains(r.Body.String(), "file not found") {
		t.Fatalf("cross-profile private file: %s", r.Body)
	}
	// The buyer still gets it through the signed link; Range requests don't count.
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	var o createOrderResp
	do(t, h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "p@example.com"}).expect(201).json(&o)
	ref := strings.TrimPrefix(o.CheckoutURL, "https://checkout.stripe.test/")
	stripeHook(t, h, fx.pid, "whsec_s", map[string]any{"id": "evt_p", "type": "checkout.session.completed", "data": map[string]any{"object": map[string]any{
		"id": ref, "status": "complete", "payment_status": "paid", "client_reference_id": o.OrderID, "amount_total": 500000, "currency": "idr"}}}).expect(200)
	dl := pathOf(fx.order(t, o).Downloads[0].URL)
	do(t, h, "GET", dl, nil).expect(200)
	do(t, h, "GET", dl, nil, withHeader("Range", "bytes=10-")).expect(206)
	do(t, h, "GET", dl, nil, withHeader("Range", "bytes=20-30")).expect(206)
	var n int
	_ = testPool.QueryRow(context.Background(), `SELECT downloads FROM orders WHERE id = $1`, o.OrderID).Scan(&n)
	if n != 1 {
		t.Fatalf("downloads counted = %d, want 1", n)
	}
}

func TestReceiptCaps(t *testing.T) {
	fx := newShop(t)
	mailer := &captureMailer{}
	fx.s.mailer = mailer
	fx.s.receiptToLimiter = newRateLimiter(1, time.Hour)
	fx.connect(t, "stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good", "webhook_secret": "whsec_s"})
	_, _ = fx.s.DeliverPending(context.Background())
	mailer.take()
	for i := 0; i < 3; i++ {
		var o createOrderResp
		do(t, fx.h, "POST", "/api/v1/public/seller/orders", map[string]any{"block_id": fx.file.ID, "email": "same@example.com"}).expect(201).json(&o)
		ref := strings.TrimPrefix(o.CheckoutURL, "https://checkout.stripe.test/")
		stripeHook(t, fx.h, fx.pid, "whsec_s", map[string]any{"id": "evt_c" + strconv.Itoa(i), "type": "checkout.session.completed", "data": map[string]any{"object": map[string]any{
			"id": ref, "status": "complete", "payment_status": "paid", "client_reference_id": o.OrderID, "amount_total": 8500000, "currency": "idr"}}}).expect(200)
		if st := fx.order(t, o).Order.Status; st != "paid" {
			t.Fatalf("order %d = %s", i, st) // the cap never blocks the order itself
		}
	}
	_, _ = fx.s.DeliverPending(context.Background())
	if sent := mailer.take(); len(sent) != 1 {
		t.Fatalf("receipts sent = %d, want 1 (capped per recipient)", len(sent))
	}
}
