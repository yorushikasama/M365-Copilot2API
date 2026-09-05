package outbound

import (
	"errors"
	"testing"
)

func TestRemoveProxyNormalizesAndRejectsMissing(t *testing.T) {
	if err := ConfigurePool([]string{"http://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveProxy("http://example.com"); err != nil {
		t.Fatal(err)
	}
	if len(ProxyPoolStatus()) != 0 {
		t.Fatalf("pool not empty: %#v", ProxyPoolStatus())
	}
	if err := RemoveProxy("http://missing.example"); err == nil {
		t.Fatal("expected missing proxy error")
	}
}

func TestAddProxyPreservesExistingEntryState(t *testing.T) {
	if err := ConfigurePool([]string{"http://example.com/"}); err != nil {
		t.Fatal(err)
	}
	CurrentPool().MarkProxyFailure("http://example.com/", errors.New("socks5 dial timeout"))
	if entries := ProxyPoolStatus(); len(entries) != 1 || entries[0]["health"] != "cooldown" {
		t.Fatalf("expected cooldown state: %#v", entries)
	}
	if err := AddProxy("http://other.example"); err != nil {
		t.Fatal(err)
	}
	entries := ProxyPoolStatus()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries: %#v", entries)
	}
	if entries[0]["health"] != "cooldown" {
		t.Fatalf("cooldown state lost on add: %#v", entries[0])
	}
}
