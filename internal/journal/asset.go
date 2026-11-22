package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AssetState is the asset-level resume record. Output is one stream per asset,
// so resume needs a single boundary (the byte length of the last complete
// packet) plus a digest of that prefix to detect tampering. The per-track
// State records, written alongside, satisfy the "per asset, track, input
// packet range" audit requirement.
//
// On resume the writer truncates the output to PrefixBytes and appends, which
// makes a torn trailing packet impossible and guarantees no packet is written
// twice.
type AssetState struct {
	AssetID string `json:"asset_id"`
	// CommittedPackets is how many output packets form the durable prefix.
	CommittedPackets int64 `json:"committed_packets"`
	// PrefixBytes is the byte length of the durable prefix.
	PrefixBytes int64 `json:"prefix_bytes"`
	// PrefixSHA256 is the digest of the prefix bytes.
	PrefixSHA256 string `json:"prefix_sha256"`
	// SourceSHA256, Adapter, AdapterVersion and ConfigDigest are the guards.
	SourceSHA256   string `json:"source_sha256"`
	Adapter        string `json:"adapter"`
	AdapterVersion int    `json:"adapter_version"`
	ConfigDigest   string `json:"config_digest"`
	// Tracks records per-track committed ranges (audit).
	Tracks []TrackRange `json:"tracks,omitempty"`
	// UpdatedAt is informational only.
	UpdatedAt string `json:"updated_at"`
}

// TrackRange is one track's committed input-packet range.
type TrackRange struct {
	TrackID          uint32 `json:"track_id"`
	Slot             string `json:"slot,omitempty"`
	CommittedThrough int64  `json:"committed_through"`
	CommitCount      int64  `json:"commit_count"`
}

func assetFileName(assetID string) string {
	return fmt.Sprintf("%s__asset.json", sanitize(assetID))
}

// LoadCheckpoint returns the asset-level checkpoint when it is still valid
// against g. A guard mismatch quarantines it and returns (nil, true, nil); a
// corrupt file quarantines it and returns an error.
func (j *Journal) LoadCheckpoint(assetID string, g Guards) (st *AssetState, quarantined bool, err error) {
	path := filepath.Join(j.dir, assetFileName(assetID))
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var s AssetState
	if err := json.Unmarshal(data, &s); err != nil {
		if qerr := j.quarantine(path); qerr != nil {
			return nil, false, qerr
		}
		return nil, true, fmt.Errorf("journal: corrupt asset checkpoint %s: %w", assetID, err)
	}
	if s.SourceSHA256 != g.SourceSHA256 || s.Adapter != g.Adapter ||
		s.AdapterVersion != g.AdapterVersion || s.ConfigDigest != g.ConfigDigest {
		if qerr := j.quarantine(path); qerr != nil {
			return nil, false, qerr
		}
		return nil, true, nil
	}
	return &s, false, nil
}

// SaveCheckpoint atomically writes the asset-level checkpoint.
func (j *Journal) SaveCheckpoint(st *AssetState) error {
	st.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return j.writeAtomic(assetFileName(st.AssetID), st)
}

// SaveTrackRanges persists the per-track audit ranges for an asset as a
// separate file, so the resume checkpoint stays small and fixed-size.
func (j *Journal) SaveTrackRanges(assetID string, ranges []TrackRange) error {
	return j.writeAtomic(fmt.Sprintf("%s__tracks.json", sanitize(assetID)), ranges)
}

// LoadTrackRanges reads the per-track audit ranges.
func (j *Journal) LoadTrackRanges(assetID string) ([]TrackRange, error) {
	data, err := os.ReadFile(filepath.Join(j.dir, fmt.Sprintf("%s__tracks.json", sanitize(assetID))))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ranges []TrackRange
	if err := json.Unmarshal(data, &ranges); err != nil {
		return nil, err
	}
	return ranges, nil
}

func (j *Journal) writeAtomic(name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(j.dir, ".ckpt-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(j.dir, name))
}
