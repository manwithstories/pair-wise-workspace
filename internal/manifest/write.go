// Package manifest writes deliverable manifests atomically and verifies the
// on-disk artifacts against recorded digests and re-serialization.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediad/internal/metadata"
	"mediad/internal/timeline"
)

// SchemaVersion is the manifest format identifier.
const SchemaVersion = "mediad.manifest/v1"

// FragmentRecord is one delivered, verified fragment.
type FragmentRecord struct {
	ID             string          `json:"id"`
	Path           string          `json:"path"`
	SHA256         string          `json:"sha256"`
	Size           int64           `json:"size"`
	Camera         string          `json:"camera"`
	Representation string          `json:"representation"`
	Sequence       uint32          `json:"sequence"`
	Period         int             `json:"period"`
	Timescale      uint32          `json:"timescale"`
	TFDT           uint64          `json:"tfdt"`
	StartDTS       int64           `json:"start_dts"`
	EndDTS         int64           `json:"end_dts"`
	StartPTS90k    int64           `json:"start_pts_90k"`
	SampleCount    int             `json:"sample_count"`
	KeyframeCount  int             `json:"keyframe_count"`
	RoundTripOK    bool            `json:"roundtrip_ok"`
	RTPRepaired    bool            `json:"rtp_repaired,omitempty"`
	Audio          *metadata.Audio `json:"audio,omitempty"`
	Color          *metadata.Color `json:"color,omitempty"`
}

// QuarantineRecord describes an isolated unparsable fragment.
type QuarantineRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Kind   string `json:"kind"`
	Offset int64  `json:"offset"`
	Error  string `json:"error"`
}

// SwitchMember is one representation's nearest keyframe at a switch point.
type SwitchMember struct {
	Representation string `json:"representation"`
	FragmentID     string `json:"fragment_id"`
	PTS90k         int64  `json:"pts_90k"`
	Deviation90k   int64  `json:"deviation_90k"`
	WithinFrame    bool   `json:"within_frame"`
}

// SwitchPoint is an evaluated common-keyframe boundary.
type SwitchPoint struct {
	PTS90k          int64          `json:"pts_90k"`
	Switchable      bool           `json:"switchable"`
	MaxDeviation90k int64          `json:"max_deviation_90k"`
	Reason          string         `json:"reason,omitempty"`
	Members         []SwitchMember `json:"members"`
}

// Stats carries the counters rendered to the terminal.
type Stats struct {
	Fragments       int `json:"fragments"`
	Unique          int `json:"unique"`
	Quarantined     int `json:"quarantined"`
	RoundTripOK     int `json:"roundtrip_ok"`
	RoundTripTotal  int `json:"roundtrip_total"`
	KeyframeHits    int `json:"keyframe_hits"`
	KeyframeTotal   int `json:"keyframe_total"`
	SHAOK           int `json:"sha_ok"`
	SHATotal        int `json:"sha_total"`
	Representations int `json:"representations"`
	Aligned         int `json:"aligned"`
	NotSwitchable   int `json:"not_switchable"`
	Gaps            int `json:"gaps"`
	OutOfOrder      int `json:"out_of_order"`
	Duplicates      int `json:"duplicates"`
	Periods         int `json:"periods"`
	Resumed         int `json:"resumed,omitempty"`
	DuplicateEmit   int `json:"duplicate_emit,omitempty"`
}

// Manifest is the deliverable checklist.
type Manifest struct {
	Schema            string                `json:"schema"`
	Case              string                `json:"case"`
	Seed              int64                 `json:"seed"`
	InputDigest       string                `json:"input_digest"`
	ConfigDigest      string                `json:"config_digest"`
	ResultHash        string                `json:"result_hash,omitempty"`
	Representations   []string              `json:"representations"`
	Aligned           int                   `json:"aligned"`
	Fragments         []FragmentRecord      `json:"fragments"`
	Quarantined       []QuarantineRecord    `json:"quarantined"`
	SwitchPoints      []SwitchPoint         `json:"switch_points"`
	AudioGroups       []metadata.AudioGroup `json:"audio_groups"`
	CompatibleVideo   []string              `json:"compatible_video"`
	IncompatibleVideo []string              `json:"incompatible_video"`
	Issues            []timeline.Issue      `json:"issues"`
	Stats             Stats                 `json:"stats"`
}

// WriteJSONAtomic writes v as indented JSON via a same-directory temp file and
// an atomic rename, so readers never observe a partially committed file.
func WriteJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-manifest-*")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	// os.CreateTemp defaults to 0600; deliverables are conventionally readable.
	_ = tmp.Chmod(0o644)

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close manifest: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("atomic rename to %s: %w", path, err)
	}
	return nil
}

// canonicalView is the stable subset that defines result identity.
type canonicalView struct {
	Fragments    []FragmentRecord `json:"fragments"`
	SwitchPoints []SwitchPoint    `json:"switch_points"`
	Aligned      int              `json:"aligned"`
}

// ResultHash computes a deterministic hash over the delivered fragment records
// and evaluated switch points only (no wall clock or paths outside records).
func ResultHash(frags []FragmentRecord, points []SwitchPoint, aligned int) string {
	b, err := json.Marshal(canonicalView{Fragments: frags, SwitchPoints: points, Aligned: aligned})
	if err != nil {
		// All fields are JSON-safe; a failure here is a programming error.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// FileSHA256 streams a file's hash without retaining it in memory.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
