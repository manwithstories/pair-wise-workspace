package pipeline

import (
	"os"
	"path/filepath"

	"relay/internal/safepath"
	"relay/internal/txn"
)

// writeFileMkdir creates parent directories inside the work root and writes
// data with restrictive permissions.
func writeFileMkdir(absPath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(absPath, data, 0o600)
}

// readCommittedManifest loads the manifest published under committed/<id8>.
func readCommittedManifest(root *safepath.Root, commitID [32]byte) (txn.Manifest, bool) {
	p, err := root.Join("committed", hex8(commitID), "manifest.bin")
	if err != nil {
		return txn.Manifest{}, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return txn.Manifest{}, false
	}
	m, err := txn.DecodeManifest(data)
	if err != nil {
		return txn.Manifest{}, false
	}
	return m, true
}

func hex8(id [32]byte) string {
	const hexd = "0123456789abcdef"
	b := make([]byte, 16)
	for i := 0; i < 8; i++ {
		b[i*2] = hexd[id[i]>>4]
		b[i*2+1] = hexd[id[i]&0x0f]
	}
	return string(b)
}
