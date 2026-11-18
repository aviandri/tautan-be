// Package payments talks to the payment providers a creator connects with
// their own account (Xendit Invoices, Midtrans Snap, Stripe Checkout).
//
// Tautan never holds funds: every call uses the creator's own API keys, and
// buyers pay on the provider's hosted page. Amounts are integers in minor units
// of the currency, as everywhere else in Tautan (Rp 85.000 = 8500000).
package payments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Provider names.
const (
	Xendit   = "xendit"
	Midtrans = "midtrans"
	Stripe   = "stripe"
)

// Names lists the supported providers in display order.
var Names = []string{Xendit, Midtrans, Stripe}

// Modes.
const (
	Sandbox = "sandbox"
	Live    = "live"
)

// Status is an order's payment status.
type Status string

const (
	StatusPending  Status = "pending"
	StatusPaid     Status = "paid"
	StatusExpired  Status = "expired"
	StatusFailed   Status = "failed"
	StatusRefunded Status = "refunded"
)

// Credentials are the creator's secrets for one provider. They are stored
// sealed and never sent to the browser.
type Credentials struct {
	// SecretKey: Xendit secret API key, Midtrans server key or Stripe secret
	// (or restricted) key.
	SecretKey string `json:"secret_key"`
	// WebhookSecret: Xendit callback verification token or Stripe webhook
	// signing secret (whsec_…). Midtrans signs with the server key, so it has none.
	WebhookSecret string `json:"webhook_secret,omitempty"`
}

// Item is one line on the provider's checkout page.
type Item struct {
	Name       string
	UnitAmount int64 // minor units
	Quantity   int
}

// Checkout describes the hosted checkout to create for one order.
type Checkout struct {
	OrderID     string // ours; sent as Xendit external_id, Midtrans order_id, Stripe client_reference_id
	Amount      int64  // minor units; must equal the sum of Items
	Currency    string // IDR or USD
	Description string
	Items       []Item
	BuyerEmail  string
	BuyerName   string
	SuccessURL  string
	CancelURL   string
	NotifyURL   string // our webhook URL; Midtrans gets it per transaction
	ExpiresAt   time.Time
}

// Session is a created hosted checkout.
type Session struct {
	Ref       string // invoice id / Midtrans order id / checkout session id
	URL       string // where to redirect the buyer
	ExpiresAt time.Time
}

// Event is a verified webhook notification, or the result of GetStatus.
type Event struct {
	Key        string // unique per provider; used to drop duplicate deliveries
	OrderID    string // our order id, when the payload carries it
	Ref        string // provider checkout ref (same as Session.Ref)
	PaymentRef string // Stripe payment_intent; refunds arrive keyed by it
	Status     Status // "" means the event doesn't change the order
	Amount     int64  // minor units, 0 when unknown
	Currency   string // upper case, "" when unknown
	Live       *bool  // set when the payload says which mode it came from
	Type       string // provider event type, for logs
	// Confirm means the payload's status isn't covered by the provider's
	// authentication (Midtrans signs only order_id, status_code and
	// gross_amount; Xendit has a static token and no body signature), so the
	// caller must ask GetStatus for the real state before acting on it.
	Confirm bool
}

// Provider is one connected provider account.
type Provider interface {
	Name() string
	Mode() string
	// CreateCheckout creates a hosted checkout and returns the redirect URL.
	CreateCheckout(ctx context.Context, c Checkout) (Session, error)
	// VerifyWebhook authenticates a webhook delivery and parses it.
	// It returns ErrBadSignature when authentication fails.
	VerifyWebhook(r *http.Request, body []byte) (Event, error)
	// GetStatus asks the provider for the current state of a checkout.
	GetStatus(ctx context.Context, ref string) (Event, error)
	// TestConnection checks that the keys work.
	TestConnection(ctx context.Context) error
}

// Options configure adapters. Zero values are fine for production.
type Options struct {
	HTTP *http.Client
	// BaseURLs overrides API hosts by provider name (tests point them at fakes).
	// Midtrans takes two: "midtrans" (Snap) and "midtrans_api" (Core API).
	BaseURLs map[string]string
	Now      func() time.Time
}

func (o Options) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) base(key, def string) string {
	if v := o.BaseURLs[key]; v != "" {
		return strings.TrimRight(v, "/")
	}
	return def
}

// New returns the adapter for a provider.
func New(name, mode string, c Credentials, o Options) (Provider, error) {
	if mode != Sandbox && mode != Live {
		return nil, fmt.Errorf("payments: unknown mode %q", mode)
	}
	switch name {
	case Xendit:
		return &xendit{mode: mode, cred: c, opt: o}, nil
	case Midtrans:
		return &midtrans{mode: mode, cred: c, opt: o}, nil
	case Stripe:
		return &stripe{mode: mode, cred: c, opt: o}, nil
	}
	return nil, fmt.Errorf("payments: unknown provider %q", name)
}

// Valid reports whether name is a supported provider.
func Valid(name string) bool {
	for _, n := range Names {
		if n == name {
			return true
		}
	}
	return false
}

// Currencies lists what each provider's hosted checkout accepts from us.
// Midtrans settles in IDR only.
func Currencies(name string) []string {
	if name == Midtrans {
		return []string{"IDR"}
	}
	return []string{"IDR", "USD"}
}

// Supports reports whether a provider accepts a currency.
func Supports(name, currency string) bool {
	for _, c := range Currencies(name) {
		if c == currency {
			return true
		}
	}
	return false
}

