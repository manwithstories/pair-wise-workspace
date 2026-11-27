package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// DigestInput computes a deterministic digest of every regular file in
// root, keyed by slash-separated relative path.
func DigestInput(root string) (string, error) {
	type item struct {
		rel  string
		hash string
	}
	var items []item
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		items = append(items, item{rel: filepath.ToSlash(rel), hash: sha256Hex(data)})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	h := sha256.New()
	for _, it := range items {
		fmt.Fprintf(h, "%s\x00%s\n", it.rel, it.hash)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hexSum returns the hex SHA-256 of data.
func hexSum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
