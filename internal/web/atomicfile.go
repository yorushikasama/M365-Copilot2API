package web

import (
	"os"

	"m365-copilot2api/internal/atomicfile"
)

// writeFileAtomic keeps this package's call sites unqualified. The durable
// write itself lives in internal/atomicfile, which is the single copy shared
// with internal/auth and internal/config.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	return atomicfile.Write(path, b, perm)
}
