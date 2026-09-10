package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveContentKeyedSameIdentity(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	// 首次请求绑定云端对话，同一 IP/UA 但不同 user 账户。
	sr.Bind("", "conv-shared", "acc1",
		&oaiReq{User: "alice", Messages: []oaiMsg{{Role: "user", Content: "hello"}, {Role: "assistant", Content: "你好"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 续接请求来自同一 IP/UA（换 user 仍可命中，说明不做 user 拦截）。
	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "bob"),
		&oaiReq{
			User: "bob",
			Messages: []oaiMsg{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "你好"},
				{Role: "user", Content: "多说点"},
			},
		})
	if res.IsNew {
		t.Fatal("同 IP/UA 前缀相同却未复用会话，内容键失效")
	}
	if res.MatchedBy != "context_prefix_2" {
		t.Fatalf("expected context_prefix_2, got %q", res.MatchedBy)
	}
	if res.ConversationID != "conv-shared" {
		t.Fatalf("expected conversation conv-shared, got %s", res.ConversationID)
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 (增量起点), got %d", res.HistoryLen)
	}
}

func TestResolveDoesNotMatchAcrossIdentity(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	sr.Bind("", "conv-a", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 不同 IP / UA 的用户输入同样的短消息，不应串到别人的会话。
	res := sr.Resolve(resolverTestRequest("198.51.100.99", "client-b", "bob"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("跨 IP/UA 的内容必须新建会话，got matched=%s conv=%s", res.MatchedBy, res.ConversationID)
	}
}

func TestResolveSingleMessageReusesForSameUser(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("sess-short", "conv-short", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "alice"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if res.IsNew {
		t.Fatalf("same user re-sending a message should reuse session, got IsNew=true")
	}
}

func TestResolveSingleMessageNeverReusesAcrossUsers(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()

	sr.Bind("sess-short", "conv-short", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	res := sr.Resolve(resolverTestRequest("203.0.113.20", "client-b", "bob"),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "继续"}}})
	if !res.IsNew {
		t.Fatalf("different user must not reuse session, got matched=%s", res.MatchedBy)
	}
}

func resolverTestRequest(ip, ua, user string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = ip + ":12345"
	r.Header.Set("User-Agent", ua)
	return r
}

// TestResolveReturnsBoundAccountPinsRoutingHint: when a conversation is matched
// by the resolver, its bound account is returned as a routing hint. openaiChat
// copies it into body.AccountID via firstNonEmpty; the account is NOT
// client-pinned and must remain failoverable when it gets throttled. This test
// locks the premise so removing the account hint (or pinning it) is a visible
// regression.
func TestResolveReturnsBoundAccount(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	sr.Bind("", "conv-jayz", "f205e3da-b084-438f-917f-2cc2ae057052",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
		}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	res := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
			{Role: "user", Content: "第二轮问题"},
		}})
	if res.IsNew {
		t.Fatal("incremental request should reuse the bound conversation")
	}
	if res.AccountID != "f205e3da-b084-438f-917f-2cc2ae057052" {
		t.Fatalf("expected resolver to return the bound account, got %q", res.AccountID)
	}

	// Mirror openaiChat's merge: a request without its own accountId takes the
	// resolved account as a routing hint, not as a client pin.
	body := &oaiReq{AccountID: ""}
	if got := firstNonEmpty(body.AccountID, res.AccountID); got != "f205e3da-b084-438f-917f-2cc2ae057052" {
		t.Fatalf("firstNonEmpty must adopt the resolved account, got %q", got)
	}
	clientPinned := body.AccountID != ""
	if clientPinned {
		t.Fatal("resolved account must not be treated as client-pinned")
	}
}

func TestResolverIncrementalBoundary(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	sr.Bind("", "conv-inc", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
		}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// 第二轮只应发送历史之外的新增消息。
	res := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "第一轮问题"},
			{Role: "assistant", Content: "第一轮回答"},
			{Role: "user", Content: "第二轮问题"},
		}})
	if res.IsNew {
		t.Fatal("增量请求应复用以 2 轮历史为前缀的会话")
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2, got %d", res.HistoryLen)
	}

	// 内容不再是前一轮任何历史的前缀时不应误命中。
	res2 := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "全新问题完全无关"}}})
	if !res2.IsNew {
		t.Fatalf("不相关内容必须新建会话, got %s conv=%s", res2.MatchedBy, res2.ConversationID)
	}
}

