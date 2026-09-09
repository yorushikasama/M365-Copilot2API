package mcp

import "testing"

// The registry used to grow without bound via MergeTools: tools declared by
// one client's request stayed registered forever and leaked into every later
// listing. ReplaceTools makes the registry a bounded snapshot of the most
// recent tool-bearing request.
func TestReplaceToolsMirrorsLatestRequest(t *testing.T) {
	defer GlobalToolRegistry.ClearTools()
	GlobalToolRegistry.ClearTools()

	GlobalToolRegistry.ReplaceTools([]Tool{{Name: "read_file"}, {Name: "write_file"}})
	if got := GlobalToolRegistry.ListTools(); len(got) != 2 {
		t.Fatalf("expected 2 tools after first replace, got %d", len(got))
	}

	GlobalToolRegistry.ReplaceTools([]Tool{{Name: "web_search"}})
	got := GlobalToolRegistry.ListTools()
	if len(got) != 1 || got[0].Name != "web_search" {
		t.Fatalf("stale tools leaked across requests: %+v", got)
	}

	GlobalToolRegistry.ReplaceTools(nil)
	if got := GlobalToolRegistry.ListTools(); len(got) != 0 {
		t.Fatalf("expected empty registry after clearing replace, got %d", len(got))
	}
}

// Concurrency smoke test: ReplaceTools and ListTools run under a lock, so
// racing them must not panic or tear.
func TestReplaceToolsConcurrentAccess(t *testing.T) {
	defer GlobalToolRegistry.ClearTools()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			GlobalToolRegistry.ReplaceTools([]Tool{{Name: "t1"}})
			_ = GlobalToolRegistry.ListTools()
		}
	}()
	<-done
}
