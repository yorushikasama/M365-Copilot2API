package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpsertAndList(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	acc, err := store.Upsert(TokenSet{
		AccessToken:  "a",
		RefreshToken: "r",
		Email:        "a@example.com",
		DisplayName:  "A",
		HomeOID:      "oid-1",
		ExpiresAt:    time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if acc.Email != "a@example.com" {
		t.Fatalf("unexpected email: %s", acc.Email)
	}
	list := store.List()
	if len(list) != 1 {
		t.Fatalf("expected 1 account, got %d", len(list))
	}
}

func TestScheduleEnabledPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	token := TokenSet{AccessToken: "a", RefreshToken: "r", Email: "a@example.com", HomeOID: "oid-1", ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := store.Upsert(token); err != nil {
		t.Fatal(err)
	}
	if !store.ScheduleEnabled("oid-1") {
		t.Fatal("new account scheduling disabled")
	}
	if err := store.SetScheduleEnabled("oid-1", false); err != nil {
		t.Fatal(err)
	}
	if store.ScheduleEnabled("oid-1") {
		t.Fatal("account scheduling still enabled")
	}
	if _, err := store.Upsert(token); err != nil {
		t.Fatal(err)
	}
	if store.ScheduleEnabled("oid-1") {
		t.Fatal("upsert reset scheduling state")
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.ScheduleEnabled("oid-1") {
		t.Fatal("scheduling state was not persisted")
	}
}

// TestMasterKeyMigration verifies that a cache written under the built-in
// fallback key stays readable (and is re-encrypted) after M365_MASTER_KEY is
// configured.
func TestMasterKeyMigration(t *testing.T) {
	t.Setenv("M365_MASTER_KEY", "")
	path := filepath.Join(t.TempDir(), "tokens.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Upsert(TokenSet{
		AccessToken:  "a",
		RefreshToken: "legacy-plain-token",
		Email:        "a@example.com",
		HomeOID:      "oid-1",
		ExpiresAt:    time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	// Now configure a real master key and reopen: the fallback-encrypted
	// token must decrypt via the legacy path and be re-encrypted on save.
	t.Setenv("M365_MASTER_KEY", "unit-test-master-key")
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	acc, ok := reopened.Get("oid-1")
	if !ok {
		t.Fatal("account lost after reopen")
	}
	if acc.RefreshToken != "legacy-plain-token" {
		t.Fatalf("refresh token not recovered via legacy key: %q", acc.RefreshToken)
	}
	// Touch the store so saveLocked runs under the new key.
	if err := reopened.SetScheduleEnabled("oid-1", true); err != nil {
		t.Fatal(err)
	}

	// Third open with the new key: no legacy path needed, token still works.
	third, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	acc3, ok := third.Get("oid-1")
	if !ok || acc3.RefreshToken != "legacy-plain-token" {
		t.Fatalf("token unreadable after re-encryption: ok=%v token=%q", ok, acc3.RefreshToken)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "enc:v1:") {
		t.Fatal("expected refresh token to be encrypted at rest")
	}
}
