package pipeline

// Checkpoints persist enough state to resume a delivery after a crash
// without rewriting samples. Every checkpoint carries the input fingerprint,
// the mapping signature, the last confirmed sample per track and the output
// offset, plus a checksum for each committed output segment.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"mediacore/internal/media"
)

// Version is the on-disk schema version. A checkpoint from another version is
// rejected rather than half-understood.
const Version = 1

// TrackState is the committed progress of one output track at a segment
// boundary. SHA256 covers the track's samples written so far, in order.
type TrackState struct {
	Key      string `json:"key"`
	OutputID uint32 `json:"output_id"`
	Written  int    `json:"written"`
	SHA256   string `json:"sha256"`
	// HashState is the marshaled SHA-256 state after Written samples, so a
	// resumed run can continue a single running hash instead of
	// re-reading the already-verified prefix.
	HashState string `json:"hash_state,omitempty"`
}

// Segment is one verified run of output bytes.
type Segment struct {
	// End is the output offset after this segment.
	End int64 `json:"end"`
	// SHA256 covers bytes [previous segment end, End).
	SHA256 string `json:"sha256"`
	// Wave is the number of completed interleave waves.
	Wave   int          `json:"wave"`
	Tracks []TrackState `json:"tracks"`
}

// Checkpoint is the persisted resume state.
type Checkpoint struct {
	Version          int       `json:"version"`
	InputFingerprint string    `json:"input_fingerprint"`
	MappingSign      string    `json:"mapping_sign"`
	OutputPath       string    `json:"output_path"`
	Complete         bool      `json:"complete"`
	Segments         []Segment `json:"segments"`
	// FinalSHA maps track key to the SHA-256 of that track's output sample
	// stream once the delivery completed.
	FinalSHA      map[string]string `json:"final_sha256,omitempty"`
	FinalInputSHA map[string]string `json:"final_input_sha256,omitempty"`
	TotalSamples  int               `json:"total_samples"`
}

// Manager writes checkpoints atomically and verifies them on load.
type Manager struct {
	path string
	cp   Checkpoint
	// nextExpected is the segment start used when appending the next record.
	nextExpected int64
	// resume marks that the current segment list starts at plan.Offset.
	resume *ResumePlan
}

// NewManager creates a manager for path. It does not read an existing file;
// use Load for that.
func NewManager(path string) *Manager {
	return &Manager{path: path, cp: Checkpoint{Version: Version, FinalSHA: map[string]string{}}}
}

// Adopt continues from a verified resume plan: prior segments stay in the
// file and are preserved on the next save.
func (m *Manager) Adopt(fingerprint, mappingSign, outputPath string, totalSamples int, plan *ResumePlan) {
	m.Init(fingerprint, mappingSign, outputPath, totalSamples)
	m.cp.Segments = nil
	m.nextExpected = plan.Offset
	m.resume = plan
}

// Path returns the checkpoint file location.
func (m *Manager) Path() string { return m.path }

// State returns a copy of the current checkpoint.
func (m *Manager) State() Checkpoint {
	cp := m.cp
	cp.Segments = append([]Segment(nil), m.cp.Segments...)
	return cp
}

// Init records the identity fields before the first sample.
func (m *Manager) Init(fingerprint, mappingSign, outputPath string, totalSamples int) {
	m.cp.InputFingerprint = fingerprint
	m.cp.MappingSign = mappingSign
	m.cp.OutputPath = outputPath
	m.cp.TotalSamples = totalSamples
}

// AppendSegment commits a segment, computing its checksum from the output
// file. The caller must have flushed the file first.
func (m *Manager) AppendSegment(out *os.File, start, end int64, wave int, tracks []TrackState) error {
	if end < start {
		return media.NewError(media.ClassCheckpointCorrupt,
			"segment ends at %d before its start %d", end, start)
	}
	h := sha256.New()
	sec := io.NewSectionReader(out, start, end-start)
	if _, err := io.Copy(h, sec); err != nil {
		return media.NewError(media.ClassIO, "cannot checksum output segment").WithCause(err)
	}
	m.cp.Segments = append(m.cp.Segments, Segment{
		End:    end,
		SHA256: hex.EncodeToString(h.Sum(nil)),
		Wave:   wave,
		Tracks: tracks,
	})
	m.nextExpected = end
	return m.save()
}

// RecordFinal stores one track's input and output stream hashes on the
// in-memory checkpoint. It does not save; Complete persists the whole set.
func (m *Manager) RecordFinal(key, input, output string) error {
	if m.cp.FinalSHA == nil {
		m.cp.FinalSHA = map[string]string{}
	}
	if m.cp.FinalInputSHA == nil {
		m.cp.FinalInputSHA = map[string]string{}
	}
	m.cp.FinalSHA[key] = output
	m.cp.FinalInputSHA[key] = input
	return nil
}

