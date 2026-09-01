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
		`id="appVersion">vdev</div>`,
		"async function loadAppVersion()",
		"fetch('/api/version'",
		"el.textContent=v==='dev'?'vdev':'v'+v",
	} {
		if !strings.Contains(page, needle) {
			t.Fatalf("frontend missing %q", needle)
		}
	}
	if strings.Contains(page, `sidebar-foot">v0.4.0`) {
		t.Fatal("frontend still contains hardcoded v0.4.0")
	}
}
