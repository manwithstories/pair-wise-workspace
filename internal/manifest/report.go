package manifest

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"time"

	"mediapipe/internal/media"
)

type Segment struct {
	Index            int     `json:"index"`
	Name             string  `json:"name"`
	Path             string  `json:"path"`
	SHA256           string  `json:"sha256"`
	SizeBytes        int64   `json:"size_bytes"`
	TargetStartSec   float64 `json:"target_start_sec"`
	ActualStartSec   float64 `json:"actual_start_sec"`
	TargetEndSec     float64 `json:"target_end_sec"`
	ActualEndSec     float64 `json:"actual_end_sec"`
	AlignmentErrorMS float64 `json:"alignment_error_ms"`
}

type SilenceRegion struct {
	StartSec    float64 `json:"start_sec"`
	EndSec      float64 `json:"end_sec"`
	DurationSec float64 `json:"duration_sec"`
	Boundary    string  `json:"boundary,omitempty"`
}

type Recovery struct {
	DuplicatePackets int    `json:"duplicate_packets"`
	ReorderedPackets int    `json:"reordered_packets"`
	RecoveredPackets int    `json:"recovered_packets"`
	LostPackets      int    `json:"lost_packets,omitempty"`
	CacheHit         bool   `json:"cache_hit"`
	SourceOK         bool   `json:"source_ok"`
	SHA256OK         bool   `json:"sha256_ok"`
	CheckpointSeq    uint64 `json:"checkpoint_seq,omitempty"`
}

type Stats struct {
	Case              string  `json:"case,omitempty"`
	TargetKbps        int     `json:"target_kbps"`
	ActualKbps        int     `json:"actual_kbps"`
	DeviationPct      float64 `json:"deviation_pct"`
	DecodeErrors      int     `json:"decode_errors"`
	AudioPeakDBFS     float64 `json:"audio_peak_dbfs"`
	SilenceRegions    int     `json:"silence_regions"`
	DuplicateSegments int     `json:"duplicate_segments"`
	ManifestErrorMS   float64 `json:"manifest_error_ms"`
}

type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	JobID         string          `json:"job_id"`
	Seed          int64           `json:"seed"`
	CreatedAt     time.Time       `json:"created_at"`
	SourceSHA256  string          `json:"source_sha256"`
	Stats         Stats           `json:"stats"`
	Recovery      *Recovery       `json:"recovery,omitempty"`
	Segments      []Segment       `json:"segments"`
	Silence       []SilenceRegion `json:"silence"`
	Files         []string        `json:"files"`
}

func New(jobID string, seed int64, sourceSHA string) Manifest {
	return Manifest{
		SchemaVersion: 1, JobID: jobID, Seed: seed, CreatedAt: time.Now().UTC(),
		SourceSHA256: sourceSHA, Segments: []Segment{}, Silence: []SilenceRegion{}, Files: []string{},
	}
}

// SetBitrate derives actual throughput from product bytes and duration.
func (m *Manifest) SetBitrate(totalBytes int64, durationSec float64, targetKbps int) {
	m.Stats.TargetKbps = targetKbps
	if durationSec <= 0 {
		m.Stats.ActualKbps = 0
		m.Stats.DeviationPct = 100
		return
	}
	actual := float64(totalBytes*8) / 1000 / durationSec
	m.Stats.ActualKbps = int(math.Round(actual))
	if targetKbps > 0 {
		m.Stats.DeviationPct = math.Round(math.Abs(actual-float64(targetKbps))/float64(targetKbps)*1000) / 10
	}
}

func (m *Manifest) AddSegment(s Segment) {
	m.Segments = append(m.Segments, s)
	m.Files = append(m.Files, s.Name)
	m.Stats.ManifestErrorMS = math.Max(m.Stats.ManifestErrorMS, s.AlignmentErrorMS)
}

// ValidateAlignment enforces the broadcast package manifest tolerance.
func (m Manifest) ValidateAlignment(limitMS float64) error {
	if limitMS <= 0 {
		limitMS = 42
	}
	if m.Stats.ManifestErrorMS > limitMS+1e-9 {
		return media.NewError(media.CodeInvalidInput, "manifest alignment exceeds tolerance", nil)
	}
	return nil
}

func WriteAtomicJSON(path string, v any) (int64, string, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return 0, "", media.WrapError(media.CodeEncode, "marshal json", err)
	}
	n, sum, err := media.AtomicWriteFile(path, bytes.NewReader(append(b, '\n')), 0o644)
	if err != nil {
		return n, sum, err
	}
	return n, sum, nil
}

// JSONLine writes one stable terminal event as a single flattened JSON object.
func JSONLine(w io.Writer, event string, payload any) error {
	fields := map[string]any{}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &fields); err != nil {
			return err
		}
	}
	fields["event"] = event
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// FaultLine is the terminal record for an expected corruption/rejection case.
type FaultLine struct {
	Event     string `json:"event"`
	Case      string `json:"case"`
	ErrorCode string `json:"error_code"`
	ExitCode  int    `json:"exit_code"`
	OK        bool   `json:"expected_rejected"`
	Detail    string `json:"detail,omitempty"`
}
