// Package remux turns validated track mappings into an executable,
// deterministic remux plan and writes archival copies whose compressed
// payloads are byte-identical to the source.
package remux

import (
	"crypto/sha256"
	"encoding/hex"

	"mkvarchive/internal/model"
	"mkvarchive/internal/track"
)

// Plan is the frozen input to a remux write. It is serializable so the
// recovery subsystem can verify and replay it across process runs.
type Plan struct {
	Version        string               `json:"version"`
	TimestampScale uint64               `json:"timestamp_scale_ns"`
	Mappings       []track.Mapping      `json:"mappings"`
	Warnings       []model.ArchiveError `json:"warnings,omitempty"`
}

// PlanVersion identifies the plan serialization contract.
const PlanVersion = "1.0"

// NewPlan freezes the mapping output together with the segment time scale.
func NewPlan(timestampScale uint64, mappings []track.Mapping,
	warnings []*model.ArchiveError) *Plan {
	flat := make([]model.ArchiveError, 0, len(warnings))
	for _, w := range warnings {
		flat = append(flat, *w)
	}
	return &Plan{
		Version:        PlanVersion,
		TimestampScale: timestampScale,
		Mappings:       mappings,
		Warnings:       flat,
	}
}

// OrderedMappings returns mappings in archive write order; the mapping step
// already sorted them by dependency, so this is a defensive copy.
func (p *Plan) OrderedMappings() []track.Mapping {
	out := make([]track.Mapping, len(p.Mappings))
	copy(out, p.Mappings)
	return out
}

// PayloadDigest is the SHA-256 of one block payload together with its source
// and archive positions for quality verification.
type PayloadDigest struct {
	SourceTrackNumber uint64 `json:"source_track_number"`
	ArchiveTrackID    uint64 `json:"archive_track_id"`
	SourceOffset      int64  `json:"source_offset"`
	RawTimecode       int64  `json:"raw_timecode"`
	SHA256            string `json:"sha256"`
	Bytes             int    `json:"bytes"`
}

// DigestBlock hashes a verbatim block payload.
func DigestBlock(m track.Mapping, sourceOffset, rawTimecode int64, payload []byte) PayloadDigest {
	sum := sha256.Sum256(payload)
	return PayloadDigest{
		SourceTrackNumber: m.SourceNumber,
		ArchiveTrackID:    m.ArchiveID,
		SourceOffset:      sourceOffset,
		RawTimecode:       rawTimecode,
		SHA256:            hex.EncodeToString(sum[:]),
		Bytes:             len(payload),
	}
}

// QualityReport aggregates the numeric acceptance evidence for a run.
type QualityReport struct {
	TargetBitrateKbps   float64         `json:"target_bitrate_kbps"`
	ActualBitrateKbps   float64         `json:"actual_bitrate_kbps"`
	BitrateDeviationPct float64         `json:"bitrate_deviation_pct"`
	PayloadSHA256Match  bool            `json:"payload_sha256_match"`
	QualityBlocks       int             `json:"quality_blocks"`
	ManifestAlignmentMS float64         `json:"manifest_alignment_ms"`
	Digests             []PayloadDigest `json:"digests"`
}
