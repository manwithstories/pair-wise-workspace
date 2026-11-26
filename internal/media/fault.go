package media

import "fmt"

// Severity values distinguish fatal structural corruption from recoverable
// skip/notices.
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityInfo    = "info"
)

// Stable fault codes. Demo assertions and tests match these strings.
const (
	CodeTruncatedBox       = "truncated_box"
	CodeBoxSizeInvalid     = "box_size_invalid"
	CodeBoxDepthExceeded   = "box_depth_exceeded"
	CodeIntegerOverflow    = "integer_overflow"
	CodeBoxOutsideParent   = "box_outside_parent"
	CodeMDATPastEOF        = "mdat_past_eof"
	CodeSampleOffsetOOB    = "sample_offset_out_of_bounds"
	CodeSampleOutsideMDAT  = "sample_outside_mdat"
	CodeTfdtRegression     = "tfdt_regression"
	CodeZeroTimescale      = "zero_timescale"
	CodeUnknownBoxSkipped  = "unknown_box_skipped"
	CodeMissingSampleTable = "missing_sample_table"
	CodeCheckpointMismatch = "checkpoint_fingerprint_mismatch"
	CodeInputPathUnsafe    = "input_path_unsafe"
	CodeFileTooLarge       = "file_too_large"
)

// Fault is the single error record used by parser, analyzer, pipeline, and
// report layers. Every persisted fault carries provenance and location fields.
type Fault struct {
	Code       string `json:"code"`
	Severity   string `json:"severity"`
	Message    string `json:"message"`
	FileSHA256 string `json:"file_sha256"`
	File       string `json:"file,omitempty"`
	TrackID    uint32 `json:"track_id"`
	ByteOffset int64  `json:"byte_offset"`
	// MediaTime is always present. For container-level faults it is the zero
	// time; zero-timescale faults retain Timescale=0 explicitly.
	MediaTime Time   `json:"media_time"`
	Details   string `json:"details,omitempty"`
}

// Error implements the error interface.
func (f Fault) Error() string {
	where := fmt.Sprintf("%s at offset %d", f.Code, f.ByteOffset)
	if f.TrackID != 0 {
		where += fmt.Sprintf(" track=%d", f.TrackID)
	}
	if f.Message != "" {
		return where + ": " + f.Message
	}
	return where
}

// NewFault creates a fully populated fault.
func NewFault(code, severity, hash, file string, trackID uint32, offset int64, t Time, format string, args ...any) Fault {
	return Fault{
		Code:       code,
		Severity:   severity,
		Message:    fmt.Sprintf(format, args...),
		FileSHA256: hash,
		File:       file,
		TrackID:    trackID,
		ByteOffset: offset,
		MediaTime:  t,
	}
}

// IsCorruptRegion reports whether code is one of the four damage classes used
// by the demo's corrupt_regions metric.
func IsCorruptRegion(code string) bool {
	switch code {
	case CodeTruncatedBox, CodeMDATPastEOF, CodeSampleOffsetOOB, CodeTfdtRegression:
		return true
	default:
		return false
	}
}
