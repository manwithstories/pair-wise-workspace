package checkpoint

import "os"

// writeFile is a small test helper for creating a corrupt checkpoint file.
func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}
