package payments

import (
	"context"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// midtrans uses Snap's redirect flow: we create a transaction server-side and
// send the buyer to redirect_url. No Snap JS runs on our pages.
// Docs: https://docs.midtrans.com/reference/backend-integration
// Notifications are signed: signature_key = sha512(order_id + status_code +
// gross_amount + server_key). Midtrans settles in IDR only.
type midtrans struct {
	mode string
	cred Credentials
	opt  Options
}

func (m *midtrans) Name() string { return Midtrans }
func (m *midtrans) Mode() string { return m.mode }

func (m *midtrans) snapBase() string {
	def := "https://app.midtrans.com"
	if m.mode == Sandbox {
		def = "https://app.sandbox.midtrans.com"
	}
	return m.opt.base(Midtrans, def)
}

func (m *midtrans) apiBase() string {
	def := "https://api.midtrans.com"
	if m.mode == Sandbox {
		def = "https://api.sandbox.midtrans.com"
	}
	return m.opt.base("midtrans_api", def)
}

func (m *midtrans) newReq(method, u string, body any) (*http.Request, error) {
	var req *http.Request
	var err error
	if body != nil {
		r, e := jsonReader(body)
		if e != nil {
			return nil, e
		}
		req, err = http.NewRequest(method, u, r)
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
	} else {
		req, err = http.NewRequest(method, u, nil)
	}
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(m.cred.SecretKey, "")
	return req, nil
}

func (m *midtrans) CreateCheckout(ctx context.Context, c Checkout) (Session, error) {
	if c.Currency != "IDR" {
		return Session{}, &Error{Code: "unsupported_currency", Message: "Midtrans only accepts IDR"}
	}
	gross, err := majorIDR(c.Amount)
	if err != nil {
		return Session{}, err
	}
	items := make([]map[string]any, 0, len(c.Items))
	for i, it := range c.Items {
		price, err := majorIDR(it.UnitAmount)
		if err != nil {
			return Session{}, err
		}
		items = append(items, map[string]any{
			"id": strconv.Itoa(i + 1), "price": price, "quantity": it.Quantity,
			"name": truncate(it.Name, 50), // Midtrans rejects longer names
		})
	}
	minutes := int(math.Ceil(c.ExpiresAt.Sub(m.opt.now()).Minutes()))
	if minutes < 5 {
		minutes = 5
	}
	body := map[string]any{
		"transaction_details": map[string]any{"order_id": c.OrderID, "gross_amount": gross},
		"item_details":        items,
		"customer_details":    map[string]any{"first_name": truncate(firstNonEmpty(c.BuyerName, c.BuyerEmail), 255), "email": c.BuyerEmail},
		"callbacks":           map[string]any{"finish": c.SuccessURL, "error": c.CancelURL},
		"expiry":              map[string]any{"unit": "minutes", "duration": minutes},
	}
	req, err := m.newReq("POST", m.snapBase()+"/snap/v1/transactions", body)
	if err != nil {
		return Session{}, err
	}
	// Send this transaction's notifications to our webhook, whatever URL the
	// creator set in the dashboard (they may use the account for other shops).
	if c.NotifyURL != "" {
		req.Header.Set("X-Override-Notification", c.NotifyURL)
	}
	var out struct {
		Token       string   `json:"token"`
		RedirectURL string   `json:"redirect_url"`
		Errors      []string `json:"error_messages"`
	}
	status, raw, err := call(ctx, m.opt.client(), req, &out)
	if err != nil {
		return Session{}, err
	}
	if status/100 != 2 {
		_ = json.Unmarshal(raw, &out)
		return Session{}, providerError(status, strings.Join(out.Errors, "; "), "Midtrans couldn't create the transaction")
	}
	if out.RedirectURL == "" {
		return Session{}, &Error{Code: "provider_error", Message: "Midtrans returned no redirect URL", Status: status}
	}
	return Session{Ref: c.OrderID, URL: out.RedirectURL, ExpiresAt: c.ExpiresAt}, nil
}

// midtransTx is the shape shared by the status API and HTTP notifications.
type midtransTx struct {
	StatusCode        string `json:"status_code"`
	StatusMessage     string `json:"status_message"`
	TransactionID     string `json:"transaction_id"`
	OrderID           string `json:"order_id"`
	GrossAmount       string `json:"gross_amount"`
	Currency          string `json:"currency"`
	TransactionStatus string `json:"transaction_status"`
	FraudStatus       string `json:"fraud_status"`
	SignatureKey      string `json:"signature_key"`
}

func (m *midtrans) event(tx midtransTx) Event {
	ev := Event{OrderID: tx.OrderID, Ref: tx.OrderID, Currency: "IDR", Type: tx.TransactionStatus}
	if tx.Currency != "" {
		ev.Currency = strings.ToUpper(tx.Currency)
	}
	if f, err := strconv.ParseFloat(tx.GrossAmount, 64); err == nil {
		ev.Amount = fromMajor(f)
	}
	switch tx.TransactionStatus {
	case "settlement":
		if tx.StatusCode == "200" {
			ev.Status = StatusPaid
		}
	case "capture":
		if tx.StatusCode != "200" {
			break
		}
		// Card payments: "challenge" waits for the merchant to review in the dashboard.
		if tx.FraudStatus == "accept" || tx.FraudStatus == "" {
			ev.Status = StatusPaid
		} else {
			ev.Status = StatusPending
		}
	case "pending", "authorize":
		ev.Status = StatusPending
	case "deny", "cancel", "failure":
		ev.Status = StatusFailed
	case "expire":
		ev.Status = StatusExpired
	case "refund", "chargeback":
		ev.Status = StatusRefunded
	}
	return ev
}

func (m *midtrans) signature(tx midtransTx) string {
	sum := sha512.Sum512([]byte(tx.OrderID + tx.StatusCode + tx.GrossAmount + m.cred.SecretKey))
	return hex.EncodeToString(sum[:])
}

func (m *midtrans) VerifyWebhook(r *http.Request, body []byte) (Event, error) {
	var tx midtransTx
	if err := json.Unmarshal(body, &tx); err != nil || tx.OrderID == "" || tx.SignatureKey == "" {
		return Event{}, ErrBadSignature
	}
	want := m.signature(tx)
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(tx.SignatureKey)), []byte(want)) != 1 {
		return Event{}, ErrBadSignature
	}
	ev := m.event(tx)
	// Only these fields are signed; transaction_status and transaction_id
	// aren't, so they stay out of the key and the status must be confirmed.
	ev.Key = tx.OrderID + ":" + tx.StatusCode + ":" + tx.GrossAmount
	ev.Confirm = true
	return ev, nil
}

