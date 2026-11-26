package media

import (
	"errors"
	"fmt"
)

// Code is a stable machine-readable fault category.
type Code string

const (
	CodeInvalidInput        Code = "invalid_input"
	CodeBoxBounds           Code = "box_bounds"
	CodeTooManyTracks       Code = "too_many_tracks"
	CodeFileTooLarge        Code = "file_too_large"
	CodeTimestampRegression Code = "timestamp_regression"
	CodeTimestampMissing    Code = "timestamp_missing"
	CodeTimestampWrap       Code = "timestamp_wrap"
	CodeEditListConflict    Code = "edit_list_conflict"
	CodeBadSampleOffset     Code = "bad_sample_offset"
	CodeTruncatedMDAT       Code = "truncated_mdat"
	CodeMissingIndex        Code = "missing_index"
	CodeAudioLayout         Code = "audio_layout_conflict"
	CodeAudioRate           Code = "audio_rate_conflict"
	CodeColorConflict       Code = "color_metadata_conflict"
	CodeUnrecoverableTrack  Code = "unrecoverable_track"
	CodeIndexIncomplete     Code = "index_incomplete"
	CodeBoundaryMismatch    Code = "boundary_mismatch"
	CodeRecoveryFailed      Code = "recovery_failed"
	CodeMetricThreshold     Code = "metric_threshold"
	CodeInternal            Code = "internal_error"
)

// ByteRange identifies an inclusive-start byte interval in the source file.
// Length zero points at a single byte.
type ByteRange struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

// End returns the exclusive end and false when the interval overflows int64.
func (r ByteRange) End() (int64, bool) {
	if r.Offset < 0 || r.Length < 0 {
		return 0, false
	}
	if r.Length > 1<<62 || r.Offset > (1<<62) {
		return 0, false
	}
	end := r.Offset + r.Length
	if end < r.Offset {
		return 0, false
	}
	return end, true
}

// TimeSpan is an interval on a track or shared clock.
type TimeSpan struct {
	StartTicks int64 `json:"start_ticks"`
	EndTicks   int64 `json:"end_ticks"`
	Clock      Clock `json:"clock"`
}

// DurationNS returns the span duration in nanoseconds.
func (s TimeSpan) DurationNS() (int64, error) {
	return AbsDiffNS(s.StartTicks, s.EndTicks, s.Clock)
}

// Fault is the structured error model threaded through all pipeline stages.
type Fault struct {
	Code        Code       `json:"code"`
	TrackID     uint32     `json:"track_id,omitempty"`
	BoxType     string     `json:"box_type,omitempty"`
	ByteRange   *ByteRange `json:"byte_range,omitempty"`
	TimeSpan    *TimeSpan  `json:"time_span,omitempty"`
	Message     string     `json:"message"`
	Original    string     `json:"original,omitempty"`
	RepairBasis string     `json:"repair_basis,omitempty"`
	Recoverable bool       `json:"recoverable"`
	Cause       error      `json:"-"`
}

func (f *Fault) Error() string {
	where := ""
	if f.TrackID != 0 {
		where = fmt.Sprintf(" track=%d", f.TrackID)
	}
	if f.BoxType != "" {
		where += " box=" + f.BoxType
	}
	if f.ByteRange != nil {
		where += fmt.Sprintf(" byte=%d", f.ByteRange.Offset)
	}
	return fmt.Sprintf("%s%s: %s", f.Code, where, f.Message)
}

func (f *Fault) Unwrap() error { return f.Cause }

// NewFault constructs a recoverable-by-default structured fault.
func NewFault(code Code, format string, args ...any) *Fault {
	return &Fault{Code: code, Message: fmt.Sprintf(format, args...), Recoverable: true}
}

// WithTrack attaches a track ID.
func (f *Fault) WithTrack(id uint32) *Fault { f.TrackID = id; return f }

// WithBox attaches a four-character box type.
func (f *Fault) WithBox(box string) *Fault { f.BoxType = box; return f }

// WithByteRange attaches a byte interval.
func (f *Fault) WithByteRange(offset, length int64) *Fault {
	f.ByteRange = &ByteRange{Offset: offset, Length: length}
	return f
}

// WithTimeSpan attaches a time interval.
func (f *Fault) WithTimeSpan(start, end int64, clock Clock) *Fault {
	f.TimeSpan = &TimeSpan{StartTicks: start, EndTicks: end, Clock: clock}
	return f
}

// WithOriginal records the original conflicting value.
func (f *Fault) WithOriginal(value string) *Fault { f.Original = value; return f }

// WithBasis records the evidence used for repair or failure.
func (f *Fault) WithBasis(basis string) *Fault { f.RepairBasis = basis; return f }

// Fatal marks the fault as not safely recoverable.
func (f *Fault) Fatal() *Fault { f.Recoverable = false; return f }

// AsFault extracts a *Fault from an error chain.
func AsFault(err error) (*Fault, bool) {
	var f *Fault
	if errors.As(err, &f) {
		return f, true
	}
	return nil, false
}

// AsFaultOrInternal extracts a *Fault or wraps an internal error.
func AsFaultOrInternal(err error) *Fault {
	if f, ok := AsFault(err); ok {
		return f
	}
	return NewFault(CodeInternal, "%v", err).Fatal()
}
