package chathub

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// safeDialControl is the gate that survives DNS rebinding: validateRemoteDownloadURL
// resolves the name and inspects the answer, but the socket re-resolves when it
// dials, so a resolver that alternates answers passes validation and then
// connects to a private address. These cases assert the check runs on the
// address actually being connected to.
func TestSafeDialControlRejectsNonPublicAddresses(t *testing.T) {
	blocked := []string{
		"169.254.169.254:80", // cloud metadata
		"127.0.0.1:8080",     // loopback
		"10.0.0.5:443",       // RFC1918
		"192.168.1.1:443",    // RFC1918
		"172.16.0.1:443",     // RFC1918
		"100.64.0.1:443",     // CGNAT
		"[::1]:443",          // IPv6 loopback
		"0.0.0.0:443",        // unspecified
		"[fe80::1]:443",      // IPv6 link-local
	}
	for _, addr := range blocked {
		if err := safeDialControl("tcp", addr, nil); err == nil {
			t.Errorf("%s must be rejected at dial time", addr)
		}
	}

	allowed := []string{"93.184.216.34:443", "[2606:2800:220:1:248:1893:25c8:1946]:443"}
	for _, addr := range allowed {
		if err := safeDialControl("tcp", addr, nil); err != nil {
			t.Errorf("%s must be allowed, got %v", addr, err)
		}
	}
}

// An address the dialer could not reduce to an IP must fail closed rather than
// be waved through.
func TestSafeDialControlRejectsUnresolvedAddress(t *testing.T) {
	if err := safeDialControl("tcp", "metadata.internal:80", nil); err == nil {
		t.Fatal("a non-IP dial address must be rejected")
	}
}

func TestIPUnsafeCoversMetadataAndCGNAT(t *testing.T) {
	for _, raw := range []string{"169.254.169.254", "100.100.0.1", "127.0.0.53", "::1"} {
		if !ipUnsafe(net.ParseIP(raw)) {
			t.Errorf("%s must be classified unsafe", raw)
		}
	}
}

// The download client must not borrow the shared pooled transport: a live idle
// connection there could be reused with no resolution and no dial-time check.
func TestDownloadClientUsesItsOwnTransport(t *testing.T) {
	c := NewClient()
	dc := c.downloadClient()
	if dc.Transport == nil {
		t.Fatal("download client has no transport")
	}
	// Compare against the transport an ordinary outbound call would use. Reading
	// the (normally nil) HTTPClient field instead made this assertion vacuous.
	if hc := c.HTTPClientForRequest(); hc != nil && dc.Transport == hc.Transport {
		t.Fatal("download client must not reuse the shared transport")
	}
	if dc.CheckRedirect == nil {
		t.Fatal("download client must keep the redirect guard")
	}
}

func TestValidateRemoteDownloadURLRejectsNonHTTPSAndBadHost(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/a.png",
		"ftp://example.com/a.png",
		"https:///a.png",
	} {
		if err := validateRemoteDownloadURL(raw); err == nil {
			t.Errorf("%s must be rejected", raw)
		}
	}
	// A literal private address is refused without needing any resolution.
	if err := validateRemoteDownloadURL("https://127.0.0.1/a.png"); err == nil {
		t.Fatal("loopback URL must be rejected")
	} else if !strings.Contains(err.Error(), "non-public") && !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// End-to-end: the dedicated transport must refuse to connect to a loopback
// server even when the URL is handed to it directly. This is the case
// validateRemoteDownloadURL cannot catch on its own, because a rebinding
// resolver passes validation and only the dial sees the real address.
func TestDownloadTransportRefusesLoopbackServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("internal secret"))
	}))
	defer srv.Close()

	c := NewClient()
	resp, err := c.downloadClient().Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("download transport connected to a loopback server; the dial guard did not fire")
	}
	if !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("expected the dial guard to reject it, got %v", err)
	}
}
