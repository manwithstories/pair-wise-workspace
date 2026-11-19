// Package provenance records, for every index entry, which source
// packets, repair rules, time base and parameters produced it, and
// supports querying by track and time interval.
package provenance

import (
	"encoding/json"
	"os"
)

// Entry is one random-access index entry.
type Entry struct {
	Index          int    `json:"index"`
	Fragment       uint32 `json:"fragment"`
	TrackID        uint32 `json:"track_id"`
	SSRC           uint32 `json:"ssrc"`
	Kind           string `json:"kind"` // "keyframe" or "sample-boundary"
	DecodeTimeNs   int64  `json:"decode_time_ns"`
	DecodeTimeUnit uint64 `json:"decode_time_units"`
	Timescale      uint32 `json:"timescale"`
	DurationNs     int64  `json:"duration_ns"`
	SrcFirstExtSeq uint64 `json:"src_first_ext_seq"`
	SrcLastExtSeq  uint64 `json:"src_last_ext_seq"`
	Repair         string `json:"repair"`    // none, reordered, loss-gap-before
	TimeBase       string `json:"time_base"` // rtcp-sr or estimated
	SHA256         string `json:"sha256"`    // digest of the fragment's sample data
}

// Interval marks a time range that is not randomly accessible.
type Interval struct {
	TrackID uint32 `json:"track_id"`
	SSRC    uint32 `json:"ssrc"`
	FromNs  int64  `json:"from_ns"`
	ToNs    int64  `json:"to_ns"`
	Reason  string `json:"reason"`
}

// ErrorEntry is one located fault in the manifest.
type ErrorEntry struct {
	Kind        string `json:"kind"`
	SSRC        uint32 `json:"ssrc"`
	ExtSeq      uint64 `json:"ext_seq"`
	DeviationNs int64  `json:"deviation_ns,omitempty"`
	Detail      string `json:"detail,omitempty"`
}

// Manifest is the full provenance record of one output.
type Manifest struct {
	InputSHA256     string            `json:"input_sha256"`
	ConfigSHA256    string            `json:"config_sha256"`
	Params          map[string]uint64 `json:"params"`
	OutputSHA256    string            `json:"output_sha256"`
	TimeBase        map[string]string `json:"time_base"` // ssrc -> rtcp-sr|estimated
	Entries         []Entry           `json:"entries"`
	NonRandomAccess []Interval        `json:"non_random_access"`
	Errors          []ErrorEntry      `json:"errors"`
	Recovered       bool              `json:"recovered"`
	ResumeMs        int64             `json:"resume_ms"`
}

// NewManifest returns an initialized manifest.
func NewManifest() *Manifest {
	return &Manifest{
		Params:   map[string]uint64{},
		TimeBase: map[string]string{},
	}
}

// Write saves the manifest atomically.
func Write(path string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load reads a manifest.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Query returns entries for a track overlapping [fromNs, toNs].
// TrackID 0 matches all tracks; a negative toNs means unbounded.
func (m *Manifest) Query(trackID uint32, fromNs, toNs int64) []Entry {
	var out []Entry
	for _, e := range m.Entries {
		if trackID != 0 && e.TrackID != trackID {
			continue
		}
		if e.DecodeTimeNs+e.DurationNs < fromNs {
			continue
		}
		if toNs >= 0 && e.DecodeTimeNs > toNs {
			continue
		}
		out = append(out, e)
	}
	return out
}
