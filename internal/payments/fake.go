package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Fake is a development-only provider: its "hosted checkout" is a page the API
// serves itself (/dev/fake-checkout/{ref}) with Pay / Fail / Expire buttons,
// so the whole sell → buy → paid → download flow runs without provider keys.
//
// It is not in Names. The server offers it only when APP_ENV isn't production
// (the server builds it with NewFake; New refuses it), and it is sandbox-only.
const Fake = "fake"

// fakeStoreMax caps how many checkouts a FakeStore keeps (oldest dropped).
const fakeStoreMax = 5000

// FakeCheckout is one checkout created on the fake provider.
type FakeCheckout struct {
	Ref      string
	Checkout Checkout
	Status   Status
	// Amount is what the provider reports as paid (normally Checkout.Amount;
	// the checkout page can report a different amount to test mismatches).
	Amount  int64
	Created time.Time
}

// FakeStore holds fake checkouts in memory. A restart forgets them: their
// orders then stay pending (status checks answer ErrNotFound).
type FakeStore struct {
	mu    sync.Mutex
	m     map[string]*FakeCheckout
	order []string
}

func NewFakeStore() *FakeStore { return &FakeStore{m: map[string]*FakeCheckout{}} }

func (s *FakeStore) put(c *FakeCheckout) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[c.Ref] = c
	s.order = append(s.order, c.Ref)
	for len(s.order) > fakeStoreMax {
		delete(s.m, s.order[0])
		s.order = s.order[1:]
	}
}

// Get returns a copy of a checkout.
func (s *FakeStore) Get(ref string) (FakeCheckout, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[ref]
	if !ok {
		return FakeCheckout{}, false
	}
	return *c, true
}

// Settle records the outcome the buyer picked on the checkout page. amount is
// the amount reported as paid (ignored unless st is paid). Only a pending
// checkout can be settled; it reports whether anything changed.
func (s *FakeStore) Settle(ref string, st Status, amount int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[ref]
	if !ok || c.Status != StatusPending {
		return false
	}
	c.Status = st
	if st == StatusPaid {
		c.Amount = amount
	}
	return true
}

type fake struct {
	mode  string
	cred  Credentials
	store *FakeStore
	base  string // API public base URL; the checkout page lives under it
	now   func() time.Time
}

// NewFake returns the fake provider. base is the API's public base URL.
func NewFake(mode string, c Credentials, store *FakeStore, base string, o Options) (Provider, error) {
	if store == nil {
		return nil, &Error{Code: "provider_error", Message: "the fake provider is only available in development"}
	}
	if mode != Sandbox {
		return nil, &Error{Code: "provider_error", Message: "the fake provider is sandbox-only"}
	}
	return &fake{mode: mode, cred: c, store: store, base: strings.TrimRight(base, "/"), now: o.now}, nil
}

func (f *fake) Name() string { return Fake }
func (f *fake) Mode() string { return f.mode }

func (f *fake) CreateCheckout(_ context.Context, c Checkout) (Session, error) {
	var sum int64
	for _, it := range c.Items {
		sum += it.UnitAmount * int64(it.Quantity)
	}
	if sum != c.Amount || c.Amount <= 0 {
		return Session{}, &Error{Code: "provider_error", Message: "amount doesn't match the items"}
	}
	if !Supports(Fake, c.Currency) {
		return Session{}, &Error{Code: "unsupported_currency", Message: "unsupported currency"}
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return Session{}, err
	}
	ref := "fk_" + hex.EncodeToString(b)
	f.store.put(&FakeCheckout{Ref: ref, Checkout: c, Status: StatusPending, Created: f.now()})
	return Session{Ref: ref, URL: f.base + "/dev/fake-checkout/" + url.PathEscape(ref), ExpiresAt: c.ExpiresAt}, nil
}

// FakeNotification is the webhook body the checkout page delivers.
type FakeNotification struct {
	Ref     string `json:"ref"`
	OrderID string `json:"order_id"`
}

// VerifyWebhook parses a notification. It carries no status: it's only a
// trigger (Confirm), and GetStatus reads the outcome from the store, so a
// forged notification can't change anything.
func (f *fake) VerifyWebhook(_ *http.Request, body []byte) (Event, error) {
	var n FakeNotification
	if err := json.Unmarshal(body, &n); err != nil || !strings.HasPrefix(n.Ref, "fk_") {
		return Event{}, &Error{Code: "invalid_payload", Message: "not a fake notification"}
	}
	return Event{Key: "fake:" + n.Ref, OrderID: n.OrderID, Ref: n.Ref, Type: "fake.notification", Confirm: true}, nil
}

func (f *fake) GetStatus(_ context.Context, ref string) (Event, error) {
	c, ok := f.store.Get(ref)
	if !ok {
		return Event{}, ErrNotFound
	}
	ev := Event{Key: "fake:" + ref, OrderID: c.Checkout.OrderID, Ref: ref, Status: c.Status,
		Currency: strings.ToUpper(c.Checkout.Currency), Type: "fake." + string(c.Status)}
	if c.Status == StatusPaid {
		ev.Amount = c.Amount
	}
	return ev, nil
}

func (f *fake) TestConnection(context.Context) error { return nil }

// validateFake checks fake credentials: any key starting with fake_, sandbox only.
func validateFake(mode string, c Credentials, errs map[string]string) {
	if mode != Sandbox {
		errs["mode"] = "the fake provider is sandbox-only"
	}
	if !strings.HasPrefix(c.SecretKey, "fake_") {
		errs["secret_key"] = "use any key starting with fake_"
	}
	if c.WebhookSecret != "" {
		errs["webhook_secret"] = "the fake provider has no webhook secret; leave this empty"
	}
}
