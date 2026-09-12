package web

import (
	"path/filepath"
	"testing"
)

// The two session stores write incompatible formats to disk: sessionResolver
// marshals a JSON array, sessionStore a JSON object. They share the 5s persist
// loop, so if they ever resolve to the same file the later flush destroys the
// other's data and the loser silently starts empty on the next boot. Both used
// to read M365_SESSION_CACHE, and docker-compose sets it, so this was the
// shipped default.
func TestSessionStoresNeverSharePath(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "sessions.json")
	t.Setenv("M365_SESSION_CACHE", shared)

	resolver := openSessionResolver()
	keys := openSessionStore()

	if keys.path == resolver.path {
		t.Fatalf("both stores resolved to %s; the persist loop would have them overwrite each other", keys.path)
	}
	if resolver.path != shared {
		t.Fatalf("resolver should keep M365_SESSION_CACHE, got %s", resolver.path)
	}
	if got, want := keys.path, filepath.Join(dir, "session-keys.json"); got != want {
		t.Fatalf("session key store path = %s, want %s", got, want)
	}
}

func TestSessionKeyCacheEnvOverridesDerivedPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	explicit := filepath.Join(dir, "explicit-keys.json")
	t.Setenv("M365_SESSION_KEY_CACHE", explicit)

	if got := openSessionStore().path; got != explicit {
		t.Fatalf("path = %s, want %s", got, explicit)
	}
}

// sessionKey is client-supplied, so the store needs a bound; without one a
// caller sending a fresh key per request grows the map and its file forever.
func TestSessionStoreEvictsBeyondCap(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_KEY_CACHE", filepath.Join(dir, "keys.json"))
	s := openSessionStore()
	s.maxSessions = 8

	for i := 0; i < 50; i++ {
		s.upsert(conversation{ID: string(rune('a'+i%26)) + string(rune('0'+i/26)), ConversationID: "c"})
	}
	if len(s.data) > 8 {
		t.Fatalf("store holds %d entries, cap is 8", len(s.data))
	}
}
