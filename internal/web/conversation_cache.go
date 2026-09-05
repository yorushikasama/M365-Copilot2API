package web

import (
	"crypto/sha256"
	"encoding/hex"
	"m365-copilot2api/internal/chathub"
	"strings"
	"sync"
	"time"
)

type cachedConversation struct {
	ConversationID string
	SessionID      string
	Tone           string
	TurnCount      int
	MessageCount   int
	CreatedAt      time.Time
	LastUsedAt     time.Time
	SystemPrompt   string
	// PrefixHash is the fingerprint of this entry's prefix messages (the first
	// MessageCount rows of the request that created it). A later request is only
	// allowed to reuse the upstream conversation when its own prefix matches,
	// otherwise its history differs from what the cloud conversation already
	// contains and splicing an increment onto the tail would silently mix two
	// different clients' or two different contexts' histories together.
	PrefixHash string
}

type conversationCache struct {
	mu      sync.Mutex
	entries map[string]*cachedConversation
	maxAge  time.Duration
}

func newConversationCache() *conversationCache {
	return &conversationCache{
		entries: make(map[string]*cachedConversation),
		maxAge:  2 * time.Hour,
	}
}

func (c *conversationCache) key(accountID, model string) string {
	return accountID + "|" + model
}

// Lookup returns the cached conversation for account/model only when the
// caller's message prefix still matches the one the cache entry was built from.
//
// Matching by account+model+system-prompt alone is not enough: the upstream
// conversation already holds the historical messages, so reusing it with a
// different prefix would concatenate this caller's increment onto someone else's
// (or some other context's) history — exactly the cross-client bleed the session
// resolver guards against with content matching.
func (c *conversationCache) Lookup(accountID, model string, messages []oaiMsg) *cachedConversation {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[c.key(accountID, model)]
	if entry == nil {
		return nil
	}
	if time.Since(entry.LastUsedAt) > c.maxAge {
		delete(c.entries, c.key(accountID, model))
		return nil
	}
	if entry.PrefixHash == "" || len(messages) < entry.MessageCount ||
		messagesHash(messages[:entry.MessageCount]) != entry.PrefixHash {
		// Prefix drifted (client switched, history got compacted, messages were
		// edited). Reusing the cloud conversation would corrupt context, so treat
		// it as a miss and let the caller start a fresh one.
		delete(c.entries, c.key(accountID, model))
		return nil
	}
	return entry
}

func (c *conversationCache) Store(accountID, model string, conv *cachedConversation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv.LastUsedAt = time.Now()
	c.entries[c.key(accountID, model)] = conv
}

func (c *conversationCache) Invalidate(accountID, model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, c.key(accountID, model))
}

func (c *conversationCache) GC() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for k, v := range c.entries {
		if now.Sub(v.LastUsedAt) > c.maxAge {
			delete(c.entries, k)
		}
	}
}

func (c *conversationCache) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"cached_conversations": len(c.entries)}
}

// systemPromptHash hashes every system/developer message. Hashing only the first
// one let a second system message drift undetected: the upstream conversation
// still carried the old system context while the hash claimed the prompt was the
// same, so the cache reused it incorrectly.
func systemPromptHash(messages []oaiMsg) string {
	var parts []string
	for _, m := range messages {
		if m.Role == "system" || m.Role == "developer" {
			if text := strings.TrimSpace(contentToString(m.Content)); text != "" {
				parts = append(parts, text)
			}
		}
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(h[:])
}

func extractLastUserMessage(messages []oaiMsg) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return contentToString(messages[i].Content)
		}
	}
	return ""
}

// messagesHash fingerprints a message sequence the same way the increment gets
// flattened downstream, so a prefix whose actual content changed (different
// client, edited history) produces a different hash and fails the Lookup match.
func messagesHash(messages []oaiMsg) string {
	text, _ := flattenPromptMessages(messages, nil)
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

func (s *Server) storeConvCache(accID, model string, res chathub.Result, tone string, messages []oaiMsg, reused bool) {
	if res.ConversationID == "" {
		return
	}
	cached := s.convCache.Lookup(accID, model, messages)
	entry := &cachedConversation{
		ConversationID: res.ConversationID,
		SessionID:      res.SessionID,
		Tone:           tone,
		MessageCount:   len(messages),
		SystemPrompt:   systemPromptHash(messages),
		PrefixHash:     messagesHash(messages),
	}
	if cached != nil && cached.ConversationID == res.ConversationID {
		entry.TurnCount = cached.TurnCount + 1
	} else {
		entry.TurnCount = 1
	}
	s.convCache.Store(accID, model, entry)
}

func (s *Server) invalidateConvCache(accID, model string) {
	s.convCache.Invalidate(accID, model)
}