// AmountRange is the order total (minor units) a provider's hosted checkout
// takes in a currency, so the API can say so before redirecting. These are
// conservative bounds for one order; a provider may still narrow them per
// payment method (e.g. retail outlets from Rp 10.000), which its own page shows.
//   - Xendit invoices and Midtrans Snap: from Rp 1.000 (QRIS and e-wallets go
//     lower, but most channels start around here); up to Rp 1.000.000.000.
//   - Stripe: the US$0.50 card minimum (Rp 10.000 covers it at current
//     rates); up to 8 digits for USD and 12 for IDR (Stripe's amount limits).
//   - Xendit USD: from US$1.
func AmountRange(name, currency string) (min, max int64) {
	switch {
	case currency == "IDR" && name == Stripe:
		return 10_000_00, 9_999_999_999_99
	case currency == "IDR":
		return 1_000_00, 1_000_000_000_00
	case currency == "USD" && name == Stripe:
		return 50, 99_999_999
	case currency == "USD":
		return 1_00, 99_999_999
	}
	return 1, 0
}

// NeedsWebhookSecret reports whether a provider uses a separate webhook secret.
func NeedsWebhookSecret(name string) bool { return name != Midtrans && name != Fake }

// Error is a provider failure with a stable code for the API.
type Error struct {
	Code    string // invalid_credentials | provider_error | provider_unreachable | unsupported_currency
	Message string
	Status  int // HTTP status from the provider, 0 when there was no response
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// ErrBadSignature means a webhook failed authentication.
var ErrBadSignature = errors.New("payments: webhook signature is invalid")

// ErrNotFound means the provider doesn't know the checkout.
var ErrNotFound = errors.New("payments: checkout not found at provider")

// ValidateCredentials checks the shape of keys before they are stored, so a
// live key can't be saved in sandbox mode (or a publishable key instead of a
// secret one). It doesn't call the provider; TestConnection does.
// Field names in the returned map match the API (secret_key, webhook_secret).
func ValidateCredentials(name, mode string, c Credentials) map[string]string {
	errs := map[string]string{}
	sk, ws := c.SecretKey, c.WebhookSecret
	if sk == "" {
		errs["secret_key"] = "required"
	} else if len(sk) > 300 || strings.ContainsAny(sk, " \t\r\n") {
		errs["secret_key"] = "doesn't look like an API key"
	}
	if len(ws) > 300 || strings.ContainsAny(ws, " \t\r\n") {
		errs["webhook_secret"] = "doesn't look like a webhook secret"
	}
	if len(errs) > 0 {
		return errs
	}
	switch name {
	case Xendit:
		switch {
		case strings.HasPrefix(sk, "xnd_public_"):
			errs["secret_key"] = "this is a public key; use a secret key (xnd_development_… or xnd_production_…)"
		case mode == Sandbox && !strings.HasPrefix(sk, "xnd_development_"):
			errs["secret_key"] = "sandbox mode needs a test key starting with xnd_development_"
		case mode == Live && !strings.HasPrefix(sk, "xnd_production_"):
			errs["secret_key"] = "live mode needs a live key starting with xnd_production_"
		}
	case Midtrans:
		switch {
		case strings.Contains(sk, "-client-"):
			errs["secret_key"] = "this is the client key; use the server key"
		case mode == Live && strings.HasPrefix(sk, "SB-"):
			errs["secret_key"] = "this is a sandbox server key (SB-Mid-server-…); live mode needs the production key"
		case mode == Sandbox && strings.HasPrefix(sk, "Mid-server-"):
			errs["secret_key"] = "this is a production server key; sandbox mode needs the SB-Mid-server-… key"
		}
		if ws != "" {
			errs["webhook_secret"] = "Midtrans signs notifications with the server key; leave this empty"
		}
	case Fake:
		validateFake(mode, c, errs)
	case Stripe:
		switch {
		case strings.HasPrefix(sk, "pk_"):
			errs["secret_key"] = "this is a publishable key; use a secret key (sk_…) or a restricted key (rk_…)"
		case mode == Sandbox && !(strings.HasPrefix(sk, "sk_test_") || strings.HasPrefix(sk, "rk_test_")):
			errs["secret_key"] = "sandbox mode needs a test key (sk_test_… or rk_test_…)"
		case mode == Live && !(strings.HasPrefix(sk, "sk_live_") || strings.HasPrefix(sk, "rk_live_")):
			errs["secret_key"] = "live mode needs a live key (sk_live_… or rk_live_…)"
		}
		if ws != "" && !strings.HasPrefix(ws, "whsec_") {
			errs["webhook_secret"] = "the signing secret starts with whsec_"
		}
	}
	return errs
}

// Hint is what the dashboard shows for a stored secret: its last 4 characters.
func Hint(secret string) string {
	if secret == "" {
		return ""
	}
	r := []rune(secret)
	if len(r) <= 4 {
		return "…" + string(r[len(r)-1:])
	}
	return "…" + string(r[len(r)-4:])
}

// majorIDR converts minor units to whole rupiah, which Xendit and Midtrans
// require. It fails when the amount has sen.
func majorIDR(minor int64) (int64, error) {
	if minor%100 != 0 {
		return 0, &Error{Code: "invalid_amount", Message: "IDR amounts must be whole rupiah"}
	}
	return minor / 100, nil
}

// fromMajor converts a provider amount in major units to minor units.
func fromMajor(v float64) int64 {
	if v < 0 {
		return 0
	}
	return int64(v*100 + 0.5)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func boolPtr(b bool) *bool { return &b }
