package stream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"mediaslice/media"
	"mediaslice/scan"
)

// CheckpointVersion identifies the on-disk checkpoint schema.
const CheckpointVersion = "mediaslice-checkpoint/v1"

// Checkpoint is the durable resume cursor. It is written atomically: a crash
// can leave either the previous complete checkpoint or the new one, never a
// torn file, and the last fully written segment always precedes it.
type Checkpoint struct {
	Version       string             `json:"version"`
	ParserVersion string             `json:"parser_version"`
	// NextSequence is the first segment not yet completed.
	NextSequence  int                `json:"next_sequence"`
	// BoundaryTick records the exact cut tick at NextSequence.
	BoundaryTick  media.Ticks        `json:"boundary_tick"`
	// Completed maps "repID/seq" -> SegmentEntry for finished segments.
	Completed     map[string]SegmentEntry `json:"completed"`
	// InputHashes is the source fingerprint captured at first run.
	InputHashes   map[string]string `json:"input_hashes"`
	// ManifestDigest covers the deterministic ordering of emitted segments.
	ManifestDigest string           `json:"manifest_digest"`
}

func checkpointPath(workDir string) (string, error) {
	return scan.SafeJoin(workDir, "checkpoint.json")
}

// SaveCheckpoint atomically writes cp under workDir.
func SaveCheckpoint(workDir string, cp *Checkpoint) error {
	path, err := checkpointPath(workDir)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return media.NewError(media.KindIO, path, err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		return media.NewError(media.KindIO, path, err.Error())
	}
	return nil
}

// LoadCheckpoint reads a checkpoint; a missing file returns (nil, nil).
func LoadCheckpoint(workDir string) (*Checkpoint, error) {
	path, err := checkpointPath(workDir)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, media.NewError(media.KindIO, path, err.Error())
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, media.NewError(media.KindCheckpoint, path,
			"invalid checkpoint JSON: "+err.Error())
	}
	return &cp, nil
}

// CheckpointMismatch identifies the first position where resume validation
// failed, so an operator can inspect the exact inconsistent artifact.
type CheckpointMismatch struct {
	Position string
	Detail   string
}

func (m *CheckpointMismatch) Error() string {
	return fmt.Sprintf("resume refused at %s: %s", m.Position, m.Detail)
}

// VerifyResume validates input fingerprints, per-segment checksums and the
// checkpoint boundary before any segment is reused. The first mismatch wins.
func VerifyResume(workDir string, cp *Checkpoint, inputs map[string]*scan.FileIndex,
	boundaryTick media.Ticks) *CheckpointMismatch {
	if cp.Version != CheckpointVersion {
		return &CheckpointMismatch{"checkpoint.version", "schema version mismatch"}
	}
	if cp.ParserVersion != scan.ParserVersion {
		return &CheckpointMismatch{"checkpoint.parser_version",
			"parser changed since checkpoint"}
	}
	// 1. Input hashes must match byte-for-byte.
	for repID, idx := range inputs {
		want, ok := cp.InputHashes[repID]
		if !ok {
			return &CheckpointMismatch{"input_hash:" + repID, "missing from checkpoint"}
		}
		if want != idx.InputSHA256 {
			return &CheckpointMismatch{"input_hash:" + repID,
				"source bytes changed: " + short(want) + " != " + short(idx.InputSHA256)}
		}
	}
	if len(cp.InputHashes) != len(inputs) {
		return &CheckpointMismatch{"input_hash", "representation set changed"}
	}
	// 2. Every completed segment file must exist and re-hash identically.
	for key, e := range cp.Completed {
		path, err := scan.SafeJoin(workDir, e.FileName)
		if err != nil {
			return &CheckpointMismatch{"segment:" + key, err.Error()}
		}
		sum, size, err := scan.HashFile(path)
		if err != nil {
			return &CheckpointMismatch{"segment:" + key, "unreadable: " + err.Error()}
		}
		if sum != e.SHA256 {
			return &CheckpointMismatch{"segment:" + key,
				"checksum mismatch at sequence " + fmt.Sprint(e.Sequence)}
		}
		if size != e.Bytes {
			return &CheckpointMismatch{"segment:" + key, "size mismatch"}
		}
	}
	// 3. The resume boundary tick must equal the freshly computed plan tick.
	if cp.BoundaryTick != boundaryTick {
		return &CheckpointMismatch{"boundary",
			fmt.Sprintf("tick %d != planned %d", cp.BoundaryTick, boundaryTick)}
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// hashEntries produces the running manifest digest over deterministic
// (repID, sequence, sha256) tuples.
func hashEntries(entries []SegmentEntry) string {
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s/%d:%s\n", e.RepID, e.Sequence, e.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))
}
