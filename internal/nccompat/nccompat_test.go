package nccompat

// Unit tests for the pure decisions this package makes. The HTTP surface is covered end to
// end through the real router in internal/server; what is pinned here is the arithmetic and
// the parsing that a handler test would only exercise indirectly.

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShortClientNameProducesALabelAPersonRecognises(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		{"Nextcloud Desktop Client/3.13.0 (Windows)", "Nextcloud Desktop Client/3.13.0"},
		{"Mozilla/5.0 (Android) Nextcloud/3.30.0", "Mozilla/5.0"},
		{"", "Nextcloud client"},
		{"   ", "Nextcloud client"},
		{"(leading parenthesis)", "(leading parenthesis)"},
	}
	for _, tc := range cases {
		if got := shortClientName(tc.ua); got != tc.want {
			t.Errorf("shortClientName(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}

	// The label becomes a database column and a UI string. A client that sends a
	// megabyte-long User-Agent must not be able to write one.
	long := shortClientName(strings.Repeat("A", 500))
	if len(long) > maxClientName {
		t.Fatalf("a long User-Agent produced a %d-character label", len(long))
	}
}

func TestQuotaForSpeaksTheProtocolsUnlimitedValue(t *testing.T) {
	// -3 is Nextcloud's "unlimited". A client that reads 0 interprets it as "no space" and
	// refuses to upload, which would make every quota-less account unusable.
	for _, q := range []int64{0, -1} {
		got := quotaFor(q, 1234)
		if got.Total != unlimitedQuota || got.Free != unlimitedQuota || got.Quota != unlimitedQuota {
			t.Fatalf("quotaFor(%d) = %+v, want the unlimited sentinel", q, got)
		}
		if got.Used != 1234 {
			t.Fatalf("an unlimited account still reports usage: %+v", got)
		}
	}

	got := quotaFor(1000, 250)
	if got.Total != 1000 || got.Used != 250 || got.Free != 750 || got.Quota != 1000 {
		t.Fatalf("quotaFor(1000, 250) = %+v", got)
	}
	if got.Relative != 25 {
		t.Fatalf("relative = %v, want 25", got.Relative)
	}

	// Over quota must not report negative free space: the client would do arithmetic on it
	// and either underflow or present a nonsensical number.
	over := quotaFor(100, 150)
	if over.Free != 0 {
		t.Fatalf("over-quota free = %d, want 0", over.Free)
	}
}

func TestWantsJSON(t *testing.T) {
	mk := func(target string, accept string) bool {
		r := httptest.NewRequest("GET", target, nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		return wantsJSON(r)
	}
	if !mk("/ocs/v1.php/cloud/user?format=json", "") {
		t.Fatal("format=json was not recognised")
	}
	if !mk("/ocs/v1.php/cloud/user?format=JSON", "") {
		t.Fatal("the format parameter must be case-insensitive")
	}
	if !mk("/ocs/v1.php/cloud/user", "application/json") {
		t.Fatal("an Accept header asking for JSON was ignored")
	}
	if mk("/ocs/v1.php/cloud/user", "") {
		t.Fatal("the default format must be XML, which is what an OCS v1 client expects")
	}
}

func TestParseProtocolVersion(t *testing.T) {
	major, minor, micro := parseProtocolVersion()
	if major != 33 || minor != 0 || micro != 0 {
		// This is the number clients compare against their minimum. Changing it is a
		// compatibility decision, not a version bump, so it fails here rather than
		// silently making every client refuse the server.
		t.Fatalf("reported protocol version is %d.%d.%d; update the test and ADR-0015 "+
			"deliberately if this changes", major, minor, micro)
	}
}

func TestSameOriginAcceptsOnlyThisServer(t *testing.T) {
	h := New(Deps{PublicURL: "https://cloud.example.test"})

	req := httptest.NewRequest("POST", "https://cloud.example.test/login/v2/flow/x", nil)
	if !h.sameOrigin(req) {
		t.Fatal("a same-origin POST was refused")
	}
	req.Header.Set("Origin", "https://cloud.example.test")
	if !h.sameOrigin(req) {
		t.Fatal("the configured origin was refused")
	}
	req.Header.Set("Origin", "https://evil.example")
	if h.sameOrigin(req) {
		t.Fatal("a cross-origin approval was accepted; the consent POST must not be " +
			"submittable from another site")
	}
}
