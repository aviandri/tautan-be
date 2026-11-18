package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/aviandri/tautan-be/internal/payments"
)

// The fake provider's hosted checkout (development only; routes are mounted
// only when s.fake != nil, i.e. APP_ENV isn't production). The buyer picks an
// outcome; the API records it in the FakeStore and delivers a notification
// through the normal webhook handler, which confirms it with GetStatus exactly
// like the Xendit and Midtrans flows (amount and currency checks included).

var fakeOutcomes = map[string]payments.Status{
	"paid":     payments.StatusPaid,
	"mismatch": payments.StatusPaid, // pays half: exercises amount_mismatch
	"failed":   payments.StatusFailed,
	"expired":  payments.StatusExpired,
}

var fakeCheckoutTmpl = template.Must(template.New("fake").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="robots" content="noindex"><title>Fake checkout (dev only)</title>
<style>
body{font:16px/1.5 system-ui,sans-serif;margin:0;background:#f4f4f5;color:#18181b}
main{max-width:420px;margin:40px auto;padding:24px;background:#fff;border-radius:12px;box-shadow:0 1px 3px #0002}
.dev{background:#fef3c7;color:#92400e;padding:6px 10px;border-radius:6px;font-size:13px}
table{width:100%;border-collapse:collapse;margin:16px 0}td{padding:4px 0}td:last-child{text-align:right}
.total td{border-top:1px solid #e4e4e7;font-weight:600;padding-top:8px}
form{margin:8px 0}button{width:100%;padding:12px;border:0;border-radius:8px;font-size:16px;cursor:pointer}
.pay{background:#16a34a;color:#fff}.alt{background:#e4e4e7;color:#18181b}
</style></head><body><main>
<p class="dev">Fake payment provider: development only. No money moves.</p>
<h1>Checkout</h1>
<p>{{.Description}}<br><small>{{.BuyerEmail}}</small></p>
<table>{{range .Items}}<tr><td>{{.Name}} × {{.Quantity}}</td><td>{{.Line}}</td></tr>{{end}}
<tr class="total"><td>Total</td><td data-testid="fake-total">{{.Total}}</td></tr></table>
{{if .Pending}}
<form method="post" action="{{.Action}}/paid"><button class="pay" name="go" data-testid="fake-pay">Pay {{.Total}}</button></form>
<form method="post" action="{{.Action}}/failed"><button class="alt" data-testid="fake-fail">Fail payment</button></form>
<form method="post" action="{{.Action}}/expired"><button class="alt" data-testid="fake-expire">Let it expire</button></form>
<form method="post" action="{{.Action}}/mismatch"><button class="alt" data-testid="fake-mismatch">Pay half (amount mismatch)</button></form>
{{else}}
<p data-testid="fake-status">This checkout is <strong>{{.Status}}</strong>.</p>
<p><a href="{{.Back}}">Back to the order</a></p>
{{end}}
</main></body></html>`))

type fakePageItem struct {
	Name     string
	Quantity int
	Line     string
}

func fakeMoney(minor int64, currency string) string {
	if strings.ToUpper(currency) == "USD" {
		return fmt.Sprintf("$%d.%02d", minor/100, minor%100)
	}
	// Rp 85.000 (whole rupiah, dot thousands separator)
	s := strconv.FormatInt(minor/100, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	return "Rp " + b.String()
}

func devPageHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex")
}

// GET /dev/fake-checkout/{ref}
func (s *Server) handleFakeCheckoutPage(w http.ResponseWriter, r *http.Request) {
	ref := chi.URLParam(r, "ref")
	c, ok := s.fake.Get(ref)
	if !ok {
		http.Error(w, "unknown fake checkout (the API may have restarted; start a new order)", http.StatusNotFound)
		return
	}
	co := c.Checkout
	items := make([]fakePageItem, 0, len(co.Items))
	for _, it := range co.Items {
		items = append(items, fakePageItem{it.Name, it.Quantity, fakeMoney(it.UnitAmount*int64(it.Quantity), co.Currency)})
	}
	devPageHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = fakeCheckoutTmpl.Execute(w, map[string]any{
		"Description": co.Description, "BuyerEmail": co.BuyerEmail, "Items": items,
		"Total": fakeMoney(co.Amount, co.Currency), "Pending": c.Status == payments.StatusPending,
		"Status": string(c.Status), "Action": "/dev/fake-checkout/" + ref, "Back": co.SuccessURL,
	})
}

// POST /dev/fake-checkout/{ref}/{paid|failed|expired|mismatch}
func (s *Server) handleFakeCheckoutAction(w http.ResponseWriter, r *http.Request) {
	ref, action := chi.URLParam(r, "ref"), chi.URLParam(r, "action")
	st, ok := fakeOutcomes[action]
	c, found := s.fake.Get(ref)
	if !ok || !found {
		http.Error(w, "unknown fake checkout or action", http.StatusNotFound)
		return
	}
	amount := c.Checkout.Amount
	if action == "mismatch" {
		amount /= 2
	}
	s.fake.Settle(ref, st, amount) // a repeat click on a settled checkout just re-notifies

	var pid string
	err := s.db.QueryRow(r.Context(), `SELECT profile_id FROM orders WHERE provider = $1 AND provider_ref = $2`,
		payments.Fake, ref).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "no order uses this fake checkout", http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	// Deliver the notification through the real webhook handler.
	body, _ := json.Marshal(payments.FakeNotification{Ref: ref, OrderID: c.Checkout.OrderID})
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/webhooks/fake/"+pid, bytes.NewReader(body))
	if err != nil {
		s.internalError(w, err)
		return
	}
	rc := chi.NewRouteContext()
	rc.URLParams.Add("provider", payments.Fake)
	rc.URLParams.Add("profile_id", pid)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
	rec := httptest.NewRecorder()
	s.handlePaymentWebhook(rec, req)
	if rec.Code != http.StatusOK {
		devPageHeaders(w)
		http.Error(w, "the webhook handler answered "+strconv.Itoa(rec.Code)+": "+rec.Body.String(), http.StatusBadGateway)
		return
	}
	next := c.Checkout.SuccessURL
	if st == payments.StatusFailed || st == payments.StatusExpired {
		next = c.Checkout.CancelURL
	}
	devPageHeaders(w)
	http.Redirect(w, r, next, http.StatusSeeOther)
}
