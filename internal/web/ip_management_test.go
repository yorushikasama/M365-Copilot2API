package web

import (
	"context"
	"path/filepath"
	"testing"
)

func TestIPManagerNormalizesPersistsAndMatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ip-rules.json")
	t.Setenv("M365_IP_RULES", path)
	m := openIPManager()
	rule, err := m.add("2001:db8::1", "test")
	if err != nil {
		t.Fatal(err)
	}
	if rule.Prefix != "2001:db8::1/128" || !m.blocked("2001:db8::1") || m.blocked("2001:db8::2") {
		t.Fatalf("rule=%+v", rule)
	}
	m2 := openIPManager()
	if len(m2.list()) != 1 || !m2.blocked("2001:db8::1") {
		t.Fatal("rule was not persisted")
	}
	if _, err := m2.add("2001:db8::1", "duplicate"); err == nil {
		t.Fatal("duplicate rule accepted")
	}
}

func TestIPManagerCIDRRemoveAndResolve(t *testing.T) {
	m := &ipManager{path: filepath.Join(t.TempDir(), "rules.json")}
	rule, err := m.add("192.0.2.0/24", "range")
	if err != nil {
		t.Fatal(err)
	}
	if !m.blocked("192.0.2.42") || m.blocked("192.0.3.1") {
		t.Fatal("CIDR matching incorrect")
	}
	if err := m.remove(rule.ID); err != nil || m.blocked("192.0.2.42") {
		t.Fatal("remove failed")
	}
	res, err := resolveIP(context.Background(), "127.0.0.1")
	if err != nil || res.Type != "loopback" || res.Public {
		t.Fatalf("resolution=%+v err=%v", res, err)
	}
	if _, err := resolveIP(context.Background(), "fe80::1%eth0"); err == nil {
		t.Fatal("zone-scoped address accepted")
	}
}
