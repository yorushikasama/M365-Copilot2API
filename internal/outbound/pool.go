package outbound

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type poolEntry struct {
	raw       string
	clients   *Clients
	failures  int
	cooldown  time.Time
	lastCheck time.Time
	latency   time.Duration
	lastError string
	health    string
}
type Pool struct {
	mu      sync.Mutex
	entries []*poolEntry
	next    int
}

func NewPool(raw []string) (*Pool, error) {
	p := &Pool{}
	seen := map[string]bool{}
	for _, v := range raw {
		if v == "" || seen[v] {
			continue
		}
		c, err := New(v)
		if err != nil {
			return nil, fmt.Errorf("proxy %q: %w", v, err)
		}
		seen[v] = true
		p.entries = append(p.entries, &poolEntry{raw: v, clients: c})
	}
	return p, nil
}
func (p *Pool) pick() *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return nil
	}
	now := time.Now()
	for i := 0; i < len(p.entries); i++ {
		e := p.entries[(p.next+i)%len(p.entries)]
		if now.Before(e.cooldown) {
			continue
		}
		p.next = (p.next + i + 1) % len(p.entries)
		return e
	}
	e := p.entries[p.next%len(p.entries)]
	p.next = (p.next + 1) % len(p.entries)
	return e
}
func isProxyError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "quota_429") || strings.Contains(s, "overload_503") || strings.Contains(s, "auth_expired_401") || strings.Contains(s, "forbidden_403") || strings.Contains(s, "upstream 429") || strings.Contains(s, "upstream 401") || strings.Contains(s, "upstream 403") || strings.Contains(s, "upstream 503") {
		return false
	}
	if strings.Contains(s, "429") && strings.Contains(s, "upstream") {
		return false
	}
	if strings.Contains(s, "socks") || strings.Contains(s, "no such host") || strings.Contains(s, "dns") && !strings.Contains(s, "limited") || strings.Contains(s, "tls") || strings.Contains(s, "certificate") || strings.Contains(s, "x509") || strings.Contains(s, "handshake") || strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded") || strings.Contains(s, "connection refused") || strings.Contains(s, "connection reset") || strings.Contains(s, "network is unreachable") || strings.Contains(s, "ws_read_timeout") || strings.Contains(s, "ws_handshake") {
		return true
	}
	if strings.Contains(s, "proxy connect") || strings.Contains(s, "socks5") {
		return true
	}
	return false
}

func proxyBaseDuration(err error) time.Duration {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "socks"):
		return 30 * time.Second
	case strings.Contains(s, "no such host") || strings.Contains(s, "dns"):
		return 30 * time.Second
	case strings.Contains(s, "tls") || strings.Contains(s, "certificate") || strings.Contains(s, "x509"):
		return 30 * time.Second
	case strings.Contains(s, "handshake"):
		return 15 * time.Second
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline"):
		return 30 * time.Second
	case strings.Contains(s, "connection refused") || strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe"):
		return 15 * time.Second
	default:
		return 15 * time.Second
	}
}

func (p *Pool) mark(raw string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.raw == raw {
			if err == nil {
				e.failures = 0
				e.cooldown = time.Time{}
				e.lastError = ""
				e.health = "reachable"
			} else {
				if !isProxyError(err) {
					e.lastError = err.Error()
					return
				}
				e.failures++
				base := proxyBaseDuration(err)
				shift := e.failures - 1
				if shift > 6 {
					shift = 6
				}
				d := base * time.Duration(1<<shift)
				if d > 5*time.Minute || d <= 0 {
					d = 5 * time.Minute
				}
				if d < 5*time.Second {
					d = 5 * time.Second
				}
				e.cooldown = time.Now().Add(d)
				e.lastError = err.Error()
				e.health = "cooldown"
			}
			return
		}
	}
}

// MarkProxyFailure is used for explicit proxy-level cooldown with account isolation.
func (p *Pool) MarkProxyFailure(raw string, err error) { p.mark(raw, err) }

