// Package linkedin builds LinkedIn "Add to profile" and "Share" URLs.
//
// These are plain redirect URLs; no LinkedIn API app or OAuth is needed.
package linkedin

import (
	"net/url"
	"strconv"
	"time"
)

// Certification describes a credential for LinkedIn's Add-to-Profile flow.
type Certification struct {
	Name    string    // shown as the certification name on the profile
	OrgID   string    // LinkedIn company page ID; makes the logo appear
	OrgName string    // fallback when OrgID is empty (shows as plain text)
	Issued  time.Time // issue month/year
	CertURL string    // public verification URL
	CertID  string    // credential ID
}

// AddToProfileURL returns the prefilled "Add licenses & certifications" URL.
func AddToProfileURL(c Certification) string {
	v := url.Values{}
	v.Set("startTask", "CERTIFICATION_NAME")
	v.Set("name", c.Name)
	if c.OrgID != "" {
		v.Set("organizationId", c.OrgID)
	} else if c.OrgName != "" {
		v.Set("organizationName", c.OrgName)
	}
	if !c.Issued.IsZero() {
		v.Set("issueYear", strconv.Itoa(c.Issued.Year()))
		v.Set("issueMonth", strconv.Itoa(int(c.Issued.Month())))
	}
	v.Set("certUrl", c.CertURL)
	v.Set("certId", c.CertID)
	return "https://www.linkedin.com/profile/add?" + v.Encode()
}

// ShareURL returns LinkedIn's share-a-link URL. LinkedIn builds the post
// preview from the page's Open Graph tags; use LinkedIn's Post Inspector to
// refresh its cache after changing them.
func ShareURL(pageURL string) string {
	return "https://www.linkedin.com/sharing/share-offsite/?url=" + url.QueryEscape(pageURL)
}
