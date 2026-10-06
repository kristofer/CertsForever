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
)

// Config holds runtime settings. Every field has an env var; see README.
type Config struct {
	Env           string // CERTS_ENV              "development" (default) or "production"
	Addr          string // CERTS_ADDR             listen address
	DBPath        string // CERTS_DB               SQLite file
	BaseURL       string // CERTS_BASE_URL         public origin, e.g. https://certs.zipcodewilmington.org
	AdminToken    string // CERTS_ADMIN_TOKEN[_FILE] bearer token for /admin/api; empty disables it
	OrgName       string // CERTS_ORG_NAME
	OrgBlurb      string // CERTS_ORG_BLURB        one or two sentences on the certificate page
	SiteURL       string // CERTS_SITE_URL         where "Learn about the program" goes
	LinkedInOrgID string // CERTS_LINKEDIN_ORG_ID  numeric LinkedIn company page ID
}

// Production reports whether stricter production checks apply.
func (c Config) Production() bool { return c.Env == "production" }

// FromEnv reads configuration with local-dev defaults, without validating
// it and without reading *_FILE secrets. Use Load for anything that serves
// traffic or writes data.
func FromEnv() Config {
	return Config{
		Env:        strings.ToLower(env("CERTS_ENV", "development")),
		Addr:       env("CERTS_ADDR", ":8080"),
		DBPath:     env("CERTS_DB", "certs.db"),
		BaseURL:    strings.TrimRight(env("CERTS_BASE_URL", "http://localhost:8080"), "/"),
		AdminToken: strings.TrimSpace(os.Getenv("CERTS_ADMIN_TOKEN")),
		OrgName:    env("CERTS_ORG_NAME", "Zip Code Wilmington"),
		OrgBlurb: env("CERTS_ORG_BLURB",
			"Zip Code Wilmington is a nonprofit, intensive coding bootcamp in Wilmington, Delaware, "+
				"preparing people from all backgrounds for careers in software."),
		SiteURL:       env("CERTS_SITE_URL", "https://zipcodewilmington.com"),
		LinkedInOrgID: strings.TrimSpace(os.Getenv("CERTS_LINKEDIN_ORG_ID")),
	}
}

// Load reads the environment, resolves *_FILE secrets (Docker/systemd
// secrets) and validates the result. All problems are reported together.
func Load() (Config, error) {
	c := FromEnv()
	tok, err := secret("CERTS_ADMIN_TOKEN")
	if err != nil {
		return c, err
	}
	c.AdminToken = tok
	return c, c.Validate()
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
	if u, err := url.Parse(c.SiteURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		bad("CERTS_SITE_URL %q must be an absolute http(s) URL", c.SiteURL)
	}
	if c.AdminToken != "" && len(c.AdminToken) < 32 {
		bad("CERTS_ADMIN_TOKEN must be at least 32 characters (try: openssl rand -hex 32)")
	}
	if c.LinkedInOrgID != "" && strings.Trim(c.LinkedInOrgID, "0123456789") != "" {
		bad("CERTS_LINKEDIN_ORG_ID must be numeric, got %q", c.LinkedInOrgID)
	}
	if strings.TrimSpace(c.OrgName) == "" {
		bad("CERTS_ORG_NAME is empty")
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
