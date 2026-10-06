// Package config reads and validates server configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"certsforever/internal/secretbox"
)

// Config holds runtime settings. Every field has an env var; see README.
type Config struct {
	Env          string // CERTS_ENV              "development" (default) or "production"
	Addr         string // CERTS_ADDR             listen address
	DBPath       string // CERTS_DB               SQLite file
	BaseURL      string // CERTS_BASE_URL         public origin, e.g. https://certs.zipcodewilmington.org
	AdminToken   string // CERTS_ADMIN_TOKEN[_FILE] bearer token for /admin/api; empty disables it
	PlatformName string // CERTS_PLATFORM_NAME    shown on pages that belong to no single client

	// MasterKey encrypts secrets at rest (TOTP). CERTS_MASTER_KEY[_FILE]:
	// 32 bytes as hex or base64. Required in production. Losing it means
	// every user must re-enrol two-factor authentication.
	MasterKey    []byte
	DevMasterKey bool // true when a fixed development key is in use

	SMTPURL  string // CERTS_SMTP_URL[_FILE]  smtp://user:pass@host:587 or smtps://…:465
	MailFrom string // CERTS_MAIL_FROM        e.g. "CertsForever <certs@example.org>"

	// BounceWebhookToken authenticates the email provider's bounce/complaint
	// webhook (POST /hooks/email/bounce). CERTS_BOUNCE_WEBHOOK_TOKEN[_FILE];
	// empty disables the endpoint.
	BounceWebhookToken string

	// TrustProxy: take the client IP from the last X-Forwarded-For entry
	// (set CERTS_TRUST_PROXY=true only behind your own reverse proxy).
	TrustProxy bool
}

// devMasterKey is used in development when CERTS_MASTER_KEY isn't set. It's
// public, so it protects nothing; production refuses to start without a real key.
var devMasterKey = []byte("certsforever-development-key-32b")

// Retired lists environment variables that used to configure the single
// organization. Branding now lives on each client record (see `certsforever
// client`); these are ignored and reported at startup.
var Retired = []string{"CERTS_ORG_NAME", "CERTS_ORG_BLURB", "CERTS_SITE_URL", "CERTS_LINKEDIN_ORG_ID"}

// RetiredInUse returns the retired variables that are still set.
func RetiredInUse() []string {
	var out []string
	for _, k := range Retired {
		if os.Getenv(k) != "" {
			out = append(out, k)
		}
	}
	return out
}

// Production reports whether stricter production checks apply.
func (c Config) Production() bool { return c.Env == "production" }

// FromEnv reads configuration with local-dev defaults, without validating
// it and without reading *_FILE secrets. Use Load for anything that serves
// traffic or writes data.
func FromEnv() Config {
	return Config{
		Env:          strings.ToLower(env("CERTS_ENV", "development")),
		Addr:         env("CERTS_ADDR", ":8080"),
		DBPath:       env("CERTS_DB", "certs.db"),
		BaseURL:      strings.TrimRight(env("CERTS_BASE_URL", "http://localhost:8080"), "/"),
		AdminToken:   strings.TrimSpace(os.Getenv("CERTS_ADMIN_TOKEN")),
		PlatformName: env("CERTS_PLATFORM_NAME", "CertsForever"),
		SMTPURL:      strings.TrimSpace(os.Getenv("CERTS_SMTP_URL")),
		MailFrom:     strings.TrimSpace(os.Getenv("CERTS_MAIL_FROM")),
		TrustProxy:   strings.EqualFold(os.Getenv("CERTS_TRUST_PROXY"), "true") || os.Getenv("CERTS_TRUST_PROXY") == "1",
	}
}

