package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"os"
)

func newSHA() hash.Hash { return sha256.New() }

func sumHex(h hash.Hash) string { return hex.EncodeToString(h.Sum(nil)) }

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
