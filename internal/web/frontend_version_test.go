package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrontendVersionDisplayUsesBackendVersion(t *testing.T) {
	pageBytes, err := os.ReadFile(filepath.Join("web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	page := string(pageBytes)
	for _, needle := range []string{
		`id="appVersion">&mdash;</div>`,
		"async function loadAppVersion()",
		"fetch('/api/version'",
		// Only a real version gets the "v" prefix. A non-version answer such as
		// the literal "development" must be shown verbatim instead of being
		// turned into "vdevelopment".
		"el.textContent=/^\\d/.test(v)?'v'+v:v",
		// The badge must also refresh after an in-page login, otherwise the
		// placeholder survives until the next full page load.
		"showPage('dashboard');loadStats();loadAppVersion();",
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("frontend missing %q", needle)
		}
	}
	if strings.Contains(page, `id="appVersion">vdev</div>`) {
		t.Fatal("version badge must not default to a fake dev build")
	}
	if strings.Contains(page, `sidebar-foot">v0.4.0`) {
		t.Fatal("frontend still contains hardcoded v0.4.0")
	}
}
