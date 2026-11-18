package mail

import (
	"bytes"
	"fmt"
	"html/template"
	"strconv"
	"strings"
)

// ReceiptLine is one bought item.
type ReceiptLine struct {
	Name     string
	Quantity int
	Amount   string // formatted line total
}

// Receipt is sent to a buyer when an order is paid (payments b). The link opens
// the order page, which hands out fresh download links.
type Receipt struct {
	Seller string // page display name or @username
	Code   string // order code, e.g. K7Q2-M9XD
	Lines  []ReceiptLine
	Total  string // formatted
	Link   string // order page
	// payments c
	Shipping  string // formatted fee, "" when free or nothing ships
	Downloads bool   // the order has files: the button says "Get your download"
	Ships     bool   // the order has items the seller ships
}

// Button is the call to action for the order page.
func (r Receipt) Button() string {
	if r.Downloads || !r.Ships {
		return "Get your download"
	}
	return "View your order"
}

var receiptLayout = template.Must(template.New("receipt").Parse(`<!doctype html>
<html><body style="margin:0;padding:24px;background:#f7f6f3;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#111">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0"><tr><td align="center">
<table role="presentation" width="100%" style="max-width:480px;background:#fff;border-radius:20px;padding:32px" cellspacing="0" cellpadding="0"><tr><td>
<p style="margin:0 0 24px;font-weight:700;font-size:18px">{{.Seller}}</p>
<h1 style="margin:0 0 6px;font-size:22px;line-height:1.25">Thanks for your order</h1>
<p style="margin:0 0 20px;font-size:14px;color:#777">Order {{.Code}}</p>
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="margin:0 0 20px;font-size:15px">
{{range .Lines}}<tr><td style="padding:8px 0;border-bottom:1px solid #eee">{{if gt .Quantity 1}}{{.Quantity}} × {{end}}{{.Name}}</td><td align="right" style="padding:8px 0;border-bottom:1px solid #eee;white-space:nowrap">{{.Amount}}</td></tr>
{{end}}{{if .Shipping}}<tr><td style="padding:8px 0;border-bottom:1px solid #eee">Shipping</td><td align="right" style="padding:8px 0;border-bottom:1px solid #eee;white-space:nowrap">{{.Shipping}}</td></tr>
{{end}}<tr><td style="padding:10px 0;font-weight:700">Total</td><td align="right" style="padding:10px 0;font-weight:700;white-space:nowrap">{{.Total}}</td></tr>
</table>
{{if .Ships}}<p style="margin:0 0 20px;font-size:15px">{{.Seller}} will ship your order and email you when it's on its way.</p>
{{end}}<p style="margin:0 0 24px"><a href="{{.Link}}" style="display:inline-block;background:#d0106e;color:#fff;text-decoration:none;font-weight:600;padding:12px 22px;border-radius:999px">{{.Button}}</a></p>
<p style="margin:0 0 8px;font-size:13px;color:#777">Or paste this link into your browser:</p>
<p style="margin:0 0 24px;font-size:13px;word-break:break-all"><a href="{{.Link}}" style="color:#555">{{.Link}}</a></p>
<p style="margin:0;font-size:13px;color:#777">Keep this email: the link opens your order any time. You paid {{.Seller}} directly; Tautan just hosts their page.</p>
</td></tr></table>
</td></tr></table>
</body></html>`))

// OrderReceipt builds the buyer's receipt.
func OrderReceipt(to string, r Receipt) Message {
	var h bytes.Buffer
	_ = receiptLayout.Execute(&h, r)
	lines := []string{"Thanks for your order from " + r.Seller, "Order " + r.Code, ""}
	for _, l := range r.Lines {
		n := l.Name
		if l.Quantity > 1 {
			n = strconv.Itoa(l.Quantity) + " × " + n
		}
		lines = append(lines, n+" — "+l.Amount)
	}
	if r.Shipping != "" {
		lines = append(lines, "Shipping — "+r.Shipping)
	}
	lines = append(lines, "Total: "+r.Total, "")
	if r.Ships {
		lines = append(lines, r.Seller+" will ship your order and email you when it's on its way.", "")
	}
	lines = append(lines, r.Button()+": "+r.Link, "",
		"Keep this email: the link opens your order any time. You paid "+r.Seller+" directly; Tautan just hosts their page.")
	return Message{
		To:      to,
		Subject: fmt.Sprintf("Your order from %s (%s)", oneLine(r.Seller, 80), r.Code),
		Text:    strings.Join(lines, "\n"),
		HTML:    h.String(),
	}
}

// Money formats minor units for receipts: "Rp 85.000", "US$19.99".
func Money(minor int64, currency string) string {
	switch currency {
	case "IDR":
		return "Rp " + group(minor/100, ".")
	case "USD":
		return fmt.Sprintf("US$%s.%02d", group(minor/100, ","), minor%100)
	}
	return fmt.Sprintf("%s %s.%02d", currency, group(minor/100, ","), minor%100)
}

func group(n int64, sep string) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + sep + s[i:]
	}
	return s
}

// Shipped tells a buyer their order is on its way (payments c).
type Shipped struct {
	Seller   string
	Code     string
	Tracking string // may be empty
	Link     string // order page
}

var shippedLayout = template.Must(template.New("shipped").Parse(`<!doctype html>
<html><body style="margin:0;padding:24px;background:#f7f6f3;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#111">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0"><tr><td align="center">
<table role="presentation" width="100%" style="max-width:480px;background:#fff;border-radius:20px;padding:32px" cellspacing="0" cellpadding="0"><tr><td>
<p style="margin:0 0 24px;font-weight:700;font-size:18px">{{.Seller}}</p>
<h1 style="margin:0 0 6px;font-size:22px;line-height:1.25">Your order is on its way</h1>
<p style="margin:0 0 20px;font-size:14px;color:#777">Order {{.Code}}</p>
{{if .Tracking}}<p style="margin:0 0 20px;font-size:15px">Tracking number: <strong style="font-family:ui-monospace,Menlo,monospace">{{.Tracking}}</strong></p>
{{end}}<p style="margin:0 0 24px"><a href="{{.Link}}" style="display:inline-block;background:#d0106e;color:#fff;text-decoration:none;font-weight:600;padding:12px 22px;border-radius:999px">View your order</a></p>
<p style="margin:0;font-size:13px;color:#777">Questions about delivery? Reply to {{.Seller}} through their page. Tautan just hosts their page.</p>
</td></tr></table>
</td></tr></table>
</body></html>`))

// OrderShipped builds the "on its way" email.
func OrderShipped(to string, s Shipped) Message {
	var h bytes.Buffer
	_ = shippedLayout.Execute(&h, s)
	lines := []string{s.Seller + " has shipped your order " + s.Code + "."}
	if s.Tracking != "" {
		lines = append(lines, "Tracking number: "+s.Tracking)
	}
	lines = append(lines, "", "View your order: "+s.Link)
	return Message{
		To:      to,
		Subject: fmt.Sprintf("Your order from %s has shipped (%s)", oneLine(s.Seller, 80), s.Code),
		Text:    strings.Join(lines, "\n"),
		HTML:    h.String(),
	}
}
