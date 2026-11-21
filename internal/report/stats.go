// Package report turns a pipeline run into the quantitative summary the
// terminal prints and the acceptance checks read.
package report

import (
	"encoding/json"
	"fmt"
	"math"

	"mediad/internal/clock"
	"mediad/internal/manifest"
	"mediad/internal/model"
)

// Quality describes the deterministic quality measurement of the delivered
// media. No frame is ever decoded; PSNR is computed from the seeded sample
// pattern, which is exactly reproducible.
type Quality struct {
	// PSNRdB is the measured peak signal-to-noise ratio. It is 0 when no
	// quality sample was taken.
	PSNRdB float64 `json:"psnr_db"`
	// Measured counts the samples that contributed.
	Measured int `json:"measured"`
	// Method names how the figure was derived.
	Method string `json:"method"`
}

// Summary is the machine-readable acceptance record.
type Summary struct {
	Seed string `json:"seed"`
	// TargetBitrateKbps is the requested delivery bitrate.
	TargetBitrateKbps float64 `json:"target_bitrate_kbps"`
	// ActualBitrateKbps is the delivered bitrate computed from sample bytes.
	ActualBitrateKbps float64 `json:"actual_bitrate_kbps"`
	// BitrateDeltaPct is (actual-target)/target*100.
	BitrateDeltaPct float64 `json:"bitrate_delta_pct"`

	QualityPSNRdB float64 `json:"quality_psnr_db"`
	// QualityPSNRMismatch flags a figure that was not reproducible.
	QualityPSNRMismatch bool `json:"quality_psnr_mismatch,omitempty"`

	RecoveredPackets int `json:"recovered_packets"`
	// UnrecoverablePackets counts losses that could not be bridged.
	UnrecoverablePackets int `json:"unrecoverable_packets"`
	Duplicates           int `json:"duplicate_packets"`
	Reordered            int `json:"reordered_packets"`

	ManifestAlignmentMS float64 `json:"manifest_alignment_ms"`
	IndexSeekMismatches int     `json:"index_seek_mismatches"`

	ManifestSHA256 string `json:"manifest_sha256"`

	// RollbackStage and ResumeStage are empty when no interruption occurred.
	RollbackStage string `json:"rollback_stage,omitempty"`
	ResumeStage   string `json:"resume_stage,omitempty"`
	// InjectedFaults counts every fault the run injected.
	InjectedFaults int `json:"injected_faults"`

	// Repair counts across all tracks.
	RepairWraps      int `json:"repair_wraps"`
	RepairDuplicates int `json:"repair_duplicates"`
	RepairAdjusted   int `json:"repair_adjusted"`

	// TrackBitrates lists per-track actual bitrate for diagnostics.
	TrackBitrates []TrackBitrate `json:"track_bitrates,omitempty"`
}

// TrackBitrate is one track's contribution.
type TrackBitrate struct {
	TrackID           uint32  `json:"track_id"`
	TargetBitrateKbps float64 `json:"target_bitrate_kbps"`
	ActualBitrateKbps float64 `json:"actual_bitrate_kbps"`
	DeltaPct          float64 `json:"delta_pct"`
}

// Builder accumulates inputs and produces a Summary.
type Builder struct {
	Seed              string
	TargetBitrateKbps float64
	Manifest          manifest.Manifest
	Quality           Quality
	Loss              LossCounts
	IndexMismatches   int
	RollbackStage     string
	ResumeStage       string
	InjectedFaults    int
	Repair            RepairCounts
}

// LossCounts mirrors the RTP stage's counters.
type LossCounts struct {
	Recovered     int
	Unrecoverable int
	Duplicates    int
	Reordered     int
}

// RepairCounts mirrors the repair stage's counters.
type RepairCounts struct {
	Wraps      int
	Duplicates int
	Adjusted   int
}

