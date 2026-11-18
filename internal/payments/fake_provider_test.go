package payments

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFakeProvider(t *testing.T) {
	if _, err := New(Fake, Sandbox, Credentials{SecretKey: "fake_x"}, Options{}); err == nil {
		t.Fatal("New must refuse the fake provider (the server builds it with NewFake outside production)")
	}
	if _, err := NewFake(Sandbox, Credentials{}, nil, "https://api.test", Options{}); err == nil {
		t.Fatal("NewFake without a store must fail")
	}
	store := NewFakeStore()
	if _, err := NewFake(Live, Credentials{}, store, "https://api.test", Options{}); err == nil {
		t.Fatal("the fake provider is sandbox-only")
	}
	p, err := NewFake(Sandbox, Credentials{SecretKey: "fake_x"}, store, "https://api.test/", Options{Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	c := sampleCheckout("IDR")
	sess, err := p.CreateCheckout(context.Background(), c)
	if err != nil || !strings.HasPrefix(sess.URL, "https://api.test/dev/fake-checkout/fk_") || sess.Ref == "" {
		t.Fatalf("session = %+v, %v", sess, err)
	}
	bad := c
	bad.Amount++
	if _, err := p.CreateCheckout(context.Background(), bad); err == nil {
		t.Fatal("amount must match the items")
	}
	ev, err := p.GetStatus(context.Background(), sess.Ref)
	if err != nil || ev.Status != StatusPending || ev.OrderID != c.OrderID || ev.Live != nil {
		t.Fatalf("pending status = %+v, %v", ev, err)
	}
	if !store.Settle(sess.Ref, StatusPaid, c.Amount) || store.Settle(sess.Ref, StatusFailed, 0) {
		t.Fatal("settle: first must change, second must not")
	}
	ev, _ = p.GetStatus(context.Background(), sess.Ref)
	if ev.Status != StatusPaid || ev.Amount != c.Amount || ev.Currency != "IDR" {
		t.Fatalf("paid status = %+v", ev)
	}
	if _, err := p.GetStatus(context.Background(), "fk_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ref: %v", err)
	}
	wh, err := p.VerifyWebhook(nil, []byte(`{"ref":"`+sess.Ref+`","status":"paid"}`))
	if err != nil || !wh.Confirm || wh.Status != "" || wh.Live != nil {
		t.Fatalf("webhook = %+v, %v", wh, err)
	}
	if _, err := p.VerifyWebhook(nil, []byte(`{"ref":"inv_1"}`)); err == nil {
		t.Fatal("non-fake refs must be rejected")
	}
	for _, tc := range []struct {
		mode string
		c    Credentials
		bad  string
	}{
		{Live, Credentials{SecretKey: "fake_x"}, "mode"},
		{Sandbox, Credentials{SecretKey: "sk_test_x"}, "secret_key"},
		{Sandbox, Credentials{SecretKey: "fake_x", WebhookSecret: "w"}, "webhook_secret"},
	} {
		if errs := ValidateCredentials(Fake, tc.mode, tc.c); errs[tc.bad] == "" {
			t.Errorf("%+v: want error on %s, got %v", tc, tc.bad, errs)
		}
	}
	if errs := ValidateCredentials(Fake, Sandbox, Credentials{SecretKey: "fake_x"}); len(errs) != 0 {
		t.Errorf("valid fake creds rejected: %v", errs)
	}
	if NeedsWebhookSecret(Fake) || Valid(Fake) {
		t.Error("fake needs no webhook secret and isn't a public provider name")
	}
}
