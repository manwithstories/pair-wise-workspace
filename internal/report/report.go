// Package report emits run statistics as field-ordered JSON. The same
// input always produces byte-identical output.
package report

import (
	"encoding/json"
	"fmt"
	"os"
)

// Jitter summarizes interarrival delay variation in milliseconds.
type Jitter struct {
	Mean    float64 `json:"mean"`
	Max     float64 `json:"max"`
	P99     float64 `json:"p99"`
	Samples int     `json:"samples"`
}

// Report is the canonical statistics record. Field order in the struct
// defines the JSON key order.
type Report struct {
	CandidatePairs   int    `json:"candidate_pairs"`
	ForwardedPackets uint64 `json:"forwarded_packets"`
	DroppedPackets   uint64 `json:"dropped_packets"`
	JitterMs         Jitter `json:"jitter_ms"`
	Egress           int    `json:"egress"`
}

// Encode marshals r with stable field order.
func Encode(r Report) ([]byte, error) { return json.Marshal(r) }

// WriteFile writes the canonical JSON encoding of r to path.
func WriteFile(path string, r Report) error {
	b, err := Encode(r)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// ReadFile reads a report back from disk.
func ReadFile(path string) (Report, error) {
	var r Report
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("report: decode %s: %w", path, err)
	}
	return r, nil
}

// Validate checks internal consistency of the counters.
func Validate(r Report) error {
	if r.Egress < 0 {
		return fmt.Errorf("report: negative egress count")
	}
	if r.JitterMs.Samples < 0 {
		return fmt.Errorf("report: negative jitter sample count")
	}
	if r.DroppedPackets == 0 && r.ForwardedPackets > 0 && r.Egress > 0 {
		want := int((r.ForwardedPackets - 1) * uint64(r.Egress))
		if r.JitterMs.Samples != want {
			return fmt.Errorf("report: jitter samples %d, want %d", r.JitterMs.Samples, want)
		}
	}
	return nil
}
