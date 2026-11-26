// Package checkpoint persists record chunks before atomically committing a
// local JSON index, and restores only chunks whose digests verify.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/relayteam/relay/internal/media"
	"github.com/relayteam/relay/internal/verify"
)

const (
	indexRelPath = "checkpoint/index.json"
	chunkDir     = "chunks"
	indexVersion = 1
)

// Config is the recovery-relevant subset of run configuration.
type Config struct {
	InputName        string `json:"input_name"`
	ChunkRecords     int    `json:"chunk_records"`
	MaxInflightBytes int64  `json:"max_inflight_bytes"`
	SegmentTicks90k  int64  `json:"segment_ticks90k"`
	SlowEvery        int    `json:"slow_every"`
	PayloadBytes     int    `json:"payload_bytes"`
	RenditionCount   int    `json:"rendition_count"`
	TrackCount       int    `json:"track_count"`
	RecordsPerStream int    `json:"records_per_stream"`
}

// Chunk is one committed NDJSON chunk and its digests.
type Chunk struct {
	Index       int    `json:"index"`
	File        string `json:"file"`
	RecordCount int    `json:"record_count"`
	FirstRecord int    `json:"first_record"`
	LastRecord  int    `json:"last_record"`
	ByteCount   int64  `json:"byte_count"`
	SHA256      string `json:"sha256"`
	CRC32       uint32 `json:"crc32"`
}

// Index is the atomically replaced checkpoint manifest.
type Index struct {
	Version           int     `json:"version"`
	InputFingerprint  string  `json:"input_fingerprint"`
	ConfigFingerprint string  `json:"config_fingerprint"`
	Config            Config  `json:"config"`
	NextRecordNo      int     `json:"next_record_no"`
	Chunks            []Chunk `json:"chunks"`
	Completed         bool    `json:"completed"`
	OutputSHA256      string  `json:"output_sha256,omitempty"`
}

// Manager owns one root directory's chunk and index lifecycle.
type Manager struct {
	root  string
	Index *Index
}

// FingerprintConfig returns the stable SHA-256 of recovery configuration.
func FingerprintConfig(cfg Config) (string, error) {
	b, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return verify.SHA256Bytes(b), nil
}

// Load reads an index. A missing checkpoint returns nil without error.
func Load(root string) (*Index, error) {
	path := filepath.Join(root, indexRelPath)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, media.E(media.ErrCheckpoint, "checkpoint.load", media.Meta{}, err)
	}
	var idx Index
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, media.E(media.ErrCheckpoint, "checkpoint.load", media.Meta{}, err)
	}
	return &idx, nil
}

// NewManager loads an existing checkpoint or creates an in-memory one.
func NewManager(root, inputFingerprint string, cfg Config) (*Manager, error) {
	cfgFP, err := FingerprintConfig(cfg)
	if err != nil {
		return nil, media.E(media.ErrCheckpoint, "checkpoint.config", media.Meta{}, err)
	}
	idx, err := Load(root)
	if err != nil {
		return nil, err
	}
	if idx == nil {
		idx = &Index{
			Version: indexVersion, InputFingerprint: inputFingerprint,
			ConfigFingerprint: cfgFP, Config: cfg, NextRecordNo: 1,
		}
		return &Manager{root: root, Index: idx}, nil
	}
	if idx.InputFingerprint != inputFingerprint {
		return nil, media.E(media.ErrInputFingerprint, "checkpoint.restore", media.Meta{},
			fmt.Errorf("checkpoint input %s != current %s", idx.InputFingerprint, inputFingerprint))
	}
	if idx.ConfigFingerprint != cfgFP {
		return nil, media.E(media.ErrConfigFingerprint, "checkpoint.restore", media.Meta{},
			fmt.Errorf("checkpoint config %s != current %s", idx.ConfigFingerprint, cfgFP))
	}
	return &Manager{root: root, Index: idx}, nil
}

// Root returns the checkpoint root.
func (m *Manager) Root() string { return m.root }