// Complete records the final per-track checksums and marks the delivery done.
func (m *Manager) Complete(final map[string]string) error {
	m.cp.Complete = true
	m.cp.FinalSHA = final
	return m.save()
}

func (m *Manager) save() error {
	if m.path == "" {
		return nil
	}
	if dir := filepath.Dir(m.path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return media.NewError(media.ClassIO, "cannot create checkpoint directory").WithCause(err)
		}
	}
	data, err := json.MarshalIndent(m.cp, "", "  ")
	if err != nil {
		return media.NewError(media.ClassIO, "cannot encode checkpoint").WithCause(err)
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return media.NewError(media.ClassIO, "cannot write checkpoint").WithCause(err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return media.NewError(media.ClassIO, "cannot replace checkpoint").WithCause(err)
	}
	return nil
}

// Read returns a checkpoint without verifying it against an output. It is
// used for complete checkpoints, whose verification needs the output layout
// and therefore lives in the muxer.
func Read(path string) (Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Checkpoint{}, media.NewError(media.ClassIO, "cannot read checkpoint %s", path).WithCause(err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return Checkpoint{}, media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint %s is not valid JSON", path).WithCause(err)
	}
	if cp.Version != Version {
		return Checkpoint{}, media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint version %d is not supported (want %d)", cp.Version, Version)
	}
	return cp, nil
}

// ResumePlan is the verified starting point for a resumed run.
type ResumePlan struct {
	Wave       int
	Offset     int64
	Written    map[string]int
	SHA256     map[string]string
	HashState  map[string]string
	Segments   int
	RolledBack int // segments discarded because they failed verification
}

// Load validates a checkpoint against the current input and output, verifies
// committed segments newest-first and rolls the output back to the latest
// segment that is intact. A checkpoint without any intact segment resets the
// output to zero bytes.
func Load(path, fingerprint, mappingSign, outputPath string) (*ResumePlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, media.NewError(media.ClassCheckpointCorrupt,
				"checkpoint %s does not exist", path).WithOffset(0)
		}
		return nil, media.NewError(media.ClassIO, "cannot read checkpoint %s", path).WithCause(err)
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint %s is not valid JSON", path).WithCause(err)
	}
	if cp.Version != Version {
		return nil, media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint version %d is not supported (want %d)", cp.Version, Version)
	}
	if cp.InputFingerprint != fingerprint {
		return nil, media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint was written for a different input")
	}
	if cp.MappingSign != mappingSign {
		return nil, media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint was written for a different track mapping")
	}
	if cp.OutputPath != outputPath {
		return nil, media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint names output %q but this run writes %q", cp.OutputPath, outputPath)
	}
	if cp.Complete {
		return nil, media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint already marks the delivery complete")
	}

	out, err := os.OpenFile(outputPath, os.O_RDWR, 0o644)
	if err != nil {
		return nil, media.NewError(media.ClassIO, "cannot open output %s for resume", outputPath).WithCause(err)
	}
	defer out.Close()
	info, err := out.Stat()
	if err != nil {
		return nil, media.NewError(media.ClassIO, "cannot stat output").WithCause(err)
	}

	plan := &ResumePlan{
		Written:   map[string]int{},
		SHA256:    map[string]string{},
		HashState: map[string]string{},
	}
	for i := len(cp.Segments) - 1; i >= 0; i-- {
		seg := cp.Segments[i]
		var start int64
		if i > 0 {
			start = cp.Segments[i-1].End
		}
		if seg.End > info.Size() {
			plan.RolledBack++
			continue
		}
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(out, start, seg.End-start)); err != nil {
			return nil, media.NewError(media.ClassIO, "cannot verify output segment").WithCause(err)
		}
		if hex.EncodeToString(h.Sum(nil)) != seg.SHA256 {
			plan.RolledBack++
			continue
		}
		plan.Wave = seg.Wave
		plan.Offset = seg.End
		plan.Segments = i + 1
		for _, t := range seg.Tracks {
			plan.Written[t.Key] = t.Written
			plan.SHA256[t.Key] = t.SHA256
			plan.HashState[t.Key] = t.HashState
		}
		return plan.finish(out, info)
	}
	// Nothing verified: start over from an empty output.
	return plan.finish(out, info)
}

func (p *ResumePlan) finish(out *os.File, info os.FileInfo) (*ResumePlan, error) {
	if info.Size() != p.Offset {
		if err := out.Truncate(p.Offset); err != nil {
			return nil, media.NewError(media.ClassIO,
				"cannot truncate output to the last valid checkpoint").WithCause(err)
		}
	}
	return p, nil
}

// FormatPlan renders a resume plan for terminal output.
func (p *ResumePlan) FormatPlan() string {
	return fmt.Sprintf("resume wave=%d offset=%d verified_segments=%d rolled_back=%d",
		p.Wave, p.Offset, p.Segments, p.RolledBack)
}
