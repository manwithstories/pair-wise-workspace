// Package provenance builds the deterministic provenance record of a
// scan: input digest, config digest, per-segment hashes, time ranges,
// errors, and the shared damage ranges. The record's SHA-256 is the
// stable report fingerprint across resumes and reruns.
package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"mediascan/internal/core"
	"mediascan/internal/index"
	"mediascan/internal/pipeline"
)

// ToolVersion identifies the record producer.
const ToolVersion = "mediascan/1.0"

// SegmentRecord is the provenance entry for one segment.
type SegmentRecord struct {
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	State     string   `json:"state"`
	SHA256    string   `json:"sha256,omitempty"`
	Start     core.Rat `json:"start"`
	End       core.Rat `json:"end"`
	DiscSeq   int      `json:"disc_seq"`
	Keyframes []int    `json:"keyframes,omitempty"`
	Gaps      []int    `json:"gaps,omitempty"`
	Errors    []string `json:"errors,omitempty"`
}

// Record is the full provenance record (also written as report.json).
type Record struct {
	Tool           string          `json:"tool"`
	InputDigest    string          `json:"input_digest"`
	ConfigDigest   string          `json:"config_digest"`
	RoundTripOK    int             `json:"roundtrip_ok"`
	RoundTripTotal int             `json:"roundtrip_total"`
	TimelineErrors []core.Range    `json:"timeline_errors"`
	Damage         []core.Range    `json:"damage"`
	Segments       []SegmentRecord `json:"segments"`
}

// Build assembles the record from the index, the terminal checkpoint
// and the merged damage ranges. Segment order follows the index.
func Build(idx *index.Index, cp *pipeline.Checkpoint, damage []core.Range) *Record {
	rec := &Record{
		Tool:           ToolVersion,
		InputDigest:    idx.InputDigest,
		ConfigDigest:   idx.ConfigDigest,
		RoundTripOK:    idx.RoundTripOK,
		RoundTripTotal: idx.RoundTripTotal,
		TimelineErrors: idx.TimelineErrors,
		Damage:         damage,
	}
	if rec.TimelineErrors == nil {
		rec.TimelineErrors = []core.Range{}
	}
	if rec.Damage == nil {
		rec.Damage = []core.Range{}
	}
	for _, s := range idx.Segments {
		sr := SegmentRecord{
			ID: s.ID, Path: s.Path, DiscSeq: s.DiscSeq,
			Start: s.Start, End: s.End,
			State: string(pipeline.StatePending),
		}
		if res := cp.Results[s.ID]; res != nil {
			sr.State = string(res.State)
			sr.SHA256 = res.SHA256
			sr.Keyframes = res.Keyframes
			sr.Gaps = res.Gaps
			sr.Errors = res.Errors
		}
		rec.Segments = append(rec.Segments, sr)
	}
	return rec
}

// Canonical returns the deterministic JSON encoding of the record.
func (r *Record) Canonical() []byte {
	b, _ := json.Marshal(r)
	return b
}

// SHA256 is the stable report fingerprint.
func (r *Record) SHA256() string {
	sum := sha256.Sum256(r.Canonical())
	return hex.EncodeToString(sum[:])
}
