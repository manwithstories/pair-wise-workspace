package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"mediaaudit/internal/index"
	"mediaaudit/internal/model"
)

// DamageSample locates one contiguous damaged frame range for the report.
type DamageSample struct {
	Track      string `json:"track"`
	FirstFrame int64  `json:"first_frame"`
	LastFrame  int64  `json:"last_frame"`
	ByteOffset int64  `json:"byte_offset"`
}

// Report is the stable, JSON-serializable analysis result. Field order and the
// checksum are deterministic so repeated runs produce byte-identical output.
type Report struct {
	PackagePath    string         `json:"package_path"`
	SampleRate     int64          `json:"sample_rate"`
	Channels       int            `json:"channels"`
	SampleFrames   [2]int64       `json:"sample_frames"` // [first, last) of damage
	ByteOffset     int64          `json:"byte_offset"`
	DamageCount    int            `json:"damage_count"`
	KeyframeGapMs  []float64      `json:"keyframe_gap_ms"`
	ClockDriftPPM  float64        `json:"clock_drift_ppm"`
	Chunks         int            `json:"chunks_committed"`
	ResumeMs       int64          `json:"resume_ms"`
	IndexSHA256    string         `json:"index_sha256"`
	ReportSHA256   string         `json:"report_sha256"`
	Faults         []model.Fault  `json:"faults"`
}

// Recompute re-runs the chunk function over every committed chunk and
// compares the damage counts with the index, proving the stored result matches
// a fresh calculation. It returns an error on any mismatch.
func Recompute(idx *index.File, totalFrames, chunkFrames int64, fn ChunkFunc) error {
	total := chunkCount(totalFrames, chunkFrames)
	if int64(len(idx.Payload.Chunks)) != total {
		return model.NewFault(model.KindIndex, "", model.FrameRange{}, -1,
			"verify: index chunk count does not cover input")
	}
	for seq := int64(0); seq < total; seq++ {
		r := frameRange(seq, totalFrames, chunkFrames)
		dmg, err := fn(seq, r)
		if err != nil {
			return err
		}
		got := idx.Payload.Chunks[seq]
		if got.Seq != seq || got.FirstFrame != int64(r.First) || got.LastFrame != int64(r.Last) {
			return model.NewFault(model.KindIndex, "", r, -1,
				"verify: stored chunk range does not match recomputation")
		}
		wantStatus := "ok"
		if dmg > 0 {
			wantStatus = "damaged"
		}
		if got.Status != wantStatus || got.DamageCount != dmg {
			return model.NewFault(model.KindIndex, "", r, -1,
				"verify: stored damage does not match recomputation")
		}
	}
	return nil
}

// checksumView is the deterministic subset of Report hashed for the checksum.
// Wall-clock measurements (resume_ms) are deliberately excluded so repeated
// runs over identical inputs yield a byte-stable checksum.
type checksumView struct {
	PackagePath   string        `json:"package_path"`
	SampleRate    int64         `json:"sample_rate"`
	Channels      int           `json:"channels"`
	SampleFrames  [2]int64      `json:"sample_frames"`
	ByteOffset    int64         `json:"byte_offset"`
	DamageCount   int           `json:"damage_count"`
	KeyframeGapMs []float64     `json:"keyframe_gap_ms"`
	ClockDriftPPM float64       `json:"clock_drift_ppm"`
	Chunks        int           `json:"chunks_committed"`
	IndexSHA256   string        `json:"index_sha256"`
	Faults        []model.Fault `json:"faults"`
}

// FinalizeReport fills derived fields and computes the self-referential report
// checksum over the deterministic subset (excluding timing-only fields).
func FinalizeReport(r *Report) error {
	view := checksumView{
		PackagePath:   r.PackagePath,
		SampleRate:    r.SampleRate,
		Channels:      r.Channels,
		SampleFrames:  r.SampleFrames,
		ByteOffset:    r.ByteOffset,
		DamageCount:   r.DamageCount,
		KeyframeGapMs: r.KeyframeGapMs,
		ClockDriftPPM: r.ClockDriftPPM,
		Chunks:        r.Chunks,
		IndexSHA256:   r.IndexSHA256,
		Faults:        r.Faults,
	}
	b, err := json.Marshal(view)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	r.ReportSHA256 = hex.EncodeToString(sum[:])
	return nil
}

// Render serializes the report with stable indentation.
func Render(r *Report) ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
