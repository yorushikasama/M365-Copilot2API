package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type IPRule struct {
	ID        string    `json:"id"`
	Prefix    string    `json:"prefix"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type ipRulesFile struct {
	Rules []IPRule `json:"rules"`
}

type ipManager struct {
	// saveMu serializes mutations (add/remove) so the file write happens
	// outside mu. Readers take mu.RLock only, so a blocked-IP check never
	// waits on an fsync; previously add/remove held the write lock across
	// writeFileAtomic and stalled every request for the duration.
	saveMu sync.Mutex

	mu   sync.RWMutex
	path string
	// rules and parsed are kept index-aligned. match() runs on every request,
	// and re-parsing each rule's prefix there turned a hot path into O(rules)
	// address parses per request.
	rules  []IPRule
	parsed []netip.Prefix
}

func openIPManager() *ipManager {
	path := strings.TrimSpace(os.Getenv("M365_IP_RULES"))
	if path == "" {
		dir := strings.TrimSpace(os.Getenv("M365_DATA_DIR"))
		if dir == "" {
			h, _ := os.UserHomeDir()
			dir = filepath.Join(h, ".config", "m365-copilot2api")
		}
		path = filepath.Join(dir, "ip-rules.json")
	}
	m := &ipManager{path: path}
	b, err := os.ReadFile(path)
	if err == nil {
		var data ipRulesFile
		// Report the decode failure, not the (nil) read error: this branch is
		// only reached once the file was read successfully.
		if decErr := json.Unmarshal(b, &data); decErr == nil {
			for _, rule := range data.Rules {
				if p, err := parseIPPrefix(rule.Prefix); err == nil {
					if rule.ID == "" {
						rule.ID = uuid.NewString()
					}
					m.rules = append(m.rules, rule)
					m.parsed = append(m.parsed, p)
				}
			}
		} else {
			log.Printf("[ip-management] ignored invalid rules file: %v", decErr)
		}
	}
	return m
}

func parseIPPrefix(value string) (netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, "%") {
		return netip.Prefix{}, errors.New("invalid IP or CIDR")
	}
	if strings.Contains(value, "/") {
		p, err := netip.ParsePrefix(value)
		if err != nil {
			return netip.Prefix{}, errors.New("invalid IP or CIDR")
		}
		return p.Masked(), nil
	}
	a, err := netip.ParseAddr(value)
	if err != nil {
		return netip.Prefix{}, errors.New("invalid IP or CIDR")
	}
	return netip.PrefixFrom(a.Unmap(), a.BitLen()), nil
}

func canonicalIPPrefix(value string) (string, error) {
	p, err := parseIPPrefix(value)
	if err != nil {
		return "", err
	}
	return p.String(), nil
}

// persist writes the given rule set to disk without holding m.mu, so a
// concurrent blocked-IP check is never blocked behind the fsync.
func (m *ipManager) persist(rules []IPRule) error {
	b, err := json.MarshalIndent(ipRulesFile{Rules: rules}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(m.path, append(b, '\n'), 0600)
}

// commit publishes a rule set that has already reached disk.
func (m *ipManager) commit(rules []IPRule, parsed []netip.Prefix) {
	m.mu.Lock()
	m.rules, m.parsed = rules, parsed
	m.mu.Unlock()
}

func (m *ipManager) list() []IPRule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]IPRule(nil), m.rules...)
}

func (m *ipManager) snapshot() ([]IPRule, []netip.Prefix) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]IPRule(nil), m.rules...), append([]netip.Prefix(nil), m.parsed...)
}

func (m *ipManager) add(prefix, note string) (IPRule, error) {
	canonical, err := canonicalIPPrefix(prefix)
	if err != nil {
		return IPRule{}, err
	}
	parsedPrefix, err := parseIPPrefix(canonical)
	if err != nil {
		return IPRule{}, err
	}
	// saveMu serializes mutators, so read-modify-write stays atomic with respect
	// to other add/remove calls even though m.mu is released for the write.
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	rules, parsed := m.snapshot()
	for _, r := range rules {
		if r.Prefix == canonical {
			return IPRule{}, errors.New("IP or CIDR rule already exists")
		}
	}
	r := IPRule{ID: uuid.NewString(), Prefix: canonical, Note: strings.TrimSpace(note), CreatedAt: time.Now().UTC()}
	// Publish only after the write succeeds: a rule that never reached disk must
	// not be enforced in memory, or it would silently disappear on restart.
	next := append(rules, r)
	if err := m.persist(next); err != nil {
		return IPRule{}, err
	}
	m.commit(next, append(parsed, parsedPrefix))
	return r, nil
}

func (m *ipManager) remove(id string) error {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	rules, parsed := m.snapshot()
	for i, r := range rules {
		if r.ID == id {
			keptRules := make([]IPRule, 0, len(rules)-1)
			keptRules = append(keptRules, rules[:i]...)
			keptRules = append(keptRules, rules[i+1:]...)
			keptParsed := make([]netip.Prefix, 0, len(parsed)-1)
			keptParsed = append(keptParsed, parsed[:i]...)
			keptParsed = append(keptParsed, parsed[i+1:]...)
			// The rule stays in force until the shorter list is on disk;
			// dropping it from memory first would unblock the address while the
			// file still listed it, and the block would return after a restart.
			if err := m.persist(keptRules); err != nil {
				return err
			}
			m.commit(keptRules, keptParsed)
			return nil
		}
	}
	return os.ErrNotExist
}

func (m *ipManager) match(ip string) (IPRule, bool) {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return IPRule{}, false
	}
	a = a.Unmap()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i, p := range m.parsed {
		if p.Contains(a) {
			return m.rules[i], true
		}
	}
	return IPRule{}, false
}

func (m *ipManager) blocked(ip string) bool {
	_, matched := m.match(ip)
	return matched
}

type IPResolution struct {
	IP         string   `json:"ip"`
	Type       string   `json:"type"`
	Public     bool     `json:"public"`
	ReverseDNS []string `json:"reverseDns,omitempty"`
	Geo        *IPGeo   `json:"geo,omitempty"`
}

func resolveIP(ctx context.Context, value string) (IPResolution, error) {
	a, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || a.Zone() != "" {
		return IPResolution{}, errors.New("invalid IP address")
	}
	a = a.Unmap()
	res := IPResolution{IP: a.String(), Public: true, Type: "public"}
	if a.IsLoopback() {
		res.Type, res.Public = "loopback", false
	} else if a.IsPrivate() {
		res.Type, res.Public = "private", false
	} else if a.IsLinkLocalUnicast() {
		res.Type, res.Public = "link-local", false
	} else if a.IsUnspecified() {
		res.Type, res.Public = "unspecified", false
	} else if a.IsMulticast() {
		res.Type, res.Public = "multicast", false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	names, _ := net.DefaultResolver.LookupAddr(lookupCtx, a.String())
	for i := range names {
		names[i] = strings.TrimSuffix(names[i], ".")
	}
	res.ReverseDNS = names
	if res.Public {
		res.Geo = lookupIPGeo(ctx, a.String())
	}
	return res, nil
}

func (s *Server) ipManagement(w http.ResponseWriter, r *http.Request) {
	if s.ipManager == nil {
		writeOpenAIError(w, 500, "configuration_error", "IP management unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		days := 30
		if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n > 0 && n <= 365 {
			days = n
		}
		// ipSnapshot hands back the cached maps themselves, shared by every
		// request that hits the 3s TTL. Annotating them in place both raced
		// concurrent readers and let one request's block verdict persist into
		// later responses, so decorate copies instead.
		cached := s.usage.ipSnapshot(days)
		ips := make([]map[string]any, 0, len(cached))
		for _, item := range cached {
			row := make(map[string]any, len(item)+3)
			for k, v := range item {
				row[k] = v
			}
			ip, _ := row["ip"].(string)
			rule, blocked := s.ipManager.match(ip)
			row["blocked"] = blocked
			if blocked {
				row["matched_rule"] = rule
				row["matchedRule"] = rule
			} else {
				row["matched_rule"] = nil
				row["matchedRule"] = nil
			}
			ips = append(ips, row)
		}
		jsonOut(w, map[string]any{"days": days, "rules": s.ipManager.list(), "ips": ips})
	case http.MethodPost:
		var body struct{ Prefix, Note string }
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body) != nil {
			writeOpenAIError(w, 400, "invalid_request_error", "bad json")
			return
		}
		rule, err := s.ipManager.add(body.Prefix, body.Note)
		if err != nil {
			writeOpenAIError(w, 400, "invalid_request_error", err.Error())
			return
		}
		jsonOut(w, map[string]any{"rule": rule})
	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		if id == "" {
			writeOpenAIError(w, 400, "invalid_request_error", "id is required")
			return
		}
		if err := s.ipManager.remove(id); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				writeOpenAIError(w, 404, "not_found", "IP rule not found")
			} else {
				writeOpenAIError(w, 500, "storage_error", "could not remove IP rule")
			}
			return
		}
		jsonOut(w, map[string]any{"status": "ok"})
	default:
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
	}
}

func (s *Server) ipResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	res, err := resolveIP(r.Context(), r.URL.Query().Get("ip"))
	if err != nil {
		writeOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}
	jsonOut(w, res)
}

func (s *Server) ipBlocked(r *http.Request) bool {
	return s.ipManager != nil && s.ipManager.blocked(clientIP(r))
}
