package selftest

import (
	"encoding/json"
	"io"
)

// CorruptionRecord reports one corruption sample and the scanner's offset
// accuracy. BadOffsetDelta is always emitted with an explicit "B" suffix.
type CorruptionRecord struct {
	Event          string `json:"event"`
	Sample         string `json:"sample"`
	BadOffsetDelta string `json:"bad_offset_delta"`
	ExpectedOffset int64  `json:"expected_offset"`
	ReportedOffset int64  `json:"reported_offset"`
}

// SummaryRecord is the final JSONL line. Every acceptance field is present
// on this single record so downstream assertions have one stable target.
type SummaryRecord struct {
	Event          string `json:"event"`
	BadOffsetDelta string `json:"bad_offset_delta"`
	ResumeMillis   int64  `json:"resume_ms"`
	InflightBytes  int64  `json:"inflight_bytes"`
	SilenceMillis  int    `json:"silence_ms"`
	DedupHits      int    `json:"dedup_hits"`
	Segments       int    `json:"segments"`
	Sha256         string `json:"sha256"`
}

// WriteJSONL serializes one record as a single line.
func WriteJSONL(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
