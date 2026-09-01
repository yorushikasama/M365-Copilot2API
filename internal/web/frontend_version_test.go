package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFrontendVersionDisplayUsesBackendVersion(t *testing.T) {
	rootPage := filepath.Join("..", "..", "web", "index.html")
	embeddedPage := filepath.Join("web", "index.html")
	root, err := os.ReadFile(rootPage)
	if err != nil {
		t.Fatal(err)
	}
	embedded, err := os.ReadFile(embeddedPage)
	if err != nil {
		t.Fatal(err)
	}
	if string(root) != string(embedded) {
		t.Fatal("frontend source and embedded copy differ")
	}
	page := string(root)
	for _, needle := range []string{
		`id="appVersion">&mdash;</div>`,
		"async function loadAppVersion()",
		"fetch('/api/version'",
		"el.textContent=v==='dev'?'vdev':'v'+v",
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
