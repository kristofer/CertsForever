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
	if err := c.Validate(); err != nil {
		t.Fatalf("valid production config rejected: %v", err)
	}
}

func TestValidateReportsEverything(t *testing.T) {
	c := valid(t)
	c.Env = "staging"
	c.Addr = "8080"
	c.DBPath = "/no/such/dir/certs.db"
	c.BaseURL = "https://certs.example.org/certs"
	c.SiteURL = "zipcode"
	c.AdminToken = "short"
	c.LinkedInOrgID = "zipcode"
	err := c.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"CERTS_ENV", "CERTS_ADDR", "CERTS_DB", "origin only", "CERTS_SITE_URL",
		"CERTS_ADMIN_TOKEN", "CERTS_LINKEDIN_ORG_ID"} {
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
