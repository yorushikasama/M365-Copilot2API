// Vercel Go Function entry point. Deploys the full gateway as a single
// serverless function: every request (any path) is routed to Handler.
//
// Deploy layout:
//
//	api/index.go      (this file)
//	vercel.json       (rewrite all paths here, set maxDuration)
//
// Limitations on Vercel's free tier: there is no persistent disk, so
// M365_DATA_DIR defaults to /tmp and account tokens do not survive cold
// starts. Suitable for trials; for production use a host with a volume.
package main

import (
	"log"
	"m365-copilot2api/internal/outbound"
	"m365-copilot2api/internal/web"
	"net/http"
	"os"
	"sync"
)

var (
	initOnce sync.Once
	handler  http.Handler
	initErr  error
)

func initServer() {
	if os.Getenv("M365_DATA_DIR") == "" {
		_ = os.Setenv("M365_DATA_DIR", "/tmp/m365-copilot2api")
	}
	web.ApplyStartupSettingsEnv()
	if err := outbound.ConfigureFromEnv(); err != nil {
		initErr = err
		return
	}
	s, e := web.New()
	if e != nil {
		initErr = e
		return
	}
	s.InitM365CloudClient()
	// StartAutoCleanup, StartCooldownProber, StartConvCacheGC, PreheatPool and
	// RefreshExpiredTokens are host-oriented startup work, skipped here: the
	// ephemeral instance has no persistent accounts (storage is /tmp), so a
	// serial refresh of every expiring account at first request would block the
	// cold start for nothing, and a stop-less background sweep is a goroutine
	// an instance that is torn down at any moment should not own. Accounts
	// refresh on demand via EnsureValid as requests arrive.
	handler = s.Routes()
	log.Println("m365-copilot2api serverless instance ready")
}

// writeInitFailure replies to a request that arrived while serverless
// initialization failed. The full error is for operators and goes only to the
// log; the HTTP body stays generic because the very first request can trigger
// initialization and is not yet authenticated, so initErr text (proxy URLs,
// token material, paths) must not be echoed back.
func writeInitFailure(w http.ResponseWriter, err error) {
	log.Printf("serverless init failed: %v", err)
	http.Error(w, "gateway initialization failed", http.StatusInternalServerError)
}

// Handler is invoked by the Vercel Go runtime for each request.
func Handler(w http.ResponseWriter, r *http.Request) {
	initOnce.Do(initServer)
	if initErr != nil {
		writeInitFailure(w, initErr)
		return
	}
	handler.ServeHTTP(w, r)
}

// main exists only so `go build ./...` accepts this package as a valid main
// package outside of Vercel's build pipeline, which wraps Handler itself.
func main() {}
