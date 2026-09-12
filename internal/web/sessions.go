package web

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

type conversation struct {
	ID             string    `json:"id"`
	AccountID      string    `json:"accountId"`
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	Title          string    `json:"title,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type sessionStore struct {
	mu          sync.Mutex
	path        string
	data        map[string]conversation
	ttl         time.Duration
	maxSessions int
	persist     *persistStore
}

const defaultSessionKeyTTL = 7 * 24 * time.Hour

const maxSessionKeys = 4096

// sessionKeyStorePath resolves where the legacy sessionKey→conversation map
// lives. It must never be M365_SESSION_CACHE: sessionResolver owns that path and
// writes a JSON array there, while this store writes a JSON object. Both share
// the 5s persist loop, so pointing them at one file makes the later flush
// destroy the other's data and the loser silently starts empty after a restart.
// docker-compose sets M365_SESSION_CACHE=/data/sessions.json, so that collision
// was the shipped default.
func sessionKeyStorePath() string {
	if p := os.Getenv("M365_SESSION_KEY_CACHE"); p != "" {
		return p
	}
	// Derive from the resolver's directory so a configured data dir still holds
	// both files, but under a distinct name.
	if p := os.Getenv("M365_SESSION_CACHE"); p != "" {
		return filepath.Join(filepath.Dir(p), "session-keys.json")
	}
	return filepath.Join(os.TempDir(), "m365-copilot2api-session-keys.json")
}

func openSessionStore() *sessionStore {
	path := sessionKeyStorePath()
	s := &sessionStore{path: path, data: map[string]conversation{}, ttl: defaultSessionKeyTTL, maxSessions: maxSessionKeys}
	s.persist = &persistStore{flush: s.flush}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			log.Printf("[sessions] failed to unmarshal %s: %v", path, err)
		}
	}
	s.evictLocked()
	return s
}

// evictLocked drops entries past the TTL and, if still over capacity, the
// least recently updated ones. sessionKey is client-supplied, so without a
// bound a caller sending a fresh key per request grows this map and its file
// for the process lifetime and across restarts.
func (s *sessionStore) evictLocked() {
	if s.ttl > 0 {
		cutoff := time.Now().UTC().Add(-s.ttl)
		for k, v := range s.data {
			if v.UpdatedAt.Before(cutoff) {
				delete(s.data, k)
			}
		}
	}
	if s.maxSessions <= 0 || len(s.data) <= s.maxSessions {
		return
	}
	type aged struct {
		id string
		at time.Time
	}
	all := make([]aged, 0, len(s.data))
	for k, v := range s.data {
		all = append(all, aged{id: k, at: v.UpdatedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < len(all)-s.maxSessions; i++ {
		delete(s.data, all[i].id)
	}
}

// flush 在锁内生成快照，锁外写盘。
func (s *sessionStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func (s *sessionStore) list() []conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]conversation, 0, len(s.data))
	for _, v := range s.data {
		out = append(out, v)
	}
	return out
}

func (s *sessionStore) get(id string) (conversation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[id]
	return v, ok
}

func (s *sessionStore) upsert(v conversation) conversation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	now := time.Now().UTC()
	if v.CreatedAt.IsZero() {
		v.CreatedAt = now
	}
	v.UpdatedAt = now
	s.data[v.ID] = v
	s.evictLocked()
	s.persist.markDirty()
	return v
}

func (s *sessionStore) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[id]; !ok {
		return false
	}
	delete(s.data, id)
	s.persist.markDirty()
	return true
}

type userSession struct {
	ConversationID string    `json:"conversationId"`
	SessionID      string    `json:"sessionId"`
	AccountID      string    `json:"accountId"`
	LastUsedAt     time.Time `json:"lastUsedAt"`
}

type userSessionStore struct {
	mu      sync.Mutex
	path    string
	data    map[string]userSession
	ttl     time.Duration
	persist *persistStore
}

func openUserSessionStore(ttl time.Duration) *userSessionStore {
	path := os.Getenv("M365_USER_SESSION_CACHE")
	if path == "" {
		path = filepath.Join(os.TempDir(), "m365-copilot2api-user-sessions.json")
	}
	s := &userSessionStore{path: path, data: map[string]userSession{}, ttl: ttl}
	s.persist = &persistStore{flush: s.flush}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			log.Printf("[user-sessions] failed to unmarshal %s: %v", path, err)
		}
	}
	s.evictLocked()
	return s
}

// flush 在锁内生成快照，锁外写盘。
func (s *userSessionStore) flush() error {
	s.mu.Lock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(s.path, b, 0o600)
}

func (s *userSessionStore) evictLocked() {
	if s.ttl <= 0 {
		return
	}
	cutoff := time.Now().UTC().Add(-s.ttl)
	for k, v := range s.data {
		if v.LastUsedAt.Before(cutoff) {
			delete(s.data, k)
		}
	}
}

// userKey namespaces the client-supplied `user` field by tenant so two API
// keys that pass the same `user` value can never resume each other's
// conversation. The stored key is opaque and never returned to a caller.
func userKey(tenant, user string) string { return tenant + "\x00" + user }

func (s *userSessionStore) Get(tenant, user string) (userSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	key := userKey(tenant, user)
	v, ok := s.data[key]
	if ok {
		v.LastUsedAt = time.Now().UTC()
		s.data[key] = v
		s.persist.markDirty()
	}
	return v, ok
}

func (s *userSessionStore) Put(tenant, user, conversationID, sessionID, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[userKey(tenant, user)] = userSession{
		ConversationID: conversationID,
		SessionID:      sessionID,
		AccountID:      accountID,
		LastUsedAt:     time.Now().UTC(),
	}
	s.persist.markDirty()
}

func (s *userSessionStore) Delete(tenant, user string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, userKey(tenant, user))
	s.persist.markDirty()
}

// ActiveConversations returns conversation IDs whose owning user used the
// session within the given window. The auto-cleanup skips these so a user's
// in-flight conversation is never removed while still in use.
func (s *userSessionStore) ActiveConversations(window time.Duration) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := time.Now().UTC().Add(-window)
	out := map[string]bool{}
	for _, v := range s.data {
		if v.LastUsedAt.After(cutoff) {
			out[v.ConversationID] = true
		}
	}
	return out
}
