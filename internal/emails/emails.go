// Package emails renders CertsForever's messages (plain text + HTML).
//
// Sign-in and invitation mail comes from the platform. Certificate mail
// comes from the issuing client: its name as the sender's display name and
// its Reply-To, so students' replies reach their school, not the platform.
package emails

import (
	"bytes"
	htmltemplate "html/template"
	"strconv"
	"strings"
	texttemplate "text/template"

	"certsforever/internal/mail"
)

// Platform identifies the service in platform mail.
type Platform struct {
	Name    string // "CertsForever"
	BaseURL string // "https://certs.example.org"
}

// Client identifies the sender of certificate mail.
type Client struct {
	Name    string
	ReplyTo string
	SiteURL string
}

type content struct {
	Preheader string // inbox preview line
	Greeting  string
	Paras     []string
	Button    string
	Link      string
	After     []string // paragraphs after the button
	Footer    string
}

var textTmpl = texttemplate.Must(texttemplate.New("t").Parse(`{{.Greeting}}

{{range .Paras}}{{.}}

{{end}}{{.Button}}:
{{.Link}}

{{range .After}}{{.}}

{{end}}--
{{.Footer}}
`))

// Inline styles only: many mail clients ignore <style> blocks.
var htmlTmpl = htmltemplate.Must(htmltemplate.New("h").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"></head>
<body style="margin:0;padding:0;background:#f6f4ef;">
<span style="display:none;max-height:0;overflow:hidden;">{{.Preheader}}</span>
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f6f4ef;padding:24px 12px;">
<tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#ffffff;border:1px solid #e2ddd3;border-top:5px solid #1f8f81;border-radius:6px;">
<tr><td style="padding:28px 28px 8px;font:16px/1.55 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#101c2e;">
<p style="margin:0 0 16px;">{{.Greeting}}</p>
{{range .Paras}}<p style="margin:0 0 16px;">{{.}}</p>{{end}}
<p style="margin:24px 0;"><a href="{{.Link}}" style="display:inline-block;background:#1f8f81;color:#ffffff;text-decoration:none;font-weight:600;padding:12px 20px;border-radius:6px;">{{.Button}}</a></p>
<p style="margin:0 0 16px;font-size:13px;color:#5b6676;">If the button doesn't work, copy this link into your browser:<br><a href="{{.Link}}" style="color:#1f8f81;word-break:break-all;">{{.Link}}</a></p>
{{range .After}}<p style="margin:0 0 16px;">{{.}}</p>{{end}}
</td></tr>
<tr><td style="padding:12px 28px 24px;font:12px/1.5 -apple-system,Segoe UI,Roboto,Helvetica,Arial,sans-serif;color:#5b6676;border-top:1px solid #e2ddd3;">{{.Footer}}</td></tr>
</table>
</td></tr></table>
</body></html>
`))

func render(to, subject string, c content) (mail.Message, error) {
	var t, h bytes.Buffer
	if err := textTmpl.Execute(&t, c); err != nil {
		return mail.Message{}, err
	}
	if err := htmlTmpl.Execute(&h, c); err != nil {
		return mail.Message{}, err
	}
	return mail.Message{To: to, Subject: subject, Text: strings.TrimSpace(t.String()) + "\n", HTML: h.String()}, nil
}

// SignIn is the "here's your sign-in link" email.
func SignIn(p Platform, to, link string, minutes int) (mail.Message, error) {
	m, err := render(to, "Sign in to "+p.Name, content{
		Preheader: "Your sign-in link (works once, expires soon)",
		Greeting:  "Hello,",
		Paras:     []string{"Use this link to sign in to " + p.Name + "."},
		Button:    "Sign in",
		Link:      link,
		After: []string{"It works once and expires in " + strconv.Itoa(minutes) + " minutes. " +
			"If you didn't ask to sign in, you can ignore this email."},
		Footer: p.Name + " · " + p.BaseURL,
	})
	m.FromName = p.Name
	return m, err
}

// Invite is sent when someone is made a client or platform administrator.
func Invite(p Platform, to, subject, intro, link string) (mail.Message, error) {
	m, err := render(to, subject, content{
		Preheader: intro,
		Greeting:  "Hello,",
		Paras:     []string{intro},
		Button:    "Sign in",
		Link:      link,
		After: []string{"This link works once and expires in 7 days. After that, sign in any time at " +
			p.BaseURL + "/login with this email address."},
		Footer: p.Name + " · " + p.BaseURL,
	})
	m.FromName = p.Name
	return m, err
}

// CertificateReady tells a student their certificate is issued and how to
// publish it.
func CertificateReady(p Platform, c Client, to, recipient, course, claimLink string) (mail.Message, error) {
	m, err := render(to, "Your "+course+" certificate is ready", content{
		Preheader: "Publish it, add it to LinkedIn, and share it.",
		Greeting:  "Congratulations, " + firstName(recipient) + "!",
		Paras: []string{
			c.Name + " has issued your certificate for completing " + course + ".",
			"It's private until you publish it. Open your certificate page to publish it, " +
				"add it to your LinkedIn profile, and share it.",
		},
		Button: "View your certificate",
		Link:   claimLink,
		After: []string{"Keep this email: the link is how you manage your certificate. " +
			"Don't forward it; anyone with it can change your certificate's settings."},
		Footer: footer(p, c),
	})
	m.FromName, m.ReplyTo = c.Name, c.ReplyTo
	return m, err
}

// Reminder is the single nudge sent when a certificate hasn't been published.
func Reminder(p Platform, c Client, to, recipient, course, claimLink string) (mail.Message, error) {
	m, err := render(to, "Reminder: publish your "+course+" certificate", content{
		Preheader: "Your certificate is still private.",
		Greeting:  "Hi " + firstName(recipient) + ",",
		Paras: []string{
			"Your " + course + " certificate from " + c.Name + " is still private. " +
				"Publish it so employers can verify it, and add it to your LinkedIn profile in a couple of clicks.",
		},
		Button: "Publish your certificate",
		Link:   claimLink,
		After:  []string{"This is the only reminder we'll send. Earlier certificate links no longer work; use this one."},
		Footer: footer(p, c),
	})
	m.FromName, m.ReplyTo = c.Name, c.ReplyTo
	return m, err
}

// Test is sent by `certsforever email test` to check the mail setup.
func Test(p Platform, to string) (mail.Message, error) {
	m, err := render(to, p.Name+" test email", content{
		Preheader: "Email delivery works.",
		Greeting:  "Hello,",
		Paras:     []string{"This is a test from " + p.Name + ". If you can read it, outgoing email works."},
		Button:    "Open " + p.Name,
		Link:      p.BaseURL + "/login",
		Footer:    p.Name + " · " + p.BaseURL,
	})
	m.FromName = p.Name
	return m, err
}

func footer(p Platform, c Client) string {
	f := "Sent by " + c.Name + " via " + p.Name + "."
	if c.ReplyTo != "" {
		f += " Questions? Reply to this email."
	}
	return f
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return "there"
}
