package linkedin

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAddToProfileURL(t *testing.T) {
	raw := AddToProfileURL(Certification{
		Name:    "Java Full-Stack Developer",
		OrgID:   "1234567",
		Issued:  time.Date(2026, time.October, 2, 0, 0, 0, 0, time.UTC),
		CertURL: "https://certs.example.org/c/ZCW-7K3M9QF2XA",
		CertID:  "ZCW-7K3M9QF2XA",
	})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	want := map[string]string{
		"startTask":      "CERTIFICATION_NAME",
		"name":           "Java Full-Stack Developer",
		"organizationId": "1234567",
		"issueYear":      "2026",
		"issueMonth":     "10",
		"certUrl":        "https://certs.example.org/c/ZCW-7K3M9QF2XA",
		"certId":         "ZCW-7K3M9QF2XA",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if q.Has("organizationName") {
		t.Error("organizationName should be omitted when organizationId is set")
	}
}

func TestShareURL(t *testing.T) {
	got := ShareURL("https://certs.example.org/c/ZCW-1?x=1")
	if !strings.HasSuffix(got, "url=https%3A%2F%2Fcerts.example.org%2Fc%2FZCW-1%3Fx%3D1") {
		t.Errorf("ShareURL not escaped: %s", got)
	}
}
