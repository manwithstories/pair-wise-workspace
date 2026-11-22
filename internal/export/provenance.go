package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"medpipe/internal/fault"
	"medpipe/internal/merge"
)

// FrameRecord is one provenance line: which input produced this frame,
// where it sits on the timeline, how many packets were missing before
// it, and the frame's content hash.
type FrameRecord struct {
	Type      string  `json:"type"` // "frame"
	Index     uint64  `json:"index"`
	Track     uint32  `json:"track"`
	ExtSeq    uint64  `json:"ext_seq"`
	Timestamp string  `json:"timestamp"` // exact shared time as "N/D"
	Seconds   float64 `json:"seconds"`
	SrcOffset int64   `json:"src_offset"`
	Missing   uint32  `json:"missing"`
	SHA256    string  `json:"sha256"`
}

// AnomalyRecord mirrors a cross-track calibration fault into the
// manifest so reviewers see it in both places.
type AnomalyRecord struct {
	Kind   string `json:"kind"`
	Track  int64  `json:"track"`
	Seq    int64  `json:"seq"`
	Offset int64  `json:"offset"`
	Msg    string `json:"msg"`
}

// ManifestHeader is the first line of provenance.jsonl.
type ManifestHeader struct {
	Type           string           `json:"type"` // "header"
	InputSHA256    string           `json:"input_sha256"`
	ParamsSHA256   string           `json:"params_sha256"`
	Anomalies      []AnomalyRecord  `json:"anomalies"`
}

// NewFrameRecord builds the record for one encoded frame.
func NewFrameRecord(index uint64, ev *merge.Event, frame *[FrameSize]byte) FrameRecord {
	sum := sha256.Sum256(frame[:])
	return FrameRecord{
		Type:      "frame",
		Index:     index,
		Track:     ev.Track,
		ExtSeq:    ev.ExtSeq,
		Timestamp: ev.Shared.String(),
		Seconds:   ev.Shared.Float(),
		SrcOffset: ev.Offset,
		Missing:   ev.MissingBefore,
		SHA256:    hex.EncodeToString(sum[:]),
	}
}

// AnomaliesFromFaults converts located faults into manifest anomalies.
func AnomaliesFromFaults(errs []*fault.Error) []AnomalyRecord {
	out := make([]AnomalyRecord, 0, len(errs))
	for _, e := range errs {
		out = append(out, AnomalyRecord{
			Kind:   e.Kind.String(),
			Track:  e.Track,
			Seq:    e.Seq,
			Offset: e.Offset,
			Msg:    e.Msg,
		})
	}
	return out
}

// MarshalFrameRecord encodes one manifest line.
func MarshalFrameRecord(r FrameRecord) []byte {
	b, _ := json.Marshal(r)
	return append(b, '\n')
}

// MarshalHeader encodes the manifest header line.
func MarshalHeader(h ManifestHeader) []byte {
	b, _ := json.Marshal(h)
	return append(b, '\n')
}

// Manifest is the machine-readable run summary stored as manifest.json
// inside the output directory; verify checks every entry.
type Manifest struct {
	InputSHA256  string            `json:"input_sha256"`
	ParamsSHA256 string            `json:"params_sha256"`
	Segments     []Segment         `json:"segments"`
	Outputs      map[string]string `json:"outputs"` // relpath -> sha256
	Metrics      Metrics           `json:"metrics"`
	TrackTiming  []TrackTiming     `json:"track_timing"`
}

// String prints one manifest line for logs.
func (m *Manifest) String() string {
	return fmt.Sprintf("input=%s frames=%d", m.InputSHA256[:12], m.Metrics.FramesWritten)
}
