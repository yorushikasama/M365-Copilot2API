package chathub

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"m365-copilot2api/internal/outbound"
)

// validateRemoteDownloadURL blocks SSRF: only https and public routable
// addresses are accepted, with a lookup-time recheck against private,
// loopback, link-local and cloud metadata ranges.
func validateRemoteDownloadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid attachment URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("attachment download requires https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("attachment URL has no host")
	}
	// Bound the resolve: net.LookupIP takes no context, so a slow or hostile
	// resolver could hold the attachment path open indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return fmt.Errorf("attachment host does not resolve")
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	for _, ip := range ips {
		if ipUnsafe(ip) {
			return fmt.Errorf("attachment URL targets a non-public address")
		}
	}
	return nil
}

func ipUnsafe(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		// 169.254.0.0/16 link-local is covered above on Go >= 1.17;
		// 100.64.0.0/10 (CGNAT) is not private per IP.IsPrivate.
		if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			return true
		}
	}
	// 169.254.169.254 cloud metadata is link-local; belt and braces.
	if strings.HasPrefix(ip.String(), "169.254.169.254") {
		return true
	}
	return false
}

// safeDialControl is the authoritative SSRF gate. validateRemoteDownloadURL
// resolves the name and checks the answer, but the connection is dialled
// separately and re-resolves, so a DNS server that alternates answers passes
// validation and then gets dialled against a private address (classic DNS
// rebinding / TOCTOU). This hook runs on the address the socket is actually
// about to connect to, on every hop, which closes that window.
//
// It is installed only when no proxy is configured: a proxy endpoint is
// frequently a private or loopback address, and rejecting it here would break
// every proxied download.
func safeDialControl(_ string, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("attachment dial: unresolved address %q", address)
	}
	if ipUnsafe(ip) {
		return fmt.Errorf("attachment dial: %s is not a public address", ip)
	}
	return nil
}

// downloadTransport hands back the transport dedicated to attachment
// downloads. It must not be the shared process transport: that one keeps up to
// 100 idle connections, so a pooled socket to an attacker-controlled host could
// be reused with no resolution and no dial-time check at all.
//
// The two variants are cached rather than built per request: a fresh
// http.Transport per download never has its idle connections reaped by anyone,
// which leaks a socket per attachment. Proxy configuration can change at
// runtime (the admin console can add a pool), so the guarded and unguarded
// transports are memoised separately and selected per call.
var (
	downloadTransportMu      sync.Mutex
	downloadTransportGuarded *http.Transport
	downloadTransportProxied *http.Transport
)

func downloadTransport() *http.Transport {
	proxied := outbound.ProxyConfigured()
	downloadTransportMu.Lock()
	defer downloadTransportMu.Unlock()
	if proxied {
		if downloadTransportProxied == nil {
			downloadTransportProxied = newDownloadTransport(false)
		}
		return downloadTransportProxied
	}
	if downloadTransportGuarded == nil {
		downloadTransportGuarded = newDownloadTransport(true)
	}
	return downloadTransportGuarded
}

func newDownloadTransport(guard bool) *http.Transport {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	if guard {
		d.Control = safeDialControl
	}
	return &http.Transport{
		DialContext:           d.DialContext,
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}
}
