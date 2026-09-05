// Package atomicfile provides the durable file-replacement primitive shared by
// every on-disk store in this project (accounts, token cache, settings, keys,
// sessions, usage, ...).
//
// It exists because the same routine had been copy-pasted into internal/web,
// internal/auth and internal/config, and the copies had already drifted: the
// internal/config one never swept stale temporary files, so a crash mid-write
// left ".accounts.json.tmp.*" droppings behind forever.
package atomicfile

import (
	"os"
	"path/filepath"
)

// FsyncDir flushes a directory entry so a preceding rename is durable across a
// power loss. Renaming is atomic, but the *name* only survives a crash once the
// parent directory itself has been synced.
func FsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// CleanupStaleTmp removes temporary files left by an interrupted Write. Both the
// dotted and undotted prefixes are swept because earlier revisions used each.
func CleanupStaleTmp(path string) {
	if path == "" {
		return
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	for _, pat := range []string{filepath.Join(dir, "."+base+".tmp.*"), filepath.Join(dir, base+".tmp.*")} {
		if matches, _ := filepath.Glob(pat); matches != nil {
			for _, m := range matches {
				_ = os.Remove(m)
			}
		}
	}
	_ = os.Remove(path + ".tmp")
}

// Write persists b to path durably: temp file -> chmod -> write -> fsync(file)
// -> close -> rename -> fsync(dir). A reader therefore only ever observes the
// complete old contents or the complete new contents, never a partial write.
//
// An empty path is a no-op so callers with an optional destination need no guard.
func Write(path string, b []byte, perm os.FileMode) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	CleanupStaleTmp(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Runs on every path: harmless after a successful rename (the name is gone),
	// and the only thing that stops a failed write from leaking the temp file.
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	_ = FsyncDir(dir)
	return nil
}
