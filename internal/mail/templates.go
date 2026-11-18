package mail

import (
	"bytes"
	"html/template"
	"strings"
)

// Account emails share one plain layout: a heading, a paragraph, one button
// and a fallback link. Keep copy short; every template has a text version.

var layout = template.Must(template.New("layout").Parse(`<!doctype html>
<html><body style="margin:0;padding:24px;background:#f7f6f3;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#111">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0"><tr><td align="center">
<table role="presentation" width="100%" style="max-width:480px;background:#fff;border-radius:20px;padding:32px" cellspacing="0" cellpadding="0"><tr><td>
<p style="margin:0 0 24px;font-weight:700;font-size:18px">tautan</p>
<h1 style="margin:0 0 12px;font-size:22px;line-height:1.25">{{.Heading}}</h1>
<p style="margin:0 0 24px;font-size:15px;line-height:1.55;color:#3a3a3a">{{.Body}}</p>
<p style="margin:0 0 24px"><a href="{{.Link}}" style="display:inline-block;background:#d0106e;color:#fff;text-decoration:none;font-weight:600;padding:12px 22px;border-radius:999px">{{.Button}}</a></p>
<p style="margin:0 0 8px;font-size:13px;color:#777">Or paste this link into your browser:</p>
<p style="margin:0 0 24px;font-size:13px;word-break:break-all"><a href="{{.Link}}" style="color:#555">{{.Link}}</a></p>
<p style="margin:0;font-size:13px;color:#777">{{.Footer}}</p>
</td></tr></table>
</td></tr></table>
</body></html>`))

type action struct {
	Subject, Heading, Body, Button, Link, Footer string
}

func (a action) message(to string) Message {
	var h bytes.Buffer
	_ = layout.Execute(&h, a)
	text := strings.Join([]string{a.Heading, "", a.Body, "", a.Button + ": " + a.Link, "", a.Footer, "", "— Tautan"}, "\n")
	return Message{To: to, Subject: a.Subject, Text: text, HTML: h.String()}
}

// PasswordReset is sent by POST /auth/password/forgot.
func PasswordReset(to, link string) Message {
	return action{
		Subject: "Reset your Tautan password",
		Heading: "Reset your password",
		Body:    "Someone (hopefully you) asked to reset the password for this Tautan account. The link works once and expires in 30 minutes.",
		Button:  "Choose a new password",
		Link:    link,
		Footer:  "Didn't ask for this? Ignore this email; your password stays the same.",
	}.message(to)
}

// VerifyEmail is sent after signup and on request.
func VerifyEmail(to, link string) Message {
	return action{
		Subject: "Confirm your email for Tautan",
		Heading: "Confirm your email",
		Body:    "Confirm this address so you can always get back into your Tautan account. The link expires in 3 days.",
		Button:  "Confirm email",
		Link:    link,
		Footer:  "Didn't create a Tautan account? You can ignore this email.",
	}.message(to)
}
