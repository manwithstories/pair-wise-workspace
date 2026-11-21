// Package manifest writes the run manifest and statistics, and verifies that a
// repeat run is stable (same frame hashes, same alignment).
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"mediad/internal/model"
	"mediad/internal/scheduler"
	"mediad/internal/transport"
)

// Version is the manifest schema version.
const Version = 1

// Manifest is the machine-readable run record.
type Manifest struct {
	Version int    `json:"version"`
	Seed    int64  `json:"seed"`
	Mode    string `json:"mode"`
	// DeterministicSalt is included so two runs can be compared field-wise.
	Timebase string `json:"timebase"`
	// Tracks captures normalized metadata for every input track.
	Tracks []TrackRecord `json:"tracks"`
	// Color is the resolved color description and its provenance.
	Color ColorRecord `json:"color"`
	// Frames records per-frame hashes, blocking and gap linkage.
	Frames []FrameRecord `json:"frames"`
	// Gaps links transport gaps to the frames they block.
	Gaps []GapRecord `json:"gaps"`
	// Bandwidth is the per-tick target/actual trace.
	Bandwidth []transport.BandwidthSampleRecord `json:"bandwidth"`
	// Transport summarizes packet handling.
	Transport transport.StreamStats `json:"transport"`
	// Nodes records scheduler outcomes and idempotency hashes.
	Nodes []NodeRecord `json:"nodes"`
	// Failures records isolated corrupted-input failures.
	Failures []FailureRecord `json:"failures"`
	// Metrics are the acceptance measurements.
	Metrics Metrics `json:"metrics"`
	// Artifacts lists files written next to the manifest with hashes.
	Artifacts []ArtifactRecord `json:"artifacts"`
	// Stable is the re-run comparison result.
	Stable *StabilityRecord `json:"stability,omitempty"`
}

// TrackRecord is one normalized track.
type TrackRecord struct {
	Key          string   `json:"key"`
	Kind         string   `json:"kind"`
	Codec        string   `json:"codec"`
	Timebase     string   `json:"timebase"`
	Start        int64    `json:"start"`
	Duration     int64    `json:"duration"`
	Rotation     int      `json:"rotation"`
	RotationRaw  string   `json:"rotation_raw"`
	CodedWidth   int      `json:"coded_width"`
	CodedHeight  int      `json:"coded_height"`
	OutputWidth  int      `json:"output_width"`
	OutputHeight int      `json:"output_height"`
	Crop         string   `json:"crop,omitempty"`
	Color        string   `json:"color"`
	Container    string   `json:"container"`
	Notes        []string `json:"notes,omitempty"`
}

// ColorRecord is the resolved color decision.
type ColorRecord struct {
	Primaries uint16   `json:"primaries"`
	Transfer  uint16   `json:"transfer"`
	Matrix    uint16   `json:"matrix"`
	FullRange bool     `json:"full_range"`
	Source    string   `json:"source"`
	Defaulted bool     `json:"defaulted"`
	Conflicts []string `json:"conflicts,omitempty"`
}

// FrameRecord is one output frame.
type FrameRecord struct {
	Index       int    `json:"index"`
	Start       int64  `json:"start"`
	End         int64  `json:"end"`
	PlanHash    string `json:"plan_hash"`
	FrameHash   string `json:"frame_hash"`
	Blocked     bool   `json:"blocked"`
	BlockingDep string `json:"blocking_dep,omitempty"`
	Hole        bool   `json:"hole"`
	Overlaps    int    `json:"overlaps"`
	Boundary    bool   `json:"boundary"`
	Gaps        []int  `json:"gaps,omitempty"`
}

// GapRecord links a transport gap to blocked frames.
type GapRecord struct {
	Index         int    `json:"index"`
	StartExt      int64  `json:"start_ext"`
	Count         int64  `json:"count"`
	Blocked       bool   `json:"blocked"`
	Dep           string `json:"dep,omitempty"`
	TimestampFrom uint32 `json:"timestamp_from,omitempty"`
	TimestampTo   uint32 `json:"timestamp_to,omitempty"`
	Frames        []int  `json:"frames"`
	ManifestMs    int64  `json:"manifest_ms"`
}

// NodeRecord is one scheduler node outcome.
type NodeRecord struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	Hash      string `json:"hash,omitempty"`
	BlockedBy string `json:"blocked_by,omitempty"`
	RootCause string `json:"root_cause,omitempty"`
	Error     string `json:"error,omitempty"`
}

// FailureRecord is one isolated corrupted-input failure.
type FailureRecord struct {
	Track    string `json:"track"`
	Category string `json:"category"`
	Interval string `json:"interval,omitempty"`
	Blocking string `json:"blocking_dep,omitempty"`
	Message  string `json:"message"`
}

// Metrics are the acceptance numbers.
type Metrics struct {
	TargetKbps       float64 `json:"target_kbps"`
	ActualKbps       float64 `json:"actual_kbps"`
	BitrateErrorPct  float64 `json:"bitrate_error_pct"`
	FrameCount       int     `json:"frame_count"`
	PSNRMeanDB       float64 `json:"psnr_mean_db"`
	SSIMMean         float64 `json:"ssim_mean"`
	ManifestDriftMs  int64   `json:"manifest_drift_ms"`
	DuplicatePackets int     `json:"duplicate_packets"`
	ReorderedPackets int     `json:"reordered_packets"`
	LostPackets      int     `json:"lost_packets"`
	BlockedNode      string  `json:"blocked_node"`
}

