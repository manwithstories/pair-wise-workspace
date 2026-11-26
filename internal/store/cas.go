package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"pairflow/internal/ingest"
	"pairflow/internal/model"
)

// CacheKeyInputs are all provenance dimensions that must invalidate reuse.
type CacheKeyInputs struct {
	AlgorithmVersion  string `json:"algorithm_version"`
	SourceFingerprint string `json:"source_fingerprint"`
	InputDigest       string `json:"input_digest"`
	NormalizedMeta    string `json:"normalized_metadata"`
	Stage             string `json:"stage"`
}

// CacheKey returns a stable 64-hex-character content cache key.
func CacheKey(in CacheKeyInputs) string {
	if in.AlgorithmVersion == "" {
		in.AlgorithmVersion = model.AlgorithmVersion
	}
	b, _ := json.Marshal(in)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Entry is the persisted cache envelope.
type Entry struct {
	Key               string         `json:"key"`
	Inputs            CacheKeyInputs `json:"inputs"`
	SourceFingerprint string         `json:"source_fingerprint"`
	InputDigest       string         `json:"input_digest"`
	OutputDigest      string         `json:"output_digest"`
	OutputPath        string         `json:"output_path"`
}

// CAS is a tiny content-addressed cache rooted on the local filesystem.
type CAS struct {
	root string
}

// NewCAS creates the cache directory.
func NewCAS(root string) (*CAS, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &CAS{root: root}, nil
}

func (c *CAS) entryPath(key string) string {
	return filepath.Join(c.root, key[:2], key+".json")
}

func (c *CAS) blobPath(digest string) string {
	return filepath.Join(c.root, "blobs", digest[:2], digest+".bin")
}

// PutBlob persists bytes by SHA-256 and returns their digest.
func (c *CAS) PutBlob(data []byte) (string, error) {
	digest := ingest.Sum256(data)
	path := c.blobPath(digest)
	if _, err := os.Stat(path); err == nil {
		return digest, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := atomicWrite(path, data); err != nil {
		return "", err
	}
	return digest, nil
}

// Put records a cache entry after its output blob is durable.
func (c *CAS) Put(e Entry) error {
	if e.Key == "" {
		e.Key = CacheKey(e.Inputs)
	}
	path := c.entryPath(e.Key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, b)
}

// GetRevalidated loads a cache entry and verifies source, input and output.
// Any mismatch is classified and the untrusted entry is not returned as usable.
func (c *CAS) GetRevalidated(key, sourceFingerprint, inputDigest string) (*Entry, []byte, *model.MediaError) {
	path := c.entryPath(key)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, &model.MediaError{Code: model.CodeCacheMismatch, Message: err.Error()}
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, nil, &model.MediaError{Code: model.CodeCacheMismatch, Message: "corrupt cache entry: " + err.Error()}
	}
	if e.Key != key || e.SourceFingerprint != sourceFingerprint || e.InputDigest != inputDigest ||
		e.Inputs.SourceFingerprint != sourceFingerprint || e.Inputs.InputDigest != inputDigest {
		return nil, nil, &model.MediaError{Code: model.CodeCacheMismatch, ExpectedDigest: inputDigest, ActualDigest: e.InputDigest, Message: "cache provenance mismatch"}
	}
	blob, err := os.ReadFile(c.blobPath(e.OutputDigest))
	if err != nil {
		return nil, nil, &model.MediaError{Code: model.CodeCacheMismatch, Message: "cached output missing: " + err.Error()}
	}
	if got := ingest.Sum256(blob); got != e.OutputDigest {
		return nil, nil, &model.MediaError{Code: model.CodeCacheMismatch, ExpectedDigest: e.OutputDigest, ActualDigest: got, Message: "cached output digest mismatch"}
	}
	return &e, blob, nil
}

// VerifySourceDigest re-hashes current source bytes before cache reuse.
func VerifySourceDigest(expected string, source []byte) *model.MediaError {
	actual := ingest.Sum256(source)
	if actual == expected {
		return nil
	}
	return &model.MediaError{Code: model.CodeCacheMismatch, ExpectedDigest: expected, ActualDigest: actual, Message: "source changed since cache entry"}
}

var _ = fmt.Sprintf
