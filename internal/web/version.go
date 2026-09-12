package web

import (
	_ "embed"
	"m365-copilot2api/internal/outbound"
	"net/http"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
	startedAt = time.Now()
)

// versionFile is compiled into the binary, so the release the source belongs to
// survives every build path — including the ones that hand the toolchain no git
// metadata and no -ldflags. That combination is what made a deployed build
// report the literal string "development" (rendered as "vdevelopment"): with no
// module version and no vcs.revision there was simply nothing left to read.
//
//go:embed VERSION
var versionFile string

// sourceVersion is the VERSION file's value, normalised to the form the badge
// wants (no "v", no surrounding whitespace).
func sourceVersion() string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(versionFile), "v"))
}

// releaseVersion is what a version badge should read: plain X.Y.Z, no "v" and
// no toolchain provenance.
var releaseVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// pseudoVersion matches the version the go command stamps into a binary built
// straight from a checkout that never went through -ldflags, e.g.
// "v0.6.7-0.20260910124631-828e899df8af": the newest reachable tag was v0.6.6,
// so the commit is heading for v0.6.7. That leading number is the one a reader
// wants. The 14-digit timestamp and 12-hex revision are provenance, and module
// metadata such as "+dirty" is not part of a version at all — leaking either
// into the badge is what made a running build read as
// "v0.6.7-0.20260910124631-828e899df8af+dirty".
var pseudoVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+-[0-9A-Za-z.\-]*?0\.\d{14}-[0-9a-f]{12}$`)

// releaseFromModuleVersion reduces what the toolchain recorded for the main
// module to the release it represents. It returns "" when there is no usable
// number: an unversioned build, or the "v0.0.0-<time>-<sha>" form a checkout
// with no reachable tag produces.
func releaseFromModuleVersion(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" || v == "(devel)" {
		return ""
	}
	if i := strings.IndexByte(v, '+'); i >= 0 { // +dirty / +incompatible
		v = v[:i]
	}
	switch {
	case pseudoVersion.MatchString(v):
		v = v[:strings.IndexByte(v, '-')]
	case releaseVersion.MatchString(strings.TrimPrefix(v, "v")):
		// An exact release tag is already the version.
	default:
		return ""
	}
	v = strings.TrimPrefix(v, "v")
	if !releaseVersion.MatchString(v) || v == "0.0.0" {
		return ""
	}
	return v
}

// buildProvenance reports the revision and dirty flag the toolchain recorded in
// the binary itself, so a build that was never stamped with ldflags can still be
// told apart from its neighbours.
func buildProvenance(info *debug.BuildInfo) (revision string, modified bool) {
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = strings.TrimSpace(setting.Value)
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return revision, modified
}

// effectiveVersion answers one question — which release is this? — and it is
// never allowed to answer with something that is not a version. The chain, in
// order of authority:
//
//  1. Version, stamped by -ldflags on release builds (the tag, e.g. "0.6.7").
//  2. The module version the toolchain recorded, normalised: an exact tag, or
//     the release a pseudo-version is heading for ("v0.6.7-0.2026…" -> "0.6.7").
//  3. The VERSION file compiled into the binary, which is always present because
//     it ships with the source.
//  4. A revision marker, only if the binary carries one.
//
// Every step above can be missing on its own — a build from a source copy with
// no .git and no -ldflags defeats 1 and 2 — but step 3 cannot go missing, which
// is what makes the badge correct on every build path instead of only on the
// ones the release workflow happens to use.
func effectiveVersion() string {
	if version := strings.TrimSpace(Version); version != "" && version != "dev" {
		return version
	}

	var info *debug.BuildInfo
	if bi, ok := debug.ReadBuildInfo(); ok {
		info = bi
		if release := releaseFromModuleVersion(bi.Main.Version); release != "" {
			return release
		}
	}

	if source := sourceVersion(); source != "" {
		return source
	}

	if info != nil {
		if revision, modified := buildProvenance(info); revision != "" {
			version := "dev-" + revision
			if modified {
				version += "-dirty"
			}
			return version
		}
	}
	return "dev"
}

// effectiveCommit mirrors effectiveVersion: release builds carry the sha from
// ldflags, while unstamped builds fall back to the revision the toolchain
// recorded, so /api/version can still identify a deployment. The dirty marker
// belongs here rather than in the version string.
func effectiveCommit() string {
	if commit := strings.TrimSpace(Commit); commit != "" && commit != "unknown" {
		return commit
	}
	unset := strings.TrimSpace(Commit)
	if unset == "" {
		unset = "unknown"
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return unset
	}
	revision, modified := buildProvenance(info)
	if revision == "" {
		return unset
	}
	if modified {
		return revision + "-dirty"
	}
	return revision
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	jsonOut(w, map[string]any{"version": effectiveVersion(), "commit": effectiveCommit(), "buildTime": BuildTime, "go": runtime.Version(), "uptimeSeconds": int(time.Since(startedAt).Seconds()), "accounts": len(s.tokens.List()), "proxyPool": len(outbound.ProxyPoolStatus())})
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
