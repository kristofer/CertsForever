package emails

import (
	"strings"
	"testing"
)

var p = Platform{Name: "CertsForever", BaseURL: "https://certs.example.org"}

func TestCertificateReadyIsBrandedAndEscaped(t *testing.T) {
	c := Client{Name: "Zip Code Wilmington", ReplyTo: "info@zipcode.example"}
	link := "https://certs.example.org/claim/abc_DEF-123"
	m, err := CertificateReady(p, c, "ada@example.com", "Ada <script>alert(1)</script> Lovelace",
		"Java & Spring", link)
	if err != nil {
		t.Fatal(err)
	}
	if m.FromName != "Zip Code Wilmington" || m.ReplyTo != "info@zipcode.example" || m.To != "ada@example.com" {
		t.Errorf("branding: %+v", m)
	}
	if m.Subject != "Your Java & Spring certificate is ready" {
		t.Errorf("subject %q", m.Subject)
	}
	for _, body := range []string{m.Text, m.HTML} {
		if !strings.Contains(body, link) {
			t.Errorf("link missing from body:\n%s", body)
		}
	}
	if strings.Contains(m.HTML, "<script>") || !strings.Contains(m.HTML, "Java &amp; Spring") {
		t.Errorf("HTML not escaped:\n%s", m.HTML)
	}
	if !strings.Contains(m.Text, "Congratulations, Ada") || !strings.Contains(m.Text, "Reply to this email") {
		t.Errorf("text:\n%s", m.Text)
	}
}

func TestPlatformMail(t *testing.T) {
	m, _ := SignIn(p, "a@example.com", "https://certs.example.org/login/tok", 15)
	if m.FromName != "CertsForever" || m.ReplyTo != "" || !strings.Contains(m.Text, "15 minutes") ||
		!strings.Contains(m.HTML, "https://certs.example.org/login/tok") {
		t.Errorf("sign-in: %+v", m)
	}
	m, _ = Invite(p, "a@example.com", "You've been invited", "Zed added you to Zip Code.", "https://x/login/t")
	if !strings.Contains(m.Text, "Zed added you to Zip Code.") || !strings.Contains(m.Text, "7 days") {
		t.Errorf("invite:\n%s", m.Text)
	}
	m, _ = Reminder(p, Client{Name: "TWA"}, "a@example.com", "", "Data", "https://x/claim/new")
	if !strings.Contains(m.Text, "Hi there,") || !strings.Contains(m.Text, "only reminder") || strings.Contains(m.Text, "Reply to this") {
		t.Errorf("reminder:\n%s", m.Text)
	}
	if m, _ := Test(p, "a@example.com"); !strings.Contains(m.Subject, "test") {
		t.Errorf("test subject %q", m.Subject)
	}
}
