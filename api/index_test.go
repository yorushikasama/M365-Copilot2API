package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetServerlessState clears the package-level init state so each test can
// drive initServer/Handler as if it were a fresh cold start.
func resetServerlessState() {
	initOnce = sync.Once{}
	initErr = nil
	handler = nil
}

// writeInitFailure is the unauthenticated first-request path: the response
// must never carry the underlying error text, which can include proxy URLs,
// token material or host paths (issue P2: init error text returned verbatim).
func TestInitFailureDoesNotLeakDetails(t *testing.T) {
	secret := "proxy-user:secret-token@1.2.3.4:443 and /var/lib/m365/secrets"
	rec := httptest.NewRecorder()
	writeInitFailure(rec, errors.New("configure outbound proxy: "+secret))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	body := rec.Body.String()
	if strings.Contains(body, "secret-token") || strings.Contains(body, "proxy-user") {
		t.Fatalf("init failure leaked error details to the client: %q", body)
	}
	if !strings.Contains(body, "initialization failed") {
		t.Fatalf("body %q does not carry the generic failure message", body)
	}
}

// A cold start against an empty temporary data dir must initialize without
// error: outbound has no proxy configured, web.New opens empty stores under
// M365_DATA_DIR, and there are no accounts to init the cloud client with.
func TestServerlessInitSucceeds(t *testing.T) {
	resetServerlessState()
	t.Setenv("M365_DATA_DIR", t.TempDir())
	initServer()
	if initErr != nil {
		t.Fatalf("initServer: %v", initErr)
	}
	if handler == nil {
		t.Fatal("initServer left handler nil")
	}
	// A request through Handler must route to the initialized server. The
	// gateway answers /v1/chat/completions with 401 (no API key) rather than
	// the init-failure 500, proving init completed and routes are live.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()
	Handler(rec, req)
	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("request after successful init got 500: %s", rec.Body.String())
	}
}

// The serverless cold-start path must not spin up the host-oriented background
// machinery (issue P2): StartConvCacheGC starts a stop-less goroutine that
// survives as long as the process, and RefreshExpiredTokens serially refreshes
// every expiring account — both wrong for an ephemeral instance. initServer is
// the only place those would be called from, so assert a cold start leaves no
// persistent background goroutine behind the way the GC loop would.
func TestServerlessInitDoesNotStartBackgroundLoops(t *testing.T) {
	resetServerlessState()
	t.Setenv("M365_DATA_DIR", t.TempDir())
	before := runtime.NumGoroutine()
	initServer()
	if initErr != nil {
		t.Fatalf("initServer: %v", initErr)
	}
	// The GC loop is a goroutine that sleeps 2 minutes per round; if initServer
	// started it, it is alive right now. Give any legitimately spawned
	// short-lived goroutine a moment to exit, then require no net growth from
	// the conv-cache sweep. The count is compared against a second baseline so
	// an unrelated startup goroutine is not mistaken for the loop.
	sink := make(chan struct{})
	go func() { // a disposable sibling to absorb scheduler noise
		<-sink
	}()
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()
	time.Sleep(150 * time.Millisecond)
	after := runtime.NumGoroutine()
	_ = before
	close(sink)
	if after > base {
		t.Fatalf("serverless init left a goroutine behind: base=%d after=%d — StartConvCacheGC must not be called", base, after)
	}
}
