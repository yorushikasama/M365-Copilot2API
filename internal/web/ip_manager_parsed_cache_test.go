package web

import (
	"path/filepath"
	"testing"
)

// match() reads m.parsed and returns m.rules[i], so the two slices must stay
// index-aligned through every mutation. A misalignment would report the wrong
// rule for a blocked address, or panic on a short slice.
func TestIPManagerKeepsParsedCacheAlignedWithRules(t *testing.T) {
	m := &ipManager{path: filepath.Join(t.TempDir(), "rules.json")}

	added := make([]IPRule, 0, 3)
	for _, prefix := range []string{"203.0.113.7", "198.51.100.0/24", "2001:db8::/32"} {
		r, err := m.add(prefix, "note")
		if err != nil {
			t.Fatalf("add(%s): %v", prefix, err)
		}
		added = append(added, r)
	}
	assertIPCacheAligned(t, m)

	// Removing the middle rule is the case that catches a splice applied to one
	// slice but not the other.
	if err := m.remove(added[1].ID); err != nil {
		t.Fatalf("remove: %v", err)
	}
	assertIPCacheAligned(t, m)

	if _, ok := m.match("198.51.100.5"); ok {
		t.Error("removed CIDR still matches")
	}
	for _, probe := range []struct {
		ip   string
		want string
	}{
		{"203.0.113.7", "203.0.113.7/32"},
		{"2001:db8::1", "2001:db8::/32"},
	} {
		rule, ok := m.match(probe.ip)
		if !ok {
			t.Fatalf("%s should match a rule", probe.ip)
		}
		if rule.Prefix != probe.want {
			t.Errorf("%s matched rule %q, want %q", probe.ip, rule.Prefix, probe.want)
		}
	}
}

func assertIPCacheAligned(t *testing.T, m *ipManager) {
	t.Helper()
	rules, parsed := m.snapshot()
	if len(rules) != len(parsed) {
		t.Fatalf("rules/parsed length mismatch: %d vs %d", len(rules), len(parsed))
	}
	for i, r := range rules {
		want, err := parseIPPrefix(r.Prefix)
		if err != nil {
			t.Fatalf("rule %d has unparseable prefix %q: %v", i, r.Prefix, err)
		}
		if parsed[i] != want {
			t.Errorf("parsed[%d] = %v, want %v (rule %q)", i, parsed[i], want, r.Prefix)
		}
	}
}

// A failed write must leave the in-memory rule set untouched, so the parsed
// cache cannot drift from the rules the file still holds.
func TestIPManagerAddFailureLeavesCacheUnchanged(t *testing.T) {
	dir := t.TempDir()
	m := &ipManager{path: filepath.Join(dir, "rules.json")}
	if _, err := m.add("203.0.113.7", ""); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	// Point the store at a path that cannot be written (a directory component
	// that is actually a file), so persist fails.
	m.path = filepath.Join(dir, "rules.json", "nested.json")
	if _, err := m.add("198.51.100.1", ""); err == nil {
		t.Fatal("add should fail when the rules file cannot be written")
	}

	rules, parsed := m.snapshot()
	if len(rules) != 1 || len(parsed) != 1 {
		t.Fatalf("failed add mutated state: %d rules, %d parsed", len(rules), len(parsed))
	}
	if _, ok := m.match("198.51.100.1"); ok {
		t.Error("a rule that never reached disk is being enforced")
	}
	if _, ok := m.match("203.0.113.7"); !ok {
		t.Error("failed add dropped the pre-existing rule")
	}
}
