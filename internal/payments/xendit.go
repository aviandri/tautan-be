package payments

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// xendit uses the Invoices API (v2): one hosted page that offers every method
// the creator has activated (VA, e-wallets, QRIS, cards, retail outlets).
// Docs: https://developers.xendit.co/api-reference/#create-invoice
// Sandbox vs live is decided by the key (xnd_development_ / xnd_production_);
// the host is the same.
type xendit struct {
	mode string
	cred Credentials
	opt  Options
}

func (x *xendit) Name() string { return Xendit }
func (x *xendit) Mode() string { return x.mode }

func (x *xendit) base() string { return x.opt.base(Xendit, "https://api.xendit.co") }

func (x *xendit) newReq(method, path string, body any) (*http.Request, error) {
	var req *http.Request
	var err error
	if body != nil {
		r, e := jsonReader(body)
		if e != nil {
			return nil, e
		}
		req, err = http.NewRequest(method, x.base()+path, r)
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		req, err = http.NewRequest(method, x.base()+path, nil)
	}
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(x.cred.SecretKey, "")
	return req, nil
}

type xenditInvoice struct {
	ID         string  `json:"id"`
	ExternalID string  `json:"external_id"`
	Status     string  `json:"status"`
	Amount     float64 `json:"amount"`
	PaidAmount float64 `json:"paid_amount"`
	Currency   string  `json:"currency"`
	InvoiceURL string  `json:"invoice_url"`
	ExpiryDate string  `json:"expiry_date"`
}

type xenditErr struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
}

func xenditError(status int, body []byte, fallback string) error {
	var e xenditErr
	_ = json.Unmarshal(body, &e)
	if e.ErrorCode == "INVALID_API_KEY" || e.ErrorCode == "REQUEST_FORBIDDEN_ERROR" {
		status = http.StatusUnauthorized
	}
	return providerError(status, e.Message, fallback)
}

func xenditAmount(minor int64, currency string) (any, error) {
	if currency == "IDR" {
		return majorIDR(minor)
	}
	return float64(minor) / 100, nil
}

func (x *xendit) CreateCheckout(ctx context.Context, c Checkout) (Session, error) {
	if !Supports(Xendit, c.Currency) {
		return Session{}, &Error{Code: "unsupported_currency", Message: "Xendit checkout supports IDR and USD"}
	}
	amount, err := xenditAmount(c.Amount, c.Currency)
	if err != nil {
		return Session{}, err
	}
	items := make([]map[string]any, 0, len(c.Items))
	for _, it := range c.Items {
		price, err := xenditAmount(it.UnitAmount, c.Currency)
		if err != nil {
			return Session{}, err
		}
		items = append(items, map[string]any{"name": truncate(it.Name, 255), "quantity": it.Quantity, "price": price})
	}
	duration := int(math.Round(c.ExpiresAt.Sub(x.opt.now()).Seconds()))
	if duration < 300 {
		duration = 300
	}
	body := map[string]any{
		"external_id":          c.OrderID,
		"amount":               amount,
		"currency":             c.Currency,
		"description":          truncate(c.Description, 255),
		"payer_email":          c.BuyerEmail,
		"invoice_duration":     duration,
		"success_redirect_url": c.SuccessURL,
		"failure_redirect_url": c.CancelURL,
		"items":                items,
		"customer":             map[string]any{"email": c.BuyerEmail, "given_names": firstNonEmpty(c.BuyerName, c.BuyerEmail)},
	}
	req, err := x.newReq("POST", "/v2/invoices", body)
	if err != nil {
		return Session{}, err
	}
	// Retrying the same order never creates a second invoice.
	req.Header.Set("X-IDEMPOTENCY-KEY", c.OrderID)
	var inv xenditInvoice
	status, raw, err := call(ctx, x.opt.client(), req, &inv)
	if err != nil {
		return Session{}, err
	}
	if status/100 != 2 {
		return Session{}, xenditError(status, raw, "Xendit couldn't create the invoice")
	}
	if inv.ID == "" || inv.InvoiceURL == "" {
		return Session{}, &Error{Code: "provider_error", Message: "Xendit returned no invoice URL", Status: status}
	}
	s := Session{Ref: inv.ID, URL: inv.InvoiceURL, ExpiresAt: c.ExpiresAt}
	if t, err := time.Parse(time.RFC3339, inv.ExpiryDate); err == nil {
		s.ExpiresAt = t
	}
	return s, nil
}

func (x *xendit) event(inv xenditInvoice) Event {
	ev := Event{
		OrderID:  inv.ExternalID,
		Ref:      inv.ID,
		Currency: strings.ToUpper(inv.Currency),
		Type:     "invoice." + strings.ToLower(inv.Status),
	}
	amt := inv.PaidAmount
	if amt == 0 {
		amt = inv.Amount
	}
	ev.Amount = fromMajor(amt)
	switch strings.ToUpper(inv.Status) {
	case "PAID", "SETTLED":
		ev.Status = StatusPaid
	case "EXPIRED":
		ev.Status = StatusExpired
	case "PENDING":
		ev.Status = StatusPending
	}
	return ev
}

// VerifyWebhook checks the x-callback-token header against the creator's
// callback verification token (Settings → Webhooks in the Xendit dashboard).
func (x *xendit) VerifyWebhook(r *http.Request, body []byte) (Event, error) {
	got := r.Header.Get("x-callback-token")
	if x.cred.WebhookSecret == "" || got == "" ||
		subtle.ConstantTimeCompare([]byte(got), []byte(x.cred.WebhookSecret)) != 1 {
		return Event{}, ErrBadSignature
	}
	var inv xenditInvoice
	if err := json.Unmarshal(body, &inv); err != nil || inv.ID == "" {
		return Event{}, &Error{Code: "invalid_payload", Message: "not an invoice callback"}
	}
	ev := x.event(inv)
	// Xendit sends a webhook-id header that is stable across retries.
	if id := r.Header.Get("webhook-id"); id != "" {
		ev.Key = "wh:" + id
	} else {
		ev.Key = inv.ID + ":" + strings.ToUpper(inv.Status)
	}
	return ev, nil
}

func (x *xendit) GetStatus(ctx context.Context, ref string) (Event, error) {
	req, err := x.newReq("GET", "/v2/invoices/"+url.PathEscape(ref), nil)
	if err != nil {
		return Event{}, err
	}
	var inv xenditInvoice
	status, raw, err := call(ctx, x.opt.client(), req, &inv)
	if err != nil {
		return Event{}, err
	}
	if status == http.StatusNotFound {
		return Event{}, ErrNotFound
	}
	if status/100 != 2 {
		return Event{}, xenditError(status, raw, "Xendit couldn't read the invoice")
	}
	return x.event(inv), nil
}

// TestConnection lists one invoice, which needs the same "Money-in" permission
// as creating them.
func (x *xendit) TestConnection(ctx context.Context) error {
	req, err := x.newReq("GET", "/v2/invoices?limit=1", nil)
	if err != nil {
		return err
	}
	status, raw, err := call(ctx, x.opt.client(), req, nil)
	if err != nil {
		return err
	}
	if status/100 != 2 {
		return xenditError(status, raw, "Xendit rejected the key")
	}
	return nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
