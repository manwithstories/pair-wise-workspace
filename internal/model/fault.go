package model

import "fmt"

// Fault codes emitted by mediq stages. They are stable strings so terminal
// output and tests can match on them.
const (
	CodeTruncatedHeader   = "truncated_header"
	CodeTruncatedPayload  = "truncated_payload"
	CodeBadMagic          = "bad_magic"
	CodeBadHeader         = "bad_header"
	CodeOversizeFrame     = "oversize_frame"
	CodeOversizeMetadata  = "oversize_metadata"
	CodeBadTimebase       = "bad_timebase"
	CodeMetadataMismatch  = "metadata_mismatch"
	CodeHashMismatch      = "hash_mismatch"
	CodeTimestampWrap     = "timestamp_wrap"
	CodeDuplicateSeq      = "duplicate_seq"
	CodeOutOfOrder        = "out_of_order"
	CodeUnrepairableTime  = "unrepairable_timestamp"
	CodeQuarantined       = "quarantined_frame"
	CodeIndexMismatch     = "index_mismatch"
	CodeCheckpointInvalid = "checkpoint_invalid"
)

// Fault is a diagnosable problem anchored to a frame and a byte offset in the
// source record stream. It doubles as an error value.
type Fault struct {
	Code     string
	Seq      uint64
	HasSeq   bool
	Offset   int64
	Expected string
	Actual   string
	Reason   string
	Err      error
}

func (f *Fault) Error() string {
	s := f.Code
	if f.HasSeq {
		s += fmt.Sprintf(" frame=%d", f.Seq)
	}
	s += fmt.Sprintf(" offset=%d", f.Offset)
	if f.Expected != "" || f.Actual != "" {
		s += fmt.Sprintf(" expected=%s actual=%s", f.Expected, f.Actual)
	}
	if f.Reason != "" {
		s += " reason=" + f.Reason
	}
	if f.Err != nil {
		s += ": " + f.Err.Error()
	}
	return s
}

func (f *Fault) Unwrap() error { return f.Err }

// Resync reports whether the reader may continue scanning after this fault.
// Structural failures (unknown magic, bad version) break framing and cannot
// be resynchronised without an explicit record boundary.
func (f *Fault) Resync() bool {
	switch f.Code {
	case CodeBadMagic, CodeBadHeader, CodeTruncatedHeader:
		return false
	}
	return true
}