func (m *midtrans) GetStatus(ctx context.Context, ref string) (Event, error) {
	req, err := m.newReq("GET", m.apiBase()+"/v2/"+url.PathEscape(ref)+"/status", nil)
	if err != nil {
		return Event{}, err
	}
	status, raw, err := call(ctx, m.opt.client(), req, nil)
	if err != nil {
		return Event{}, err
	}
	var tx midtransTx
	_ = json.Unmarshal(raw, &tx)
	// The Core API reports errors in status_code even on HTTP 200.
	code := tx.StatusCode
	if code == "" {
		code = strconv.Itoa(status)
	}
	switch {
	case code == "404":
		return Event{}, ErrNotFound
	case code == "401" || status == http.StatusUnauthorized:
		return Event{}, providerError(http.StatusUnauthorized, tx.StatusMessage, "Midtrans rejected the server key")
	case tx.TransactionStatus != "" && tx.OrderID != "":
		// A transaction: its status_code follows the state (201 pending,
		// 202 deny, 407 expire), so it isn't an error.
	case status/100 != 2 || (len(code) == 3 && code[0] != '2'):
		return Event{}, providerError(status, tx.StatusMessage, "Midtrans couldn't read the transaction")
	}
	return m.event(tx), nil
}

// TestConnection asks for the status of an order that can't exist: a valid
// key gets "404 transaction doesn't exist", an invalid one gets 401.
func (m *midtrans) TestConnection(ctx context.Context) error {
	_, err := m.GetStatus(ctx, "tautan-connection-test")
	if err == ErrNotFound {
		return nil
	}
	if err == nil {
		return nil
	}
	return err
}
