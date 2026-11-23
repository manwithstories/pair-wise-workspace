package checkpoint

import "os"

// syncDir flushes a directory entry so an atomic rename is durable. On systems
// where opening a directory for sync is unsupported it is a best-effort no-op.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
