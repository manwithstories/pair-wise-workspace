// Provenance records: a reviewable trace of one run.
//
// Each run appends a JSON object to provenance.jsonl. The record binds the
// input fingerprint, the adapter versions, the effective configuration, the
// output digest and every error with its byte offset and track, so an
// operator can answer "where did this output come from" without re-running.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"mediacore/internal/media"
)

// ProvenanceVersion is the record schema revision.
const ProvenanceVersion = 1

// ProvenancePath returns the provenance log path for a work directory.
func ProvenancePath(dir string) string { return filepath.Join(dir, "provenance.jsonl") }

// ErrorRecord is the persisted form of one attributed failure.
type ErrorRecord struct {
	Code   string `json:"error_code"`
	Offset int64  `json:"offset"`
	HasOff bool   `json:"has_offset"`
	Track  int    `json:"track"`
	Stage  string `json:"stage,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Provenance is one run's immutable record.
type Provenance struct {
	Version          int           `json:"version"`
	RecordedAt       string        `json:"recorded_at"`
	Mode             string        `json:"mode"`
	InputPath        string        `json:"input_path"`
	InputSHA256      string        `json:"input_sha256"`
	InputBytes       int64         `json:"input_bytes"`
	ContainerVersion string        `json:"container_version"`
	CodecVersion     string        `json:"codec_version"`
	ConfigDigest     string        `json:"config_digest"`
	Params           Params        `json:"params"`
	Output           OutputSummary `json:"output"`
	Errors           []ErrorRecord `json:"errors,omitempty"`
	ResumedFromBlock int           `json:"resumed_from_block"`
	RecoveryMS       int64         `json:"recovery_ms"`
	ExitCode         int           `json:"exit_code"`
	Note             string        `json:"note,omitempty"`
}

// Params is the effective, fully-defaulted configuration.
type Params struct {
	Rate           int     `json:"rate"`
	Channels       int     `json:"channels"`
	LoudnessDBFS   float64 `json:"loudness_dbfs"`
	BlockFrames    int     `json:"block_frames"`
	InterruptAfter int     `json:"interrupt_after"`
}

// OutputSummary describes the produced artefacts.
type OutputSummary struct {
	Path       string  `json:"path"`
	SHA256     string  `json:"sha256"`
	SamplesIn  int64   `json:"samples_in"`
	FramesOut  int64   `json:"frames_out"`
	SamplesOut int64   `json:"samples_out"`
	Blocks     int     `json:"blocks"`
	PeakDBFS   float64 `json:"peak_dbfs"`
	TotalBytes int64   `json:"total_bytes"`
}

// Append serialises p as one line and appends it atomically enough for a
// local work directory. A partially written final line is detectable because
// every reader decodes line by line.
func Append(dir string, p *Provenance) error {
	p.Version = ProvenanceVersion
	if p.RecordedAt == "" {
		p.RecordedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	line, err := json.Marshal(p)
	if err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "encode provenance: %v", err).Wrap(err)
	}
	line = append(line, '\n')
	f, err := os.OpenFile(ProvenancePath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "open provenance: %v", err).Wrap(err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "write provenance: %v", err).Wrap(err)
	}
	if err := f.Sync(); err != nil {
		return media.NewError(media.CodeAtomicWrite, -1, 0, "sync provenance: %v", err).Wrap(err)
	}
	return nil
}

// ReadAll returns every provenance record, in order. Malformed trailing
// lines are skipped rather than failing the read, because the last line of
// an interrupted process may be torn.
func ReadAll(dir string) ([]Provenance, error) {
	data, err := os.ReadFile(ProvenancePath(dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, media.NewError(media.CodeUsage, -1, 0, "read provenance: %v", err).Wrap(err)
	}
	var out []Provenance
	start := 0
	for i := 0; i <= len(data); i++ {
		if i < len(data) && data[i] != '\n' {
			continue
		}
		line := data[start:i]
		start = i + 1
		if len(line) == 0 {
			continue
		}
		var p Provenance
		if err := json.Unmarshal(line, &p); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ErrorsFrom converts engine errors to records attributable in provenance.
func ErrorsFrom(errs []error) []ErrorRecord {
	var recs []ErrorRecord
	for _, err := range errs {
		if err == nil {
			continue
		}
		if me, ok := media.AsError(err); ok {
			recs = append(recs, ErrorRecord{
				Code: string(me.Code), Offset: me.Offset, HasOff: me.HasOffset,
				Track: me.Track, Stage: me.Stage, Detail: me.Message,
			})
			continue
		}
		recs = append(recs, ErrorRecord{Code: "UNCLASSIFIED", Detail: err.Error()})
	}
	return recs
}