// IsProxyIsolated reports whether the error is a proxy transport error that
// should not affect account health (account isolation).
func IsProxyIsolated(err error) bool { return isProxyError(err) }
func (p *Pool) HTTPClient() *http.Client {
	if e := p.pick(); e != nil {
		return &http.Client{Transport: &poolRoundTripper{pool: p, entry: e, base: e.clients.HTTP.Transport}}
	}
	return directClients().HTTP
}
func (p *Pool) WebSocketDialer() *websocket.Dialer {
	base := directClients().WebSocket
	baseDialer := &net.Dialer{}
	base.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		// One pick per dial: NetDialContext runs once per WebSocket connection,
		// so a connection still never switches proxies mid-flight, but successive
		// connections must rotate through the pool and respect cooldowns. The
		// previous sticky selection pinned every connection to the first healthy
		// proxy for the whole process lifetime and ignored cooldowns marked by
		// HTTP traffic on the same entries.
		e := p.pick()
		if e == nil {
			return baseDialer.DialContext(ctx, network, address)
		}
		var conn net.Conn
		var err error
		if proxyFn := e.clients.WebSocket.Proxy; proxyFn != nil {
			// http:// entries carry their proxy in WebSocket.Proxy while their
			// NetDialContext stays the direct dialer, so using NetDialContext
			// would silently bypass the proxy. Tunnel via CONNECT instead.
			var pu *url.URL
			pu, err = proxyFn(&http.Request{URL: &url.URL{}})
			if err == nil && pu != nil {
				conn, err = httpProxyTunnel(ctx, pu, address)
			} else if err == nil {
				conn, err = baseDialer.DialContext(ctx, network, address)
			}
		} else {
			dial := e.clients.WebSocket.NetDialContext
			if dial == nil {
				dial = baseDialer.DialContext
			}
			conn, err = dial(ctx, network, address)
		}
		p.mark(e.raw, err)
		return conn, err
	}
	return base
}

// httpProxyTunnel dials address through an HTTP proxy with CONNECT, so
// WebSocket traffic over http:// pool entries is actually proxied instead of
// falling back to a direct connection.
func httpProxyTunnel(ctx context.Context, proxyURL *url.URL, address string) (net.Conn, error) {
	host := proxyURL.Host
	if proxyURL.Port() == "" {
		host = net.JoinHostPort(proxyURL.Hostname(), "80")
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	q := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: address},
		Host:   address,
		Header: make(http.Header),
	}
	if proxyURL.User != nil {
		pw, _ := proxyURL.User.Password()
		q.SetBasicAuth(proxyURL.User.Username(), pw)
	}
	if err := q.Write(raw); err != nil {
		raw.Close()
		return nil, err
	}
	rd := bufio.NewReader(raw)
	resp, err := http.ReadResponse(rd, q)
	if err != nil {
		raw.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		raw.Close()
		return nil, fmt.Errorf("HTTP proxy CONNECT %s: %s", address, resp.Status)
	}
	return &bufferedConn{Conn: raw, reader: rd}, nil
}
func (p *Pool) List() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, map[string]any{"url": e.raw, "failures": e.failures, "cooldownUntil": e.cooldown, "lastCheck": e.lastCheck, "latencyMs": e.latency.Milliseconds(), "lastError": e.lastError, "health": e.health})
	}
	return out
}

// Add appends a proxy to the pool in place. A full rebuild would forget the
// cooldowns and failure counts of existing entries, re-enabling a proxy that
// was cooling down the moment an unrelated one is added.
func (p *Pool) Add(raw string) error {
	c, err := New(raw)
	if err != nil {
		return fmt.Errorf("proxy %q: %w", raw, err)
	}
	normalized := normalizeProxyURL(raw)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if normalizeProxyURL(e.raw) == normalized {
			return nil
		}
	}
	p.entries = append(p.entries, &poolEntry{raw: raw, clients: c})
	return nil
}

func (p *Pool) Remove(raw string) bool {
	normalized := normalizeProxyURL(raw)
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if normalizeProxyURL(e.raw) == normalized {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			return true
		}
	}
	return false
}

// normalizeProxyURL is the comparison form used by the admin add/remove paths:
// trailing slashes are cosmetic and must not defeat an exact-match remove.
func normalizeProxyURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

type poolRoundTripper struct {
	pool  *Pool
	entry *poolEntry
	base  http.RoundTripper
}

func (t *poolRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	t.pool.mark(t.entry.raw, err)
	if err == nil {
		return resp, nil
	}
	// Replay the request on the next healthy proxy once (body must be replayable).
	if r.Body != nil && r.GetBody == nil {
		return resp, err
	}
	t.pool.mu.Lock()
	n := len(t.pool.entries) + 1
	t.pool.mu.Unlock()
	if n > 3 {
		n = 3
	}
	for i := 0; i < n; i++ {
		next := t.pool.pick()
		if next == nil || next == t.entry {
			break
		}
		body, berr := r.GetBody()
		if berr != nil {
			break
		}
		retry := r.Clone(r.Context())
		retry.Body = body
		resp2, err2 := next.clients.HTTP.Transport.RoundTrip(retry)
		t.pool.mark(next.raw, err2)
		if err2 == nil {
			return resp2, nil
		}
	}
	return resp, err
}
