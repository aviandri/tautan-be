package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aviandri/tautan-be/internal/payments"
	"github.com/aviandri/tautan-be/internal/testutil"
)

const testPaymentsKey = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

// fakeProviders answers the three providers' "test connection" calls.
// A request authenticates when its key contains "good".
type fakeProviders struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits int
}

func newFakeProviders(t *testing.T) *fakeProviders {
	f := &fakeProviders{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.mu.Unlock()
		user, _, _ := r.BasicAuth()
		auth := user + r.Header.Get("Authorization")
		good := strings.Contains(auth, "good")
		switch {
		case r.URL.Path == "/v2/invoices": // Xendit
			if !good {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error_code":"INVALID_API_KEY","message":"API key is invalid"}`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/status"): // Midtrans
			if !good {
				_, _ = w.Write([]byte(`{"status_code":"401","status_message":"Unknown Merchant server_key/id"}`))
				return
			}
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"status_code":"404","status_message":"Transaction doesn't exist."}`))
		case r.URL.Path == "/v1/checkout/sessions": // Stripe
			if !good {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"message":"Invalid API Key provided"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newPaymentsServer(t *testing.T) (*Server, http.Handler, *fakeProviders) {
	t.Helper()
	testutil.Truncate(t, testPool)
	cfg := testConfig()
	cfg.PaymentsEncryptionKey = testPaymentsKey
	cfg.PublicBaseURL = "https://api.tautan.test"
	s := New(cfg, testPool)
	s.Quiet = true
	f := newFakeProviders(t)
	u := f.srv.URL
	s.PaymentOptions = payments.Options{BaseURLs: map[string]string{
		payments.Xendit: u, payments.Midtrans: u, "midtrans_api": u, payments.Stripe: u}}
	return s, s.Routes(), f
}

type providersResp struct {
	Available bool               `json:"available"`
	Providers []providerSettings `json:"providers"`
}

type putResp struct {
	Provider providerSettings `json:"provider"`
	Test     *providerTest    `json:"test"`
}

func TestPaymentProvidersFlow(t *testing.T) {
	_, h, f := newPaymentsServer(t)
	a := signup(t, h, "seller")
	tok := bearer(a.AccessToken)
	pid := a.Profile.ID

	var list providersResp
	do(t, h, "GET", "/api/v1/payments/providers", nil, tok).expect(200).json(&list)
	// 4 = the three real providers plus the dev-only fake (not in production).
	if !list.Available || len(list.Providers) != 4 || list.Providers[0].Provider != "xendit" || list.Providers[0].Configured {
		t.Fatalf("initial list = %+v", list)
	}
	if got := list.Providers[2].WebhookURL; got != "https://api.tautan.test/webhooks/stripe/"+pid {
		t.Fatalf("webhook url = %q", got)
	}
	if c := list.Providers[1].Currencies; len(c) != 1 || c[0] != "IDR" || list.Providers[1].NeedsWebhookSec {
		t.Fatalf("midtrans view = %+v", list.Providers[1])
	}

	// Validation: wrong mode for the key, publishable key, bad mode.
	r := do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"mode": "live", "secret_key": "sk_test_good1234"}, tok).expect(400)
	if r.errCode() != "invalid_data" || !strings.Contains(r.Body.String(), "secret_key") {
		t.Fatalf("mode mismatch: %s", r.Body)
	}
	do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"mode": "test"}, tok).expect(400)
	do(t, h, "PUT", "/api/v1/payments/providers/paypal", map[string]any{"mode": "live"}, tok).expect(404)
	// Enabling needs the webhook secret.
	if c := do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_good1234", "enabled": true}, tok).expect(400).errCode(); c != "webhook_secret_required" {
		t.Fatalf("code = %s", c)
	}

	// Save and auto-test.
	var put putResp
	do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{
		"mode": "sandbox", "secret_key": "sk_test_good1234", "webhook_secret": "whsec_abcd9876", "enabled": true,
	}, tok).expect(200).json(&put)
	p := put.Provider
	if put.Test == nil || !put.Test.OK || !p.Configured || !p.Enabled || *p.Mode != "sandbox" ||
		*p.SecretKeyHint != "…1234" || *p.WebhookSecretHint != "…9876" || p.VerifiedAt == nil || p.LastError != nil {
		t.Fatalf("put = %+v / %+v", p, put.Test)
	}
	// Keys never come back.
	body := do(t, h, "GET", "/api/v1/payments/providers", nil, tok).expect(200).Body.String()
	if strings.Contains(body, "sk_test_good1234") || strings.Contains(body, "whsec_abcd9876") {
		t.Fatalf("secret leaked: %s", body)
	}
	// And are encrypted at rest.
	var sealed []byte
	if err := testPool.QueryRow(context.Background(), `SELECT credentials FROM payment_providers WHERE profile_id = $1 AND provider = 'stripe'`, pid).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("sk_test_good1234")) || bytes.Contains(sealed, []byte("whsec_")) {
		t.Fatal("credentials stored in plaintext")
	}

	// Toggling enabled without keys keeps them and doesn't re-test.
	hits := f.hits
	do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"enabled": false}, tok).expect(200).json(&put)
	if put.Provider.Enabled || put.Test != nil || f.hits != hits || *put.Provider.SecretKeyHint != "…1234" {
		t.Fatalf("toggle = %+v test=%v hits %d→%d", put.Provider, put.Test, hits, f.hits)
	}
	do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"enabled": true}, tok).expect(200).json(&put)
	if !put.Provider.Enabled {
		t.Fatal("re-enable failed")
	}

	// A bad key is saved but disabled, with the provider's message.
	do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"secret_key": "sk_test_bad00000"}, tok).expect(200).json(&put)
	if put.Test == nil || put.Test.OK || put.Test.Code != "invalid_credentials" || put.Provider.Enabled ||
		put.Provider.LastError == nil || put.Provider.VerifiedAt != nil || *put.Provider.WebhookSecretHint != "…9876" {
		t.Fatalf("bad key = %+v / %+v", put.Provider, put.Test)
	}
	// Enabling an unverified provider is refused by the re-test.
	do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"enabled": true}, tok).expect(200).json(&put)
	if put.Provider.Enabled {
		t.Fatal("enabled with a failing key")
	}

	// Switching mode needs the new mode's key; stored keys don't carry over.
	r = do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"mode": "live"}, tok).expect(400)
	if !strings.Contains(r.Body.String(), "switching mode") {
		t.Fatalf("switch mode: %s", r.Body)
	}
	do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"mode": "live", "secret_key": "rk_live_good5555"}, tok).expect(200).json(&put)
	if *put.Provider.Mode != "live" || put.Provider.WebhookSecretHint != nil || !put.Test.OK {
		t.Fatalf("live = %+v", put.Provider)
	}

	// Midtrans: no webhook secret, can be enabled straight away.
	do(t, h, "PUT", "/api/v1/payments/providers/midtrans", map[string]any{"mode": "sandbox", "secret_key": "SB-Mid-server-good", "enabled": true}, tok).expect(200).json(&put)
	if !put.Provider.Enabled || !put.Test.OK {
		t.Fatalf("midtrans = %+v %+v", put.Provider, put.Test)
	}
	// Xendit with a wrong key.
	do(t, h, "PUT", "/api/v1/payments/providers/xendit", map[string]any{"mode": "sandbox", "secret_key": "xnd_development_bad", "webhook_secret": "cb", "enabled": true}, tok).expect(200).json(&put)
	if put.Provider.Enabled || put.Test.Code != "invalid_credentials" || put.Test.Message != "API key is invalid" {
		t.Fatalf("xendit = %+v %+v", put.Provider, put.Test)
	}

	// Re-test stored keys.
	var tr putResp
	do(t, h, "POST", "/api/v1/payments/providers/midtrans/test", nil, tok).expect(200).json(&tr)
	if !tr.Test.OK || tr.Provider.VerifiedAt == nil {
		t.Fatalf("retest = %+v", tr)
	}
	do(t, h, "POST", "/api/v1/payments/providers/xendit/test", nil, tok).expect(200).json(&tr)
	if tr.Test.OK || tr.Provider.LastError == nil || tr.Provider.LastError.Code != "invalid_credentials" {
		t.Fatalf("xendit retest = %+v", tr)
	}

	// Other users can't see these settings.
	b := signup(t, h, "other")
	do(t, h, "GET", "/api/v1/payments/providers", nil, bearer(b.AccessToken)).expect(200).json(&list)
	for _, p := range list.Providers {
		if p.Configured {
			t.Fatalf("leaked to another profile: %+v", p)
		}
	}
	do(t, h, "POST", "/api/v1/payments/providers/stripe/test", nil, bearer(b.AccessToken)).expect(404)

	// Delete.
	do(t, h, "DELETE", "/api/v1/payments/providers/midtrans", nil, tok).expect(204)
	do(t, h, "GET", "/api/v1/payments/providers", nil, tok).expect(200).json(&list)
	if list.Providers[1].Configured {
		t.Fatal("midtrans still configured after delete")
	}
	do(t, h, "GET", "/api/v1/payments/providers", nil).expect(401)
}

func TestPaymentProvidersRowBinding(t *testing.T) {
	s, h, _ := newPaymentsServer(t)
	a := signup(t, h, "alice")
	b := signup(t, h, "bobby")
	put := map[string]any{"mode": "sandbox", "secret_key": "SB-Mid-server-good"}
	do(t, h, "PUT", "/api/v1/payments/providers/midtrans", put, bearer(a.AccessToken)).expect(200)
	do(t, h, "PUT", "/api/v1/payments/providers/midtrans", put, bearer(b.AccessToken)).expect(200)
	// Copying alice's ciphertext into bob's row must not decrypt (AAD binds it to the row).
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `UPDATE payment_providers SET credentials = (SELECT credentials FROM payment_providers WHERE profile_id = $1) WHERE profile_id = $2`,
		a.Profile.ID, b.Profile.ID); err != nil {
		t.Fatal(err)
	}
	if c := do(t, h, "POST", "/api/v1/payments/providers/midtrans/test", nil, bearer(b.AccessToken)).expect(409).errCode(); c != "credentials_unreadable" {
		t.Fatalf("code = %s", c)
	}
	_ = s
}

func TestPaymentProvidersKeyRotation(t *testing.T) {
	_, h, _ := newPaymentsServer(t)
	a := signup(t, h, "rotor")
	tok := bearer(a.AccessToken)
	do(t, h, "PUT", "/api/v1/payments/providers/midtrans", map[string]any{"mode": "sandbox", "secret_key": "SB-Mid-server-good"}, tok).expect(200)

	// Restart with a new key, the old one listed as retired: stored keys still work.
	cfg := testConfig()
	cfg.PaymentsEncryptionKey = hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	cfg.PaymentsEncryptionKeysOld = []string{testPaymentsKey}
	f := newFakeProviders(t)
	s2 := New(cfg, testPool)
	s2.Quiet = true
	s2.PaymentOptions = payments.Options{BaseURLs: map[string]string{payments.Midtrans: f.srv.URL, "midtrans_api": f.srv.URL}}
	h2 := s2.Routes()
	var tr putResp
	do(t, h2, "POST", "/api/v1/payments/providers/midtrans/test", nil, tok).expect(200).json(&tr)
	if !tr.Test.OK {
		t.Fatalf("after rotation = %+v", tr)
	}
}

func TestPaymentsUnavailableWithoutKey(t *testing.T) {
	_, h := newTestServer(t) // no PAYMENTS_ENCRYPTION_KEY
	a := signup(t, h, "nokey")
	var list providersResp
	do(t, h, "GET", "/api/v1/payments/providers", nil, bearer(a.AccessToken)).expect(200).json(&list)
	if list.Available || len(list.Providers) != 4 { // incl. the dev-only fake
		t.Fatalf("list = %+v", list)
	}
	if c := do(t, h, "PUT", "/api/v1/payments/providers/stripe", map[string]any{"mode": "sandbox", "secret_key": "sk_test_x"}, bearer(a.AccessToken)).expect(503).errCode(); c != "payments_unavailable" {
		t.Fatalf("code = %s", c)
	}
}
