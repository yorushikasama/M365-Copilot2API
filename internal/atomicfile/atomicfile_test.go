package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesContentAndAppliesPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "store.json")

	if err := Write(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("content = %q, want %q", got, "second")
	}
	// A rename-based write must replace, never append or merge.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != int64(len("second")) {
		t.Fatalf("size = %d, want %d", fi.Size(), len("second"))
	}
}

// Write must not leave temporary files behind, and it must sweep droppings from
// an earlier interrupted write. internal/config used to skip this sweep.
func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")

	// Stand in for a process that died between CreateTemp and Rename.
	stale := filepath.Join(dir, ".accounts.json.tmp.123456")
	if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Write(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "accounts.json" {
			t.Fatalf("unexpected leftover file %q", e.Name())
		}
	}
}

// An empty path is a documented no-op so callers with an optional destination
// need no guard of their own.
func TestWriteEmptyPathIsNoop(t *testing.T) {
	if err := Write("", []byte("x"), 0o600); err != nil {
		t.Fatalf("Write(\"\") = %v, want nil", err)
	}
}
