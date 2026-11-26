package media

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// Rat is an exact rational time base. FFmpeg's AVRational uses int fields,
// while pipeline arithmetic uses int64 to avoid intermediate overflow.
type Rat struct {
	N int64 `json:"num"`
	D int64 `json:"den"`
}

func NewRat(num, den int64) Rat {
	if den == 0 {
		return Rat{N: 0, D: 1}
	}
	if den < 0 {
		num, den = -num, -den
	}
	g := gcd(abs64(num), den)
	if g == 0 {
		return Rat{N: 0, D: 1}
	}
	return Rat{N: num / g, D: den / g}
}

func (r Rat) Valid() bool { return r.D > 0 }

func (r Rat) Float64() float64 {
	if r.D == 0 {
		return 0
	}
	return float64(r.N) / float64(r.D)
}

func (r Rat) Seconds(ts int64) float64 {
	if r.D == 0 {
		return 0
	}
	return float64(ts) * float64(r.N) / float64(r.D)
}

// Rescale converts a timestamp between time bases using nearest rounding.
func Rescale(ts int64, from, to Rat) (int64, error) {
	if from.D == 0 || to.D == 0 || to.N == 0 {
		return 0, NewError(CodeInvalidTimeBase, "invalid rational time base", nil)
	}
	// ts * from.N / from.D, then / to.Float and * to.D/to.N.
	num := bigMulDiv(ts*from.N, to.D, from.D*to.N)
	return num, nil
}

// FrameDuration returns the duration of one frame/access unit in seconds.
func FrameDuration(frameRate Rat) float64 {
	if frameRate.N == 0 {
		return 0
	}
	return float64(frameRate.D) / float64(frameRate.N)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func abs64(v int64) int64 {
	if v == math.MinInt64 {
		return math.MaxInt64
	}
	if v < 0 {
		return -v
	}
	return v
}

// bigMulDiv performs round(a*b/c) using arbitrary precision intermediates.
func bigMulDiv(a, b, c int64) int64 {
	if c == 0 {
		return 0
	}
	q := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	d := big.NewInt(c)
	num, rem := new(big.Int), new(big.Int)
	num.QuoRem(q, d, rem)
	half := new(big.Int).Rsh(d, 1)
	if rem.Sign() < 0 {
		if new(big.Int).Abs(rem).Cmp(half) >= 0 {
			num.Sub(num, big.NewInt(1))
		}
	} else if rem.Cmp(half) >= 0 {
		num.Add(num, big.NewInt(1))
	}
	return num.Int64()
}

type ErrorCode string

const (
	CodeInvalidInput       ErrorCode = "invalid_input"
	CodeInvalidTimeBase    ErrorCode = "invalid_timebase"
	CodePathTraversal      ErrorCode = "path_traversal"
	CodeTooLarge           ErrorCode = "input_too_large"
	CodeTimestampRollback  ErrorCode = "timestamp_rollback"
	CodeTruncatedMoov      ErrorCode = "truncated_moov"
	CodeDemux              ErrorCode = "demux_error"
	CodeDecode             ErrorCode = "decode_error"
	CodeUnrecoverableLoss  ErrorCode = "unrecoverable_packet_loss"
	CodeChecksumMismatch   ErrorCode = "checksum_mismatch"
	CodeSourceMismatch     ErrorCode = "source_mismatch"
	CodeIO                 ErrorCode = "io_error"
	CodeCheckpoint         ErrorCode = "checkpoint_error"
	CodeEncode             ErrorCode = "encode_error"
	CodeUnsupportedFeature ErrorCode = "unsupported_feature"
)

// Error is the single error envelope used by terminal output and exit mapping.
type Error struct {
	Code     ErrorCode `json:"error_code"`
	Exit     int       `json:"exit_code"`
	Path     string    `json:"path,omitempty"`
	Stream   int       `json:"stream_index,omitempty"`
	Position int64     `json:"position,omitempty"`
	Op       string    `json:"op,omitempty"`
	Err      error     `json:"-"`
}

func (e *Error) Error() string {
	var msg string
	if e.Err != nil {
		msg = e.Err.Error()
	} else {
		msg = string(e.Code)
	}
	where := ""
	if e.Stream >= 0 || e.Position > 0 {
		where = fmt.Sprintf(" stream=%d position=%d", e.Stream, e.Position)
	}
	if e.Op != "" {
		return fmt.Sprintf("%s: %s%s: %s", e.Op, e.Code, where, msg)
	}
	return fmt.Sprintf("%s%s: %s", e.Code, where, msg)
}

func (e *Error) Unwrap() error { return e.Err }
func (e *Error) ExitCode() int {
	if e.Exit != 0 {
		return e.Exit
	}
	return ExitForCode(e.Code)
}

func NewError(code ErrorCode, msg string, cause error) *Error {
	return &Error{Code: code, Exit: ExitForCode(code), Err: errors.New(msg), Stream: -1}
}

func WrapError(code ErrorCode, op string, cause error) *Error {
	return &Error{Code: code, Exit: ExitForCode(code), Op: op, Err: cause, Stream: -1}
}

func TimestampError(stream int, position int64, detail string) *Error {
	return &Error{
		Code: CodeTimestampRollback, Exit: 2, Stream: stream, Position: position,
		Op: "validate_timestamps", Err: errors.New(detail),
	}
}

func ExitForCode(code ErrorCode) int {
	switch code {
	case CodeUnrecoverableLoss, CodeChecksumMismatch, CodeSourceMismatch:
		return 3
	case CodeIO, CodeCheckpoint:
		return 4
	case CodeTruncatedMoov, CodeTimestampRollback, CodeDemux, CodeDecode, CodeInvalidInput, CodeTooLarge:
		return 2
	default:
		return 1
	}
}

func AsError(err error) (*Error, bool) {
	var me *Error
	if errors.As(err, &me) {
		return me, true
	}
	return nil, false
}
