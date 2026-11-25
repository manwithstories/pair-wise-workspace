// Package verify confirms hashes and serialization round trips before publish.
package verify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"mediaproc/internal/model"
	"mediaproc/internal/segment"
)

// Record is the deterministic JSON sidecar for one published segment.
type Record struct {
	SegmentIndex   int      `json:"segment_index"`
	StartNS        int64    `json:"start_ns"`
	EndNS          int64    `json:"end_ns"`
	KeyframeIndex  int      `json:"keyframe_index"`
	KeyframeOffset int64    `json:"keyframe_offset"`
	SHA256         string   `json:"sha256"`
	BlockCount     int      `json:"block_count"`
	SilentWindows  int      `json:"silent_windows"`
	Fingerprints   []string `json:"fingerprints,omitempty"`
	MP4Validated   bool     `json:"mp4_validated"`
}

// RecordFromSegment creates a serializable verification record.
func RecordFromSegment(s *segment.Segment) Record {
	r := Record{
		SegmentIndex:   s.Index,
		StartNS:        int64(s.StartNS),
		EndNS:          int64(s.EndNS),
		KeyframeIndex:  s.KeyframeIndex,
		KeyframeOffset: s.KeyframeOffset,
		SHA256:         s.SHA256,
		BlockCount:     len(s.Blocks),
		SilentWindows:  s.SilentWindows,
		Fingerprints:   append([]string(nil), s.Fingerprints...),
	}
	return r
}

// HashBytes returns the hex SHA-256 of data.
func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// VerifySegmentHash recomputes the ordered segment hash.
func VerifySegmentHash(s *segment.Segment) error {
	if s == nil {
		return model.InvariantError(model.PhaseVerify, -1, "nil segment", nil)
	}
	got := segment.Hash(s)
	if got != s.SHA256 {
		return model.InvariantError(model.PhaseVerify, s.KeyframeOffset,
			fmt.Sprintf("segment %d hash mismatch: recorded=%s recomputed=%s",
				s.Index, s.SHA256, got), nil)
	}
	return nil
}

// HashResult summarizes segment hash verification.
type HashResult struct {
	Total int
	OK    int
}

// VerifySegments checks every segment hash.
func VerifySegments(segments []*segment.Segment) (HashResult, error) {
	res := HashResult{Total: len(segments)}
	for _, s := range segments {
		if err := VerifySegmentHash(s); err != nil {
			return res, err
		}
		res.OK++
	}
	return res, nil
}

// RoundtripRecords JSON-encodes and decodes records, ensuring stable fields.
func RoundtripRecords(records []Record) ([]Record, error) {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return nil, model.InvariantError(model.PhaseVerify, -1, "marshal records", err)
	}
	var decoded []Record
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&decoded); err != nil {
		return nil, model.InvariantError(model.PhaseVerify, -1, "decode records", err)
	}
	if len(decoded) != len(records) {
		return nil, model.InvariantError(model.PhaseVerify, -1,
			fmt.Sprintf("record count changed: %d != %d", len(decoded), len(records)), nil)
	}
	for i := range records {
		if !reflect.DeepEqual(decoded[i], records[i]) {
			return nil, model.InvariantError(model.PhaseVerify, records[i].KeyframeOffset,
				fmt.Sprintf("record %d failed JSON roundtrip", i), nil)
		}
	}
	return decoded, nil
}

// RoundtripPercent returns OK/Total as the fixed two-decimal percentage.
func RoundtripPercent(ok, total int) string {
	if total == 0 {
		return "100.00%"
	}
	return fmt.Sprintf("%.2f%%", 100*float64(ok)/float64(total))
}
