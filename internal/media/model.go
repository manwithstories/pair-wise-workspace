// Package media defines the shared media time base, stream descriptors and the
// unified error model used by every pipeline stage.
package media

import (
	"errors"
	"fmt"
)

// Time90 is a media timestamp expressed in 90 kHz ticks. Values are extended
// past the 33-bit MPEG-TS wrap point by the timeline package, so a monotonic
// Time90 stream can span far longer than the ~26.5 hour raw modulo range.
type Time90 int64

// TicksPerSecond is the 90 kHz MPEG clock rate.
const TicksPerSecond Time90 = 90_000

// Seconds converts a tick duration to seconds as a float.
func (t Time90) Seconds() float64 { return float64(t) / float64(TicksPerSecond) }

// Milliseconds converts a tick duration to milliseconds as a float.
func (t Time90) Milliseconds() float64 { return float64(t) * 1000.0 / float64(TicksPerSecond) }

// StreamKind classifies an elementary stream carried inside PES packets.
type StreamKind uint8

const (
	KindUnknown StreamKind = iota
	KindVideoH264
	KindAudioPCM16LE
	// KindAudioUnsupported marks audio codecs this pipeline does not decode.
	// Such streams must be reported, never treated as silence.
	KindAudioUnsupported
)

// ErrorCode is the stable classification used by the unified error model.
type ErrorCode int

const (
	EInternal ErrorCode = iota
	EUnrecoverable // input corruption that cannot be healed, exit code 2
	EResourceLimit // hard queue/buffer limit reached, exit code 3
	ECancelled     // pipeline cancelled, exit code 3
	EUnsupportedCodec
	ECRCReject // PSI section failed CRC, previous good version retained
	ECCGap     // transport continuity counter discontinuity
	ETruncated // input ended in the middle of a packet/section
	EClockGap  // timestamp jump that cannot be unwrapped
)

// Error is the single error type shared by parser, timeline, segmenter and
// packager. Callers classify failures with errors.As/Is instead of parsing
// strings.
type Error struct {
	Code ErrorCode
	Op   string // short operation tag, e.g. "psi.parse"
	Msg  string
	Err  error // wrapped cause, may be nil
}

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Is reports equality by error code, so errors.Is(err, ErrUnrecoverable)
// works on any *Error carrying the same classification.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// Sentinel errors for classification. Construct contextual failures with the
// E* helpers below, then compare against these via errors.Is.
var (
	ErrInternal         = &Error{Code: EInternal}
	ErrUnrecoverable    = &Error{Code: EUnrecoverable}
	ErrResourceLimit    = &Error{Code: EResourceLimit}
	ErrCancelled        = &Error{Code: ECancelled}
	ErrUnsupportedCodec = &Error{Code: EUnsupportedCodec}
	ErrCRCReject        = &Error{Code: ECRCReject}
	ErrCCGap            = &Error{Code: ECCGap}
	ErrTruncated        = &Error{Code: ETruncated}
	ErrClockGap         = &Error{Code: EClockGap}
)

// Wrap builds a contextual *Error. code is one of the E* ErrorCode values.
func Wrap(code ErrorCode, op, msg string, cause error) *Error {
	return &Error{Code: code, Op: op, Msg: msg, Err: cause}
}

// CodeOf extracts the ErrorCode of err, defaulting to EInternal.
func CodeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return EInternal
}

// AccessUnit is one decoded video frame or one audio PES payload, stamped in
// the extended 90 kHz timeline. Buffers are owned by the receiver.
type AccessUnit struct {
	PID    uint16
	Kind   StreamKind
	PTS    Time90 // extended, valid when HasPTS
	DTS    Time90 // extended, valid when HasDTS
	HasPTS bool
	HasDTS bool
	// Data holds Annex B H.264 bytes for video, raw PCM16LE little-endian
	// samples for PCM audio.
	Data []byte
	// ClockGap marks units whose timestamp could not be proven monotonic;
	// downstream consumers must not fabricate a time for them.
	ClockGap bool
}

// IsKeyframe reports whether a video access unit starts with an IDR slice
// (NAL type 5) or SPS/PPS (types 7/8) that conventionally accompanies one.
func (u *AccessUnit) IsKeyframe() bool {
	if u.Kind != KindVideoH264 || len(u.Data) == 0 {
		return false
	}
	for _, nal := range SplitAnnexB(u.Data) {
		if len(nal) > 0 && (nal[0]&0x1f) == 5 {
			return true
		}
	}
	return false
}

// SplitAnnexB cuts an Annex B byte stream into individual NAL units, dropping
// start-code prefixes and zero padding. It lives in the media package because
// both the TS parser and the segmenter need identical framing.
func SplitAnnexB(b []byte) [][]byte {
	var starts []int
	n := len(b)
	for i := 0; i+3 <= n; {
		if b[i] == 0 && b[i+1] == 0 {
			if b[i+2] == 1 {
				starts = append(starts, i+3)
				i += 3
				continue
			}
			if i+3 < n && b[i+2] == 0 && b[i+3] == 1 {
				starts = append(starts, i+4)
				i += 4
				continue
			}
		}
		i++
	}
	out := make([][]byte, 0, len(starts))
	for i, s := range starts {
		end := n
		if i+1 < len(starts) {
			end = starts[i+1]
			// Remove the trailing zero bytes that belong to the next prefix.
			prefix := b[s:end]
			for j := len(prefix) - 1; j >= 0 && prefix[j] == 0; j-- {
				end--
			}
		}
		if end > s {
			out = append(out, b[s:end])
		}
	}
	return out
}