func TestResolverEvictsAfterTTL(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	sr := openSessionResolver()
	sr.Bind("sess-old", "conv-old", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "旧问题"}}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	// 把会话标记为超过默认 2h 闲置。
	sr.mu.Lock()
	old := sr.sessions["sess-old"]
	old.LastUsedAt = time.Now().UTC().Add(-3 * time.Hour)
	sr.sessions["sess-old"] = old
	sr.mu.Unlock()

	res := sr.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "旧问题"}}})
	if !res.IsNew {
		t.Fatalf("闲置超 TTL 的会话应失效，got matched=%s", res.MatchedBy)
	}
}

func TestResolverPersistsHistoryAcrossReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	t.Setenv("M365_SESSION_CACHE", path)

	sr1 := openSessionResolver()
	sr1.Bind("", "conv-persist", "acc1",
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "persisted question"},
			{Role: "assistant", Content: "persisted answer"},
		}},
		"",
		httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if err := sr1.persist.flushNowBlocking(); err != nil {
		t.Fatal(err)
	}

	// 模拟重启：重新打开同一缓存文件，历史仍在 → 前缀仍可命中。
	sr2 := openSessionResolver()
	res := sr2.Resolve(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "persisted question"},
			{Role: "assistant", Content: "persisted answer"},
			{Role: "user", Content: "follow-up"},
		}})
	if res.IsNew {
		t.Fatal("contextHistory 应持久化，重启后仍可内容复用")
	}
	if res.ConversationID != "conv-persist" {
		t.Fatalf("unexpected conversation %s", res.ConversationID)
	}
	if res.HistoryLen != 2 {
		t.Fatalf("expected HistoryLen=2 after reload, got %d", res.HistoryLen)
	}
}

func TestAutoCleanupDefaultMaxAgeTwoHours(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("M365_SESSION_CACHE", filepath.Join(dir, "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(dir, "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(dir, "users.json"))
	s := newTestServerForAutoCleanup(t)
	s.conversationManager.Record("conv-old", "acc1", "old")
	s.conversationManager.mu.Lock()
	old := s.conversationManager.data["conv-old"]
	old.LastUsedAt = time.Now().UTC().Add(-3 * time.Hour)
	s.conversationManager.data["conv-old"] = old
	s.conversationManager.mu.Unlock()

	active := s.activeConversationSet(2 * time.Hour)
	if active["conv-old"] {
		t.Error("3h 闲置的会话不应在 2h 保护窗口内")
	}
}

func TestInvalidateMatchingDropsStaleBinding(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	// 云端对话 conv-A 已包含 Q1+A1（成功轮绑定）。
	sr.Bind("", "conv-A", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "user", Content: "Q1"}, {Role: "assistant", Content: "A1"}}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 新请求 Q2 在失败前已派发到 conv-A：绑定必须失效，
	// 否则下一轮会按旧前缀只发增量，云端顶部的 Q2 被跳过，
	// 模型回答上一个问题（2026-09-10 事故）。
	sr.InvalidateMatching(resolverTestRequest("203.0.113.10", "client-a", "alice"),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "Q1"},
			{Role: "assistant", Content: "A1"},
			{Role: "user", Content: "Q2"},
		}})

	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "alice"),
		&oaiReq{Messages: []oaiMsg{
			{Role: "user", Content: "Q1"},
			{Role: "assistant", Content: "A1"},
			{Role: "user", Content: "Q2"},
		}})
	if !res.IsNew {
		t.Fatalf("失效后仍复用旧会话 conv=%s matched=%s，增量会跳过失败轮的问题", res.ConversationID, res.MatchedBy)
	}
}

func TestMatchContextIgnoresSystemOnlyPrefix(t *testing.T) {
	t.Setenv("M365_SESSION_CACHE", filepath.Join(t.TempDir(), "sessions.json"))
	t.Setenv("M365_CONVERSATION_CACHE", filepath.Join(t.TempDir(), "conversations.json"))
	t.Setenv("M365_USER_SESSION_CACHE", filepath.Join(t.TempDir(), "users.json"))
	sr := openSessionResolver()

	// 某会话的历史只有 system（异常绑定）。
	sr.Bind("", "conv-sys", "acc1",
		&oaiReq{Messages: []oaiMsg{{Role: "system", Content: "sys"}}, Metadata: &oaiMetadata{CopilotTempSession: false}},
		"",
		resolverTestRequest("203.0.113.10", "client-a", "alice"))

	// 另一个全新对话共享同一段 system：不得仅凭 system 前缀绑到 conv-sys。
	res := sr.Resolve(resolverTestRequest("203.0.113.10", "client-a", "alice"),
		&oaiReq{Messages: []oaiMsg{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "全新问题"},
		}})
	if !res.IsNew {
		t.Fatalf("system-only 前缀不应匹配，却复用了 conv=%s", res.ConversationID)
	}
}
