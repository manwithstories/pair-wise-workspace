package output

import (
	"encoding/json"
	"os"
	"path/filepath"

	"mediad/internal/model"
	"mediad/internal/transport"
)

// ManifestName is the provenance JSON file written next to the deliverable.
const ManifestName = "provenance.json"

// RecoveryDecision documents one packet-level repair outcome.
type RecoveryDecision struct {
	SequenceNumber uint16 `json:"sequence_number"`
	Mechanism      string `json:"mechanism"` // nack | xor | none
	Recovered      bool   `json:"recovered"`
	PTSNS          int64  `json:"pts_ns"`
}

// CheckpointRef pins a persisted checkpoint to its block checksums.
type CheckpointRef struct {
	SamplesDone int             `json:"samples_done"`
	RunningHash string          `json:"running_sha256"`
	Blocks      []BlockChecksum `json:"blocks"`
	Interrupted bool            `json:"interrupted"`
}

// Manifest is the immutable, content-addressed provenance record.
type Manifest struct {
	InputSHA256        string             `json:"input_sha256"`
	OutputSHA256       string             `json:"output_sha256"`
	OutputBytes        int64              `json:"output_bytes"`
	FirstBadOffset     int64              `json:"first_bad_offset"`
	Faults             []*model.Fault     `json:"faults"`
	RecoveredPackets   int                `json:"recovered_packets"`
	UnrecoveredPackets int                `json:"unrecovered_packets"`
	AVDriftMS          int64              `json:"av_drift_ms"`
	Gaps               []transport.Gap    `json:"gaps"`
	Decisions          []RecoveryDecision `json:"recovery_decisions"`
	Checkpoint         CheckpointRef      `json:"checkpoint"`
	Resumed            bool               `json:"resumed"`
}

// NewManifest allocates an empty but non-nil manifest for one source.
func NewManifest(inputSHA string) *Manifest {
	return &Manifest{
		InputSHA256:    inputSHA,
		FirstBadOffset: model.OffsetUnknown,
		Faults:         []*model.Fault{},
		Gaps:           []transport.Gap{},
		Decisions:      []RecoveryDecision{},
	}
}

// RecordFaults folds index/sync faults into the manifest and keeps the
// smallest exact bad-byte offset encountered.
func (m *Manifest) RecordFaults(faults []*model.Fault) {
	for _, f := range faults {
		m.Faults = append(m.Faults, f)
		if f.Offset >= 0 && (m.FirstBadOffset < 0 || f.Offset < m.FirstBadOffset) {
			m.FirstBadOffset = f.Offset
		}
	}
}

// RecordTransport maps stats, gaps and per-packet decisions into the manifest.
func (m *Manifest) RecordTransport(stats transport.RecoveryStats, gaps []transport.Gap,
	decisions []RecoveryDecision) {
	m.RecoveredPackets = stats.RecoveredPackets
	m.UnrecoveredPackets = stats.UnrecoveredPackets
	m.Gaps = gaps
	m.Decisions = decisions
}

// Write atomically persists the manifest under outDir.
func (m *Manifest) Write(outDir string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	target := filepath.Join(outDir, ManifestName)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// LoadManifest reads back a manifest for verification.
func LoadManifest(outDir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(outDir, ManifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}