// Load reads the environment, resolves *_FILE secrets (Docker/systemd
// secrets) and validates the result. All problems are reported together.
func Load() (Config, error) {
	c := FromEnv()
	var errs []error
	tok, err := secret("CERTS_ADMIN_TOKEN")
	errs = append(errs, err)
	c.AdminToken = tok
	smtpURL, err := secret("CERTS_SMTP_URL")
	errs = append(errs, err)
	c.SMTPURL = smtpURL
	hook, err := secret("CERTS_BOUNCE_WEBHOOK_TOKEN")
	errs = append(errs, err)
	c.BounceWebhookToken = hook
	key, err := secret("CERTS_MASTER_KEY")
	errs = append(errs, err)
	if key != "" {
		if c.MasterKey, err = secretbox.ParseKey(key); err != nil {
			errs = append(errs, fmt.Errorf("CERTS_MASTER_KEY: %w", err))
		}
	}
	if len(c.MasterKey) == 0 && !c.Production() {
		c.MasterKey, c.DevMasterKey = devMasterKey, true
	}
	errs = append(errs, c.Validate())
	return c, errors.Join(errs...)
}

// Validate checks the configuration and returns every problem found.
func (c Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.Env != "development" && c.Env != "production" {
		bad("CERTS_ENV must be \"development\" or \"production\", got %q", c.Env)
	}
	if _, _, err := net.SplitHostPort(c.Addr); err != nil {
		bad("CERTS_ADDR %q is not host:port (e.g. \":8080\"): %v", c.Addr, err)
	}
	if c.DBPath == "" {
		bad("CERTS_DB is empty")
	} else if fi, err := os.Stat(filepath.Dir(c.DBPath)); err != nil || !fi.IsDir() {
		bad("CERTS_DB directory %q does not exist", filepath.Dir(c.DBPath))
	}

	if u, err := url.Parse(c.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		bad("CERTS_BASE_URL %q must be an absolute http(s) URL", c.BaseURL)
	} else {
		if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			bad("CERTS_BASE_URL %q must be an origin only (no path, query or fragment)", c.BaseURL)
		}
		if c.Production() {
			if u.Scheme != "https" {
				bad("CERTS_BASE_URL must use https in production (certificate links are permanent)")
			}
			if h := u.Hostname(); h == "localhost" || net.ParseIP(h) != nil {
				bad("CERTS_BASE_URL host %q is not a public domain name", h)
			}
		}
	}
	if c.AdminToken != "" && len(c.AdminToken) < 32 {
		bad("CERTS_ADMIN_TOKEN must be at least 32 characters (try: openssl rand -hex 32)")
	}
	if strings.TrimSpace(c.PlatformName) == "" {
		bad("CERTS_PLATFORM_NAME is empty")
	}
	if c.Production() && (len(c.MasterKey) != 32 || c.DevMasterKey) {
		bad("CERTS_MASTER_KEY is required in production (32 bytes; try: openssl rand -hex 32)")
	}
	if c.BounceWebhookToken != "" && len(c.BounceWebhookToken) < 32 {
		bad("CERTS_BOUNCE_WEBHOOK_TOKEN must be at least 32 characters (try: openssl rand -hex 32)")
	}
	if c.SMTPURL != "" {
		if u, err := url.Parse(c.SMTPURL); err != nil || (u.Scheme != "smtp" && u.Scheme != "smtps") || u.Hostname() == "" {
			bad("CERTS_SMTP_URL must look like smtp://user:pass@host:587 or smtps://user:pass@host:465")
		}
		if c.MailFrom == "" {
			bad("CERTS_MAIL_FROM is required when CERTS_SMTP_URL is set")
		}
	}
	return errors.Join(errs...)
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// secret returns $KEY, or the trimmed contents of the file named by
// $KEY_FILE. Setting both is an error.
func secret(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	file := strings.TrimSpace(os.Getenv(key + "_FILE"))
	switch {
	case file == "":
		return v, nil
	case v != "":
		return "", fmt.Errorf("set only one of %s and %s_FILE", key, key)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("%s_FILE: %w", key, err)
	}
	return strings.TrimSpace(string(b)), nil
}
