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
	mu    sync.RWMutex
	path  string
	rules []IPRule
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
		if json.Unmarshal(b, &data) == nil {
			for _, rule := range data.Rules {
				if _, err := parseIPPrefix(rule.Prefix); err == nil {
					if rule.ID == "" {
						rule.ID = uuid.NewString()
					}
					m.rules = append(m.rules, rule)
				}
			}
		} else {
			log.Printf("[ip-management] ignored invalid rules file: %v", err)
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

func (m *ipManager) saveLocked() error {
	b, err := json.MarshalIndent(ipRulesFile{Rules: m.rules}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(m.path, append(b, '\n'), 0600)
}

func (m *ipManager) list() []IPRule {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]IPRule(nil), m.rules...)
}

func (m *ipManager) add(prefix, note string) (IPRule, error) {
	canonical, err := canonicalIPPrefix(prefix)
	if err != nil {
		return IPRule{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rules {
		if r.Prefix == canonical {
			return IPRule{}, errors.New("IP or CIDR rule already exists")
		}
	}
	r := IPRule{ID: uuid.NewString(), Prefix: canonical, Note: strings.TrimSpace(note), CreatedAt: time.Now().UTC()}
	m.rules = append(m.rules, r)
	if err := m.saveLocked(); err != nil {
		m.rules = m.rules[:len(m.rules)-1]
		return IPRule{}, err
	}
	return r, nil
}

func (m *ipManager) remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.rules {
		if r.ID == id {
			m.rules = append(m.rules[:i], m.rules[i+1:]...)
			if err := m.saveLocked(); err != nil {
				return err
			}
			return nil
		}
	}
	return os.ErrNotExist
}

func (m *ipManager) blocked(ip string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil {
		return false
	}
	a = a.Unmap()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.rules {
		p, err := netip.ParsePrefix(r.Prefix)
		if err == nil && p.Contains(a) {
			return true
		}
	}
	return false
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
		jsonOut(w, map[string]any{"days": days, "rules": s.ipManager.list(), "ips": s.usage.ipSnapshot(days)})
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
