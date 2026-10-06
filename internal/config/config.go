// Package config reads server configuration from the environment.
package config

import (
	"os"
	"strings"
)

// Config holds runtime settings. Every field has an env var; see README.
type Config struct {
	Addr          string // CERTS_ADDR             listen address
	DBPath        string // CERTS_DB               SQLite file
	BaseURL       string // CERTS_BASE_URL         public origin, e.g. https://certs.zipcodewilmington.org
	AdminToken    string // CERTS_ADMIN_TOKEN      bearer token for /admin/api; empty disables it
	OrgName       string // CERTS_ORG_NAME
	OrgBlurb      string // CERTS_ORG_BLURB        one or two sentences on the certificate page
	SiteURL       string // CERTS_SITE_URL         where "Learn about the program" goes
	LinkedInOrgID string // CERTS_LINKEDIN_ORG_ID  numeric LinkedIn company page ID
}

// FromEnv loads configuration with sensible local-dev defaults.
func FromEnv() Config {
	return Config{
		Addr:       env("CERTS_ADDR", ":8080"),
		DBPath:     env("CERTS_DB", "certs.db"),
		BaseURL:    strings.TrimRight(env("CERTS_BASE_URL", "http://localhost:8080"), "/"),
		AdminToken: os.Getenv("CERTS_ADMIN_TOKEN"),
		OrgName:    env("CERTS_ORG_NAME", "Zip Code Wilmington"),
		OrgBlurb: env("CERTS_ORG_BLURB",
			"Zip Code Wilmington is a nonprofit, intensive coding bootcamp in Wilmington, Delaware, "+
				"preparing people from all backgrounds for careers in software."),
		SiteURL:       env("CERTS_SITE_URL", "https://zipcodewilmington.com"),
		LinkedInOrgID: os.Getenv("CERTS_LINKEDIN_ORG_ID"),
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
