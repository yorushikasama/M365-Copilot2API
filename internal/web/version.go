package web

import (
	"fmt"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

var (
	Version     = "dev"
	Commit      = "unknown"
	BuildTime   = "unknown"
	startedAt   = time.Now()
	updateCheck uint32
)

func effectiveVersion() string {
	if version := strings.TrimSpace(Version); version != "" && version != "dev" {
		return version
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "development"
	}
	if version := strings.TrimSpace(info.Main.Version); version != "" && version != "(devel)" {
		return strings.TrimPrefix(version, "v")
	}

	var revision string
	modified := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if revision == "" {
		return "development"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	version := "dev-" + revision
	if modified {
		version += "-dirty"
	}
	return version
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	jsonOut(w, map[string]any{"version": effectiveVersion(), "commit": Commit, "buildTime": BuildTime, "go": runtime.Version(), "uptimeSeconds": int(time.Since(startedAt).Seconds()), "accounts": len(s.tokens.List()), "proxyPool": len(outbound.ProxyPoolStatus())})
}

func (s *Server) update(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	// Read-only endpoint: release automation remains the only publisher/upgrader.
	stable := strings.TrimSpace(Version) != "" && Version != "dev"
	jsonOut(w, map[string]any{"current": effectiveVersion(), "channel": map[bool]string{true: "stable", false: "development"}[stable], "updateAvailable": false, "recommendUpdate": false, "message": map[bool]string{true: "当前为稳定版，可检查稳定版更新", false: "当前为开发版，不推荐更新"}[stable]})
}

func ReleaseTag() string {
	v := strings.TrimSpace(Version)
	if v == "" || v == "dev" {
		return ""
	}
	return fmt.Sprintf("v%s", v)
}
