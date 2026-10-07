package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func valid(t *testing.T) Config {
	c := FromEnv()
	c.DBPath = filepath.Join(t.TempDir(), "certs.db")
	return c
}

func TestDefaultsAreValidForDevelopment(t *testing.T) {
	c := valid(t)
	if err := c.Validate(); err != nil {
		t.Fatalf("defaults should validate in development: %v", err)
	}
}

func TestProductionRules(t *testing.T) {
	c := valid(t)
	c.Env = "production"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "https") || !strings.Contains(err.Error(), "public domain") {
		t.Fatalf("localhost http base URL must fail in production: %v", err)
	}
	c.BaseURL = "https://certs.example.org"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "CERTS_MASTER_KEY") {
		t.Fatalf("production without a master key: %v", err)
	}
	c.MasterKey = make([]byte, 32)
	if err := c.Validate(); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
	}
	c.SMTPURL = "smtp://mail.example.org:587"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "CERTS_MAIL_FROM") {
		t.Fatalf("SMTP without From: %v", err)
	}
	c.MailFrom = "CertsForever <certs@example.org>"
	c.SMTPURL = "mail.example.org"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "CERTS_SMTP_URL") {
		t.Fatalf("bad SMTP URL: %v", err)
	}
}

func TestValidateReportsEverything(t *testing.T) {
	c := valid(t)
	c.Env = "staging"
	c.Addr = "8080"
	c.DBPath = "/no/such/dir/certs.db"
	c.BaseURL = "https://certs.example.org/certs"
	c.AdminToken = "short"
	c.PlatformName = " "
	err := c.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"CERTS_ENV", "CERTS_ADDR", "CERTS_DB", "origin only",
		"CERTS_ADMIN_TOKEN", "CERTS_PLATFORM_NAME"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s in: %v", want, err)
		}
	}
}

func TestSecretFromFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "token")
	os.WriteFile(f, []byte("  0123456789abcdef0123456789abcdef\n"), 0o600)
	t.Setenv("CERTS_ADMIN_TOKEN", "")
	t.Setenv("CERTS_ADMIN_TOKEN_FILE", f)
	t.Setenv("CERTS_DB", filepath.Join(t.TempDir(), "certs.db"))
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.AdminToken != "0123456789abcdef0123456789abcdef" {
		t.Errorf("token = %q", c.AdminToken)
	}

	t.Setenv("CERTS_ADMIN_TOKEN", "also-set")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "only one") {
		t.Errorf("both set: %v", err)
	}
}

func TestRetiredVariablesAreReported(t *testing.T) {
	t.Setenv("CERTS_ORG_NAME", "Zip Code Wilmington")
	t.Setenv("CERTS_SITE_URL", "")
	got := RetiredInUse()
	if len(got) != 1 || got[0] != "CERTS_ORG_NAME" {
		t.Fatalf("RetiredInUse = %v", got)
	}
}

func TestMasterKey(t *testing.T) {
	t.Setenv("CERTS_DB", filepath.Join(t.TempDir(), "certs.db"))
	c, err := Load()
	if err != nil || !c.DevMasterKey || len(c.MasterKey) != 32 {
		t.Fatalf("development falls back to the dev key: %v %v", err, c.DevMasterKey)
	}
	t.Setenv("CERTS_MASTER_KEY", strings.Repeat("ab", 32))
	if c, err = Load(); err != nil || c.DevMasterKey || c.MasterKey[0] != 0xab {
		t.Fatalf("hex key: %v", err)
	}
	t.Setenv("CERTS_MASTER_KEY", "too-short")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "CERTS_MASTER_KEY") {
		t.Fatalf("bad key: %v", err)
	}
	t.Setenv("CERTS_MASTER_KEY", "")
	t.Setenv("CERTS_ENV", "production")
	t.Setenv("CERTS_BASE_URL", "https://certs.example.org")
	if _, err = Load(); err == nil || !strings.Contains(err.Error(), "CERTS_MASTER_KEY is required") {
		t.Fatalf("production without key: %v", err)
	}
}

func TestCommentAsValue(t *testing.T) {
	err := commentValues([]string{
		"CERTS_ADMIN_TOKEN=# leave empty: no scripts yet",
		"CERTS_SMTP_URL=   # later",
		"CERTS_PLATFORM_NAME=Laurel Posts",
		"CERTS_MAIL_FROM=Laurel Posts <certs@laurelposts.com>",
		"OTHER=# not ours",
	})
	if err == nil || !strings.Contains(err.Error(), "CERTS_ADMIN_TOKEN is set to a comment") ||
		!strings.Contains(err.Error(), "CERTS_SMTP_URL is set to a comment") ||
		strings.Contains(err.Error(), "PLATFORM_NAME") || strings.Contains(err.Error(), "OTHER") {
		t.Fatalf("comment values: %v", err)
	}
	t.Setenv("CERTS_SMTP_URL", "# later")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "put comments on their own line") {
		t.Fatalf("Load: %v", err)
	}
}
