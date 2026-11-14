package mp4atom

import (
	"io"

	"mp4replay/internal/boxscan"
	"mp4replay/internal/report"
)

// Validate enforces the subset of ISO-BMFF the replay pipeline supports.
// Unsupported shapes return a coded error instead of being silently truncated.
func (t *Table) Validate(ra io.ReaderAt, stbl *boxscan.Box) error {
	// Multiple sample descriptions are not supported by the remuxer, which
	// always emits description index 1.
	for _, e := range t.Stsc {
		if e.DescIndex != 1 {
			return report.NewError(report.ExitBoxInvalid, "validate",
				"multiple sample descriptions are not supported")
		}
		if e.SamplesPerChunk == 0 {
			return report.NewError(report.ExitBoxInvalid, "validate",
				"stsc samples_per_chunk must be positive")
		}
	}
	if n, err := ParseStsd(ra, stbl); err != nil {
		return err
	} else if n == 0 {
		return report.NewError(report.ExitBoxInvalid, "validate",
			"stsd has no sample description")
	}
	if len(t.Stts) == 0 {
		return report.NewError(report.ExitBoxInvalid, "validate",
			"empty stts")
	}
	for _, e := range t.Stts {
		if e.Count == 0 {
			return report.NewError(report.ExitBoxInvalid, "validate",
				"stts sample_count must be positive")
		}
	}
	if t.Timescale == 0 {
		return report.NewError(report.ExitBoxInvalid, "validate",
			"mdhd timescale is zero")
	}
	if t.StszFixed == 0 && t.StszCount != 0 &&
		len(t.StszSizes) != int(t.StszCount) {
		return report.NewError(report.ExitBoxInvalid, "validate",
			"stsz entry count mismatch")
	}
	return nil
}
