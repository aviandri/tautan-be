package payments

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestSealerRoundTripAndAAD(t *testing.T) {
	k := newKey(t)
	for _, enc := range []string{hex.EncodeToString(k), base64.StdEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k)} {
		s, err := NewSealer(enc, nil)
		if err != nil {
			t.Fatalf("key %q: %v", enc, err)
		}
		ct, err := s.Seal([]byte("sk_test_123"), []byte("p1|stripe|sandbox"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(ct, []byte("sk_test_123")) {
			t.Fatal("plaintext visible in ciphertext")
		}
		pt, stale, err := s.Open(ct, []byte("p1|stripe|sandbox"))
		if err != nil || stale || string(pt) != "sk_test_123" {
			t.Fatalf("open = %q %v %v", pt, stale, err)
		}
		if _, _, err := s.Open(ct, []byte("p2|stripe|sandbox")); err == nil {
			t.Fatal("opened with the wrong AAD")
		}
		ct[len(ct)-1] ^= 1
		if _, _, err := s.Open(ct, []byte("p1|stripe|sandbox")); err == nil {
			t.Fatal("opened a tampered ciphertext")
		}
	}
}

func TestSealerRotation(t *testing.T) {
	oldK, newK := hex.EncodeToString(newKey(t)), hex.EncodeToString(newKey(t))
	before, _ := NewSealer(oldK, nil)
	ct, _ := before.Seal([]byte("secret"), nil)

	after, err := NewSealer(newK, []string{oldK})
	if err != nil {
		t.Fatal(err)
	}
	pt, stale, err := after.Open(ct, nil)
	if err != nil || !stale || string(pt) != "secret" {
		t.Fatalf("open with retired key = %q %v %v", pt, stale, err)
	}
	withoutOld, _ := NewSealer(newK, nil)
	if _, _, err := withoutOld.Open(ct, nil); err == nil {
		t.Fatal("opened without the retired key")
	}
}

func TestSealerConfig(t *testing.T) {
	if s, err := NewSealer("", nil); s != nil || err != nil {
		t.Fatalf("empty key = %v %v, want disabled", s, err)
	}
	var none *Sealer
	if _, err := none.Seal([]byte("x"), nil); err != ErrNoKey {
		t.Fatalf("nil sealer err = %v", err)
	}
	for _, bad := range []string{"short", hex.EncodeToString(make([]byte, 16)), "not-base64-!!!"} {
		if _, err := NewSealer(bad, nil); err == nil {
			t.Fatalf("accepted bad key %q", bad)
		}
	}
}

func TestValidateCredentials(t *testing.T) {
	cases := []struct {
		name, mode string
		c          Credentials
		field      string // "" = valid
	}{
		{Xendit, Sandbox, Credentials{SecretKey: "xnd_development_abc", WebhookSecret: "tok"}, ""},
		{Xendit, Live, Credentials{SecretKey: "xnd_development_abc"}, "secret_key"},
		{Xendit, Sandbox, Credentials{SecretKey: "xnd_public_development_abc"}, "secret_key"},
		{Midtrans, Sandbox, Credentials{SecretKey: "SB-Mid-server-abc"}, ""},
		{Midtrans, Live, Credentials{SecretKey: "SB-Mid-server-abc"}, "secret_key"},
		{Midtrans, Sandbox, Credentials{SecretKey: "Mid-server-abc"}, "secret_key"},
		{Midtrans, Sandbox, Credentials{SecretKey: "SB-Mid-client-abc"}, "secret_key"},
		{Midtrans, Sandbox, Credentials{SecretKey: "SB-Mid-server-abc", WebhookSecret: "x"}, "webhook_secret"},
		{Stripe, Sandbox, Credentials{SecretKey: "sk_test_abc", WebhookSecret: "whsec_x"}, ""},
		{Stripe, Sandbox, Credentials{SecretKey: "rk_test_abc"}, ""},
		{Stripe, Live, Credentials{SecretKey: "sk_test_abc"}, "secret_key"},
		{Stripe, Sandbox, Credentials{SecretKey: "pk_test_abc"}, "secret_key"},
		{Stripe, Sandbox, Credentials{SecretKey: "sk_test_abc", WebhookSecret: "nope"}, "webhook_secret"},
		{Stripe, Sandbox, Credentials{SecretKey: ""}, "secret_key"},
		{Stripe, Sandbox, Credentials{SecretKey: "sk_test_a b"}, "secret_key"},
	}
	for _, c := range cases {
		errs := ValidateCredentials(c.name, c.mode, c.c)
		if c.field == "" && len(errs) > 0 {
			t.Errorf("%s/%s %+v: unexpected %v", c.name, c.mode, c.c, errs)
		}
		if c.field != "" && errs[c.field] == "" {
			t.Errorf("%s/%s %+v: want error on %s, got %v", c.name, c.mode, c.c, c.field, errs)
		}
	}
}

func TestHint(t *testing.T) {
	if got := Hint("sk_test_51Habcd1234"); got != "…1234" {
		t.Fatalf("hint = %q", got)
	}
	if Hint("") != "" {
		t.Fatal("empty secret should have no hint")
	}
}