// ArtifactRecord is one file produced by the run.
type ArtifactRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// StabilityRecord compares a re-run with the first run.
type StabilityRecord struct {
	FrameHashesStable bool     `json:"frame_hashes_stable"`
	MismatchedFrames  []int    `json:"mismatched_frames,omitempty"`
	ManifestStable    bool     `json:"manifest_stable"`
	DriftMs           int64    `json:"drift_ms"`
	Reasons           []string `json:"reasons,omitempty"`
}

// New builds an empty manifest.
func New(seed int64, mode string) *Manifest {
	return &Manifest{Version: Version, Seed: seed, Mode: mode}
}

// AddArtifact hashes a file and records it.
func (m *Manifest) AddArtifact(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return model.Wrap(model.CategoryManifest, "artifact", err)
	}
	sum := sha256.Sum256(data)
	m.Artifacts = append(m.Artifacts, ArtifactRecord{
		Path: filepath.Base(path), SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)),
	})
	sort.SliceStable(m.Artifacts, func(i, j int) bool { return m.Artifacts[i].Path < m.Artifacts[j].Path })
	return nil
}

// NodesFromReport converts scheduler output into manifest records.
func (m *Manifest) NodesFromReport(r *scheduler.Report) {
	for _, st := range r.NodeStateList() {
		rec := NodeRecord{Name: st.Name, Status: string(st.Status),
			Attempts: st.Attempts, Hash: st.Result.Hash,
			BlockedBy: st.BlockedBy, RootCause: st.RootCause}
		if st.Err != nil {
			rec.Error = st.Err.Error()
		}
		m.Nodes = append(m.Nodes, rec)
	}
}

// Write atomically writes the manifest as indented JSON.
func (m *Manifest) Write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return model.Wrap(model.CategoryManifest, "mkdir", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return model.Wrap(model.CategoryManifest, "marshal", err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return model.Wrap(model.CategoryManifest, "write", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.Wrap(model.CategoryManifest, "rename", err)
	}
	return nil
}

// Load reads a manifest from disk.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.Wrap(model.CategoryManifest, "read", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, model.Wrap(model.CategoryManifest, "unmarshal", err)
	}
	return &m, nil
}

// Compare verifies that a re-run is stable: frame hashes match by index, the
// frame count matches, and per-frame alignment drift is within tolerance.
func Compare(first, second *Manifest, maxDriftMs int64) StabilityRecord {
	rec := StabilityRecord{FrameHashesStable: true, ManifestStable: true}
	if first.Metrics.FrameCount != second.Metrics.FrameCount {
		rec.FrameHashesStable = false
		rec.ManifestStable = false
		rec.Reasons = append(rec.Reasons,
			fmt.Sprintf("frame_count %d != %d", first.Metrics.FrameCount, second.Metrics.FrameCount))
	}
	byIndex := make(map[int]FrameRecord, len(second.Frames))
	for _, f := range second.Frames {
		byIndex[f.Index] = f
	}
	for _, f := range first.Frames {
		g, ok := byIndex[f.Index]
		if !ok {
			rec.FrameHashesStable = false
			rec.MismatchedFrames = append(rec.MismatchedFrames, f.Index)
			continue
		}
		if f.FrameHash != g.FrameHash || f.PlanHash != g.PlanHash {
			rec.FrameHashesStable = false
			rec.MismatchedFrames = append(rec.MismatchedFrames, f.Index)
		}
		if d := abs64(f.Start - g.Start); d > rec.DriftMs {
			rec.DriftMs = d
		}
	}
	if !rec.FrameHashesStable {
		rec.ManifestStable = false
		rec.Reasons = append(rec.Reasons, "frame plan or pixel hash changed")
	}
	if rec.DriftMs > maxDriftMs {
		rec.ManifestStable = false
		rec.Reasons = append(rec.Reasons, fmt.Sprintf("alignment drift %dms > %dms", rec.DriftMs, maxDriftMs))
	}
	if first.Color.Key() != second.Color.Key() {
		rec.ManifestStable = false
		rec.Reasons = append(rec.Reasons, "color resolution changed")
	}
	// Compare the track keys that contributed to the product. Failure records
	// are deliberately excluded: a corrupt-input run and its verification
	// share the same contributing set even though one has extra diagnostics.
	if !sameTrackKeys(contributing(first.Tracks), contributing(second.Tracks)) {
		rec.ManifestStable = false
		rec.Reasons = append(rec.Reasons, "track set changed")
	}
	return rec
}

// ColorKey renders the color decision for comparison.
// contributing filters out tracks that cannot paint. A corrupt-input run still
// parses the healthy audio track, but audio never contributes layers, so the
// product track set must be compared on video tracks only.
func contributing(tracks []TrackRecord) []TrackRecord {
	out := make([]TrackRecord, 0, len(tracks))
	for _, t := range tracks {
		if t.Kind == "video" {
			out = append(out, t)
		}
	}
	return out
}

func sameTrackKeys(a, b []TrackRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key {
			return false
		}
	}
	return true
}

func (c ColorRecord) Key() string {
	return fmt.Sprintf("p%d/t%d/m%d/full=%t/src=%s", c.Primaries, c.Transfer, c.Matrix, c.FullRange, c.Source)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
