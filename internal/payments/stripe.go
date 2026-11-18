package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// stripe uses Checkout Sessions in payment mode with inline price_data, so the
// creator doesn't need Products set up in Stripe.
// Docs: https://docs.stripe.com/api/checkout/sessions/create
// Webhooks are signed: Stripe-Signature "t=<ts>,v1=<hex HMAC-SHA256(secret, ts.body)>".
// IDR and USD are both two-decimal currencies in Stripe, so our minor units
// pass through unchanged.
type stripe struct {
	mode string
	cred Credentials
	opt  Options
}

// stripeTolerance is how old a signed webhook may be.
const stripeTolerance = 5 * time.Minute

func (s *stripe) Name() string { return Stripe }
func (s *stripe) Mode() string { return s.mode }

func (s *stripe) base() string { return s.opt.base(Stripe, "https://api.stripe.com") }

func (s *stripe) newReq(method, path string, form url.Values) (*http.Request, error) {
	var req *http.Request
	var err error
	if form != nil {
		req, err = http.NewRequest(method, s.base()+path, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequest(method, s.base()+path, nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cred.SecretKey)
	return req, nil
}

func stripeError(status int, body []byte, fallback string) error {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return providerError(status, e.Error.Message, fallback)
}

type stripeSession struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	URL               string            `json:"url"`
	Status            string            `json:"status"`         // open | complete | expired
	PaymentStatus     string            `json:"payment_status"` // paid | unpaid | no_payment_required
	ClientReferenceID string            `json:"client_reference_id"`
	Metadata          map[string]string `json:"metadata"`
	AmountTotal       int64             `json:"amount_total"`
	Currency          string            `json:"currency"`
	PaymentIntent     json.RawMessage   `json:"payment_intent"` // id string, or an object when expanded
	ExpiresAt         int64             `json:"expires_at"`
	Livemode          bool              `json:"livemode"`
}

func (ss stripeSession) paymentIntentID() string {
	var id string
	if json.Unmarshal(ss.PaymentIntent, &id) == nil {
		return id
	}
	var obj struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(ss.PaymentIntent, &obj)
	return obj.ID
}

func (ss stripeSession) orderID() string {
	if ss.ClientReferenceID != "" {
		return ss.ClientReferenceID
	}
	return ss.Metadata["order_id"]
}

func (s *stripe) CreateCheckout(ctx context.Context, c Checkout) (Session, error) {
	if !Supports(Stripe, c.Currency) {
		return Session{}, &Error{Code: "unsupported_currency", Message: "Stripe checkout supports IDR and USD here"}
	}
	f := url.Values{}
	f.Set("mode", "payment")
	f.Set("success_url", c.SuccessURL)
	f.Set("cancel_url", c.CancelURL)
	f.Set("client_reference_id", c.OrderID)
	f.Set("metadata[order_id]", c.OrderID)
	f.Set("payment_intent_data[metadata][order_id]", c.OrderID)
	if c.BuyerEmail != "" {
		f.Set("customer_email", c.BuyerEmail)
	}
	if c.Description != "" {
		f.Set("payment_intent_data[description]", truncate(c.Description, 1000))
	}
	cur := strings.ToLower(c.Currency)
	for i, it := range c.Items {
		p := "line_items[" + strconv.Itoa(i) + "]"
		f.Set(p+"[quantity]", strconv.Itoa(it.Quantity))
		f.Set(p+"[price_data][currency]", cur)
		f.Set(p+"[price_data][unit_amount]", strconv.FormatInt(it.UnitAmount, 10))
		f.Set(p+"[price_data][product_data][name]", truncate(it.Name, 250))
	}
	// Stripe allows 30 minutes to 24 hours.
	exp := c.ExpiresAt
	now := s.opt.now()
	if exp.Sub(now) < 31*time.Minute {
		exp = now.Add(31 * time.Minute)
	}
	if exp.Sub(now) > 24*time.Hour {
		exp = now.Add(24 * time.Hour)
	}
	f.Set("expires_at", strconv.FormatInt(exp.Unix(), 10))
	req, err := s.newReq("POST", "/v1/checkout/sessions", f)
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Idempotency-Key", "tautan-checkout-"+c.OrderID)
	var out stripeSession
	status, raw, err := call(ctx, s.opt.client(), req, &out)
	if err != nil {
		return Session{}, err
	}
	if status/100 != 2 {
		return Session{}, stripeError(status, raw, "Stripe couldn't create the checkout")
	}
	if out.ID == "" || out.URL == "" {
		return Session{}, &Error{Code: "provider_error", Message: "Stripe returned no checkout URL", Status: status}
	}
	return Session{Ref: out.ID, URL: out.URL, ExpiresAt: time.Unix(out.ExpiresAt, 0).UTC()}, nil
}

func (s *stripe) sessionEvent(ss stripeSession) Event {
	ev := Event{
		OrderID:    ss.orderID(),
		Ref:        ss.ID,
		PaymentRef: ss.paymentIntentID(),
		Amount:     ss.AmountTotal,
		Currency:   strings.ToUpper(ss.Currency),
		Live:       boolPtr(ss.Livemode),
	}
	switch {
	case ss.Status == "expired":
		ev.Status = StatusExpired
	case ss.PaymentStatus == "paid" || ss.PaymentStatus == "no_payment_required":
		ev.Status = StatusPaid
	default:
		ev.Status = StatusPending
	}
	return ev
}

// verifySignature implements Stripe's scheme: any v1 entry may match (several
// appear while a secret is being rolled), and the timestamp must be recent.
func (s *stripe) verifySignature(header string, body []byte) bool {
	if s.cred.WebhookSecret == "" || header == "" {
		return false
	}
	var ts string
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "v1":
			sigs = append(sigs, v)
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || len(sigs) == 0 {
		return false
	}
	age := s.opt.now().Sub(time.Unix(sec, 0))
	if age > stripeTolerance || age < -stripeTolerance {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.cred.WebhookSecret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := mac.Sum(nil)
	for _, sig := range sigs {
		got, err := hex.DecodeString(sig)
		if err == nil && hmac.Equal(got, want) {
			return true
		}
	}
	return false
}

func (s *stripe) VerifyWebhook(r *http.Request, body []byte) (Event, error) {
	if !s.verifySignature(r.Header.Get("Stripe-Signature"), body) {
		return Event{}, ErrBadSignature
	}
	var evt struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Livemode bool   `json:"livemode"`
		Data     struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &evt); err != nil || evt.ID == "" {
		return Event{}, &Error{Code: "invalid_payload", Message: "not a Stripe event"}
	}
	ev := Event{Key: evt.ID, Type: evt.Type, Live: boolPtr(evt.Livemode)}
	switch evt.Type {
	case "checkout.session.completed", "checkout.session.async_payment_succeeded",
		"checkout.session.async_payment_failed", "checkout.session.expired":
		var ss stripeSession
		if err := json.Unmarshal(evt.Data.Object, &ss); err != nil {
			return Event{}, &Error{Code: "invalid_payload", Message: "bad checkout session"}
		}
		se := s.sessionEvent(ss)
		se.Key, se.Type, se.Live = ev.Key, ev.Type, ev.Live
		if evt.Type == "checkout.session.async_payment_failed" {
			se.Status = StatusFailed
		}
		return se, nil
	case "charge.refunded":
		var ch struct {
			PaymentIntent string            `json:"payment_intent"`
			Refunded      bool              `json:"refunded"` // true only when fully refunded
			Amount        int64             `json:"amount"`
			Currency      string            `json:"currency"`
			Metadata      map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(evt.Data.Object, &ch); err != nil {
			return Event{}, &Error{Code: "invalid_payload", Message: "bad charge"}
		}
		ev.PaymentRef = ch.PaymentIntent
		ev.OrderID = ch.Metadata["order_id"]
		ev.Amount = ch.Amount
		ev.Currency = strings.ToUpper(ch.Currency)
		if ch.Refunded {
			ev.Status = StatusRefunded
		}
		return ev, nil
	}
	// Other event types are acknowledged and ignored.
	return ev, nil
}

func (s *stripe) GetStatus(ctx context.Context, ref string) (Event, error) {
	req, err := s.newReq("GET", "/v1/checkout/sessions/"+url.PathEscape(ref), nil)
	if err != nil {
		return Event{}, err
	}
	var ss stripeSession
	status, raw, err := call(ctx, s.opt.client(), req, &ss)
	if err != nil {
		return Event{}, err
	}
	if status == http.StatusNotFound {
		return Event{}, ErrNotFound
	}
	if status/100 != 2 {
		return Event{}, stripeError(status, raw, "Stripe couldn't read the checkout")
	}
	return s.sessionEvent(ss), nil
}

// TestConnection lists one checkout session, which needs the same permission
// (Checkout Sessions) as creating them, so restricted keys are checked too.
func (s *stripe) TestConnection(ctx context.Context) error {
	req, err := s.newReq("GET", "/v1/checkout/sessions?limit=1", nil)
	if err != nil {
		return err
	}
	status, raw, err := call(ctx, s.opt.client(), req, nil)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return stripeError(status, raw, "Stripe rejected the key")
	}
	return nil
}
