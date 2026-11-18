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

// Field is one label/value row in a submission notice.
type Field struct{ Label, Value string }

var submissionLayout = template.Must(template.New("submission").Parse(`<!doctype html>
<html><body style="margin:0;padding:24px;background:#f7f6f3;font-family:-apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#111">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0"><tr><td align="center">
<table role="presentation" width="100%" style="max-width:520px;background:#fff;border-radius:20px;padding:32px" cellspacing="0" cellpadding="0"><tr><td>
<p style="margin:0 0 24px;font-weight:700;font-size:18px">tautan</p>
<h1 style="margin:0 0 6px;font-size:20px;line-height:1.3">{{.Heading}}</h1>
<p style="margin:0 0 20px;font-size:14px;color:#777">{{.Sub}}</p>
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="margin:0 0 24px;font-size:15px">
{{range .Fields}}<tr><td style="padding:8px 0;border-top:1px solid #eee;vertical-align:top;width:34%;color:#777">{{.Label}}</td><td style="padding:8px 0;border-top:1px solid #eee;white-space:pre-wrap;word-break:break-word">{{.Value}}</td></tr>
{{end}}</table>
<p style="margin:0 0 24px"><a href="{{.Link}}" style="display:inline-block;background:#d0106e;color:#fff;text-decoration:none;font-weight:600;padding:12px 22px;border-radius:999px">Open in Tautan</a></p>
<p style="margin:0;font-size:13px;color:#777">{{.Footer}} <a href="{{.SettingsLink}}" style="color:#555">Notification settings</a></p>
</td></tr></table>
</td></tr></table>
</body></html>`))

// oneLine makes owner-controlled text safe for a Subject header.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// SubmissionNotice tells a page owner about a new form response or contact
// message. Values are visitor-supplied: html/template escapes them, and the
// subject only uses owner-controlled text, flattened to one line.
func SubmissionNotice(to, replyTo, pageName, blockTitle string, fields []Field, link, settingsLink string) Message {
	what := oneLine(blockTitle, 60)
	if what == "" {
		what = "your form"
	}
	data := struct {
		Heading, Sub, Link, SettingsLink, Footer string
		Fields                                   []Field
	}{
		Heading: "New response to " + what, Sub: "On " + pageName, Fields: fields, Link: link, SettingsLink: settingsLink,
		Footer: "Reply to this email to answer them directly. You get these because notifications are on.",
	}
	if replyTo == "" {
		data.Footer = "You get these because notifications are on."
	}
	var h bytes.Buffer
	_ = submissionLayout.Execute(&h, data)
	lines := []string{data.Heading, data.Sub, ""}
	for _, f := range fields {
		lines = append(lines, f.Label+": "+f.Value)
	}
	lines = append(lines, "", "Open in Tautan: "+link, "", data.Footer, "Notification settings: "+settingsLink)
	return Message{To: to, ReplyTo: replyTo, Subject: oneLine("New response: "+what+" ("+pageName+")", 120),
		Text: strings.Join(lines, "\n"), HTML: h.String()}
}

// EmailChangeConfirm goes to the new address from POST /me/email.
func EmailChangeConfirm(to, link string) Message {
	return action{
		Subject: "Confirm your new email for Tautan",
		Heading: "Confirm your new email",
		Body:    "Use this address to sign in to Tautan and get account emails. Your email only changes once you confirm. The link works once and expires in 24 hours. Confirming signs you out on all your devices.",
		Button:  "Confirm new email",
		Link:    link,
		Footer:  "Didn't ask for this? Ignore this email and nothing changes.",
	}.message(to)
}

// EmailChanged goes to the old address once a change is confirmed. The
// link undoes the change for 7 days.
func EmailChanged(to, maskedNew, link string) Message {
	return action{
		Subject: "Your Tautan email was changed",
		Heading: "Your email was changed",
		Body:    "The email on your Tautan account was changed to " + maskedNew + ", and every session was signed out. If you made this change, you don't need to do anything.",
		Button:  "This wasn't me: undo it",
		Link:    link,
		Footer:  "The undo link works for 7 days. After undoing, reset your password, since someone else may know it.",
	}.message(to)
}

// LeadMagnetDelivery emails a visitor the download they asked for on a page.
// Only owner text that's already public (the card title and page name) is
// used, flattened to one line.
func LeadMagnetDelivery(to, pageName, pageURL, title, link string) Message {
	what := oneLine(title, 80)
	return action{
		Subject: oneLine("Your download: "+what, 120),
		Heading: what,
		Body:    "Here's the download you asked for on " + oneLine(pageName, 60) + " (" + pageURL + "). The link works for 7 days.",
		Button:  "Download",
		Link:    link,
		Footer:  "You got this because this address was entered on that page. If that wasn't you, ignore this email; we won't send more.",
	}.message(to)
}

// GiveawayWinner tells the drawn entrant they won. Owner text (title, prize,
// page name) is already public on the card; it's flattened to one line and
// escaped. No Reply-To: the owner's account email stays private.
func GiveawayWinner(to, pageName, pageURL, title, prize string) Message {
	page := oneLine(pageName, 60)
	return action{
		Subject: oneLine("You won: "+oneLine(title, 80)+" ("+page+")", 120),
		Heading: "You won " + oneLine(prize, 120) + "!",
		Body:    "Congratulations: you were drawn as the winner of \"" + oneLine(title, 100) + "\" on " + page + ". They'll get in touch at this address to arrange your prize. You can also reach them through their page.",
		Button:  "Visit " + page,
		Link:    pageURL,
		Footer:  "You got this because this address entered the giveaway on that page. Tautan hosts the page and never asks for payment to claim a prize.",
	}.message(to)
}