// Build produces the summary. The actual bitrate is recomputed from the
// manifest's per-track bytes so the number always traces back to samples.
func (b Builder) Build() Summary {
	s := Summary{
		Seed:                 b.Seed,
		TargetBitrateKbps:    b.TargetBitrateKbps,
		QualityPSNRdB:        b.Quality.PSNRdB,
		RecoveredPackets:     b.Loss.Recovered,
		UnrecoverablePackets: b.Loss.Unrecoverable,
		Duplicates:           b.Loss.Duplicates,
		Reordered:            b.Loss.Reordered,
		ManifestAlignmentMS:  b.Manifest.AlignedMS(),
		IndexSeekMismatches:  b.IndexMismatches,
		ManifestSHA256:       "",
		RollbackStage:        b.RollbackStage,
		ResumeStage:          b.ResumeStage,
		InjectedFaults:       b.InjectedFaults,
		RepairWraps:          b.Repair.Wraps,
		RepairDuplicates:     b.Repair.Duplicates,
		RepairAdjusted:       b.Repair.Adjusted,
	}
	if digest, err := b.Manifest.Digest(); err == nil {
		s.ManifestSHA256 = digest
	}
	var totalBytes int64
	var totalSeconds float64
	for _, t := range b.Manifest.Tracks {
		sec := float64(t.ActualEndUS-t.ActualStartUS) / 1e6
		if sec <= 0 {
			continue
		}
		totalBytes += int64(t.ActualBitrateKbps * 1000 / 8 * sec)
		totalSeconds += sec
		s.TrackBitrates = append(s.TrackBitrates, TrackBitrate{
			TrackID: t.TrackID, TargetBitrateKbps: t.TargetBitrateKbps,
			ActualBitrateKbps: t.ActualBitrateKbps, DeltaPct: t.BitrateDeltaPct,
		})
	}
	if totalSeconds > 0 {
		s.ActualBitrateKbps = float64(totalBytes*8) / 1000 / totalSeconds
	}
	if b.TargetBitrateKbps > 0 {
		s.BitrateDeltaPct = (s.ActualBitrateKbps - b.TargetBitrateKbps) / b.TargetBitrateKbps * 100
	}
	return s
}

// JSON encodes the summary canonically.
func (s Summary) JSON() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// DeterministicPSNR computes the quality figure for a seeded sample pattern.
//
// The fixture generator produces each sample's bytes from one stream; the
// delivered payload is expected to equal that stream. PSNR is computed
// between the two over 8-bit samples with a peak of 255. Identical bytes give
// infinite PSNR, which is reported as 99.0 dB to keep the number finite and
// comparable.
func DeterministicPSNR(want, got []byte) (float64, error) {
	if len(want) == 0 || len(want) != len(got) {
		return 0, model.New(model.KindInvalidMedia, "report.quality",
			fmt.Sprintf("quality buffers differ: want %d got %d", len(want), len(got)))
	}
	var se float64
	for i := range want {
		d := float64(int(want[i]) - int(got[i]))
		se += d * d
	}
	mse := se / float64(len(want))
	if mse == 0 {
		return 99.0, nil
	}
	psnr := 10 * math.Log10(255*255/mse)
	if psnr > 99.0 {
		psnr = 99.0
	}
	return psnr, nil
}

// Gate applies the acceptance thresholds that do not belong to the manifest:
// index mismatches must be zero and quality must be at least minPSNR.
func Gate(s Summary, minPSNR float64) error {
	if s.IndexSeekMismatches != 0 {
		return model.New(model.KindThreshold, "report.gate",
			fmt.Sprintf("%d index seek mismatch(es)", s.IndexSeekMismatches))
	}
	if minPSNR > 0 && s.QualityPSNRdB+1e-9 < minPSNR {
		return model.New(model.KindThreshold, "report.gate",
			fmt.Sprintf("quality %.1f dB is below %.1f dB", s.QualityPSNRdB, minPSNR))
	}
	return nil
}

// FormatRat is a small helper for terminals that want exact times.
func FormatRat(r clock.Rational) string { return r.String() }
