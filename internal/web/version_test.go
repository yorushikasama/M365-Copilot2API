package web

import (
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// semverPrefix matches anything that reads as a version rather than a word.
var semverPrefix = regexp.MustCompile(`^\d+\.\d+\.\d+`)

// The inputs mirror what the toolchain actually records for a binary built from
// a checkout that never went through -ldflags:
//
//	$ go version -m m365-copilot2api-linux-amd64
//	mod m365-copilot2api v0.6.7-0.20260910124631-828e899df8af+dirty
//
// That whole string is what the version badge used to show. The number a reader
// wants is the release the commit is heading for — v0.6.7.
func TestReleaseFromModuleVersion(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"live 2026-09-11 stamp", "v0.6.7-0.20260910124631-828e899df8af+dirty", "0.6.7"},
		{"pseudo version", "v0.6.7-0.20260910124631-828e899df8af", "0.6.7"},
		{"pseudo version past a prerelease", "v0.6.7-rc1.0.20260910124631-828e899df8af", "0.6.7"},
		{"pseudo version without v prefix", "0.6.7-0.20260910124631-828e899df8af", "0.6.7"},
		{"exact release tag", "v0.6.6", "0.6.6"},
		{"exact release tag without v prefix", "0.6.6", "0.6.6"},
		{"module metadata", "v1.2.3+incompatible", "1.2.3"},
		// No reachable tag means no release number to report; the caller falls
		// back to the revision instead of claiming v0.0.0.
		{"untagged checkout", "v0.0.0-20260910124631-828e899df8af", ""},
		{"no tags at all", "v0.0.0", ""},
		{"devel", "(devel)", ""},
		{"empty", "", ""},
		{"blank", "   ", ""},
		{"not a version", "development", ""},
	}
	for _, c := range cases {
		if got := releaseFromModuleVersion(c.in); got != c.want {
			t.Errorf("%s: releaseFromModuleVersion(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

// A release build is stamped through ldflags; that value must win and pass
// through untouched.
func TestEffectiveVersionPrefersInjectedVersion(t *testing.T) {
	restore := Version
	defer func() { Version = restore }()
	Version = "0.6.7"
	if got := effectiveVersion(); got != "0.6.7" {
		t.Fatalf("effectiveVersion() = %q, want 0.6.7", got)
	}
}

// Whatever the binary carries, the badge must not render toolchain provenance.
func TestEffectiveVersionNeverLeaksProvenance(t *testing.T) {
	original := Version
	defer func() { Version = original }()
	Version = "dev"

	got := effectiveVersion()
	if strings.Contains(got, "+dirty") || strings.Contains(got, "-0.20") {
		t.Fatalf("effectiveVersion() leaked build provenance: %q", got)
	}
	if got == "" {
		t.Fatal("effectiveVersion() must always answer with something")
	}
}

func TestBuildProvenance(t *testing.T) {
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{
		{Key: "vcs.revision", Value: "828e899df8af35dc5e9a3af382a32ce97386446a"},
		{Key: "vcs.modified", Value: "true"},
	}}
	rev, modified := buildProvenance(info)
	if rev != "828e899df8af" {
		t.Fatalf("revision = %q, want the first 12 chars of the sha", rev)
	}
	if !modified {
		t.Fatal("vcs.modified=true must be reported")
	}

	if rev, modified := buildProvenance(&debug.BuildInfo{}); rev != "" || modified {
		t.Fatalf("build info without vcs settings must report nothing, got %q/%v", rev, modified)
	}
}

// The dirty marker moved out of the version string but must not be lost — it is
// what tells two unstamped builds apart.
func TestEffectiveCommit(t *testing.T) {
	restore := Commit
	defer func() { Commit = restore }()

	Commit = "abc1234"
	if got := effectiveCommit(); got != "abc1234" {
		t.Fatalf("effectiveCommit() = %q, want the injected commit", got)
	}
	Commit = ""
	fallback := effectiveCommit()
	if fallback == "" {
		t.Fatal("effectiveCommit() must always answer with something")
	}
	if strings.Contains(fallback, "+") {
		t.Fatalf("effectiveCommit() must not carry module metadata: %q", fallback)
	}
}

// Live incident, 2026-09-11 ~10:11: the deployed binary carried no module
// version ("(devel)") and no vcs settings at all, so the old fallback answered
// the literal string "development" and the badge rendered it as "vdevelopment".
// The VERSION file is compiled into the binary, so it cannot be missing the way
// git metadata and ldflags can.
func TestSourceVersionIsARelease(t *testing.T) {
	got := sourceVersion()
	if !releaseVersion.MatchString(got) {
		t.Fatalf("internal/web/VERSION holds %q; it must be a plain X.Y.Z release", got)
	}
}

func TestEffectiveVersionAlwaysAnswersWithAVersion(t *testing.T) {
	original := Version
	defer func() { Version = original }()
	Version = "dev"

	got := effectiveVersion()
	if got == "development" || got == "" {
		t.Fatalf("effectiveVersion() = %q; the compiled-in VERSION file must keep a number available on every build path", got)
	}
	if !semverPrefix.MatchString(got) {
		t.Fatalf("effectiveVersion() = %q, which does not read as a version", got)
	}
}
