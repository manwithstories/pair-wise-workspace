package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// WriteTemp writes the encoded index to a temporary file and returns its
// path and SHA-256 without replacing the destination.
func WriteTemp(path string, data []byte) (string, string, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".stage-*")
	if err != nil {
		return "", "", err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", "", err
	}
	sum := sha256.Sum256(data)
	return tmp.Name(), hex.EncodeToString(sum[:]), nil
}

// VerifyTemp reparses a staged index, checks it and confirms its bytes.
func VerifyTemp(tmpPath, wantSHA string, outDir string) error {
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != wantSHA {
		return fmt.Errorf("index: staged hash mismatch")
	}
	idx, err := Decode(data)
	if err != nil {
		return err
	}
	return idx.Validate(outDir)
}

// Commit atomically replaces path with the verified temporary file.
func Commit(tmpPath, path string) error {
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// WriteAtomic performs the full stage, verify and commit sequence.
func WriteAtomic(path string, idx *Index, outDir string) (string, error) {
	data, err := idx.Encode()
	if err != nil {
		return "", err
	}
	tmp, sum, err := WriteTemp(path, data)
	if err != nil {
		return "", err
	}
	if err := VerifyTemp(tmp, sum, outDir); err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := Commit(tmp, path); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return sum, nil
}
