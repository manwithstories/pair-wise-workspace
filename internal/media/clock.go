package media

import (
	"errors"
	"fmt"
)

// ParserVersion is persisted in provenance so recovered outputs can only be
// reused by a parser with identical semantics.
const ParserVersion = "avpipe-parser-1.0.0"

// PCRMod27 is the combined PCR wrap modulus in 27 MHz units: a 33-bit
// 90 kHz base multiplied by 300, plus the 27 MHz extension.
const PCRMod27 uint64 = 300 * (1 << 33)

// PTSMod90k is the 33-bit PTS/DTS wrap modulus in 90 kHz ticks.
const PTSMod90k uint64 = 1 << 33

// TickHz is the common presentation time scale used by alignment and HLS.
const TickHz uint64 = 90000

// ExitInterrupt is the deliberately requested checkpoint interruption code.
const ExitInterrupt = 75

// ErrorCode is the stable machine-readable failure class.
type ErrorCode string

const (
	CodeConfig      ErrorCode = "config_error"
	CodePath        ErrorCode = "unsafe_path"
	CodeSync        ErrorCode = "sync_lost"
	CodeIncomplete  ErrorCode = "incomplete_input"
	CodeNoPCRAnchor ErrorCode = "no_trusted_pcr_anchor"
	CodePSILength   ErrorCode = "psi_length_out_of_bounds"
	CodePSICRC      ErrorCode = "psi_crc_mismatch"
	CodeCCGap       ErrorCode = "continuity_counter_gap"
	CodeMissingIDR  ErrorCode = "missing_idr"
	CodeAlignment   ErrorCode = "alignment_gap"
	CodeCheckpoint  ErrorCode = "checkpoint_corrupt"
	CodeIO          ErrorCode = "io_error"
)

// Error is the shared error model crossing TS, index, alignment and pipeline
// boundaries. Recoverable describes whether a later valid unit can replace
// the rejected state.
type Error struct {
	Code        ErrorCode `json:"code"`
	Message     string    `json:"message"`
	Offset      int64     `json:"offset,omitempty"`
	Recoverable bool      `json:"recoverable"`
	Err         error     `json:"-"`
}

func (e *Error) Error() string {
	if e.Offset >= 0 {
		return fmt.Sprintf("%s at_offset=%d: %s", e.Code, e.Offset, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

func NewError(code ErrorCode, offset int64, format string, args ...any) *Error {
	recoverable := code == CodePSICRC || code == CodeCCGap ||
		code == CodeMissingIDR || code == CodeAlignment
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Offset: offset, Recoverable: recoverable}
}

func WrapError(code ErrorCode, offset int64, err error, format string, args ...any) *Error {
	return &Error{
		Code:        code,
		Message:     fmt.Sprintf(format, args...),
		Offset:      offset,
		Recoverable: code == CodePSICRC || code == CodeCCGap || code == CodeMissingIDR || code == CodeAlignment,
		Err:         err,
	}
}

// ExitCode maps the shared error class to the process contract.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var me *Error
	if errors.As(err, &me) {
		switch me.Code {
		case CodeConfig, CodePath:
			return 2
		case CodeCheckpoint, CodeIO:
			return 3
		default:
			if !me.Recoverable {
				return 3
			}
		}
	}
	return 3
}

// Epoch identifies a clock timeline separated by an explicit or implausible
// discontinuity. PCR/PTS wrap does not create an epoch.
type Epoch struct {
	ID          int    `json:"id"`
	StartOffset int64  `json:"start_offset"`
	StartTick   uint64 `json:"start_tick_90k"`
	PCRWraps    int    `json:"pcr_wraps"`
}

// Clock expands modulo PCR/PTS counters and isolates discontinuities.
type Clock struct {
	Epochs        []Epoch `json:"epochs"`
	epoch         int
	havePCR       bool
	lastPCRRaw    uint64
	pcrExpanded   uint64
	pcrWraps      int
	havePTS       bool
	lastPTSRaw    uint64
	ptsExpanded   uint64
	ptsWraps      int
	TrustedAnchor bool `json:"trusted_anchor"`
}

func NewClock() *Clock {
	c := &Clock{}
	c.Epochs = []Epoch{{ID: 0, StartOffset: -1}}
	return c
}

// FeedPCR accepts the combined 42-bit PCR in 27 MHz units.
func (c *Clock) FeedPCR(raw uint64, offset int64, discontinuity bool) (uint64, Epoch, error) {
	if raw >= PCRMod27 {
		return 0, Epoch{}, NewError(CodeConfig, offset, "pcr raw %d exceeds modulus", raw)
	}
	if !c.havePCR {
		c.havePCR = true
		c.TrustedAnchor = true
		c.lastPCRRaw = raw
		c.pcrExpanded = raw
		c.Epochs[0].StartOffset = offset
		c.Epochs[0].StartTick = raw / 300
		return c.pcrExpanded / 300, c.Epochs[0], nil
	}
	if discontinuity {
		c.epoch++
		c.pcrWraps = 0
		c.Epochs = append(c.Epochs, Epoch{ID: c.epoch, StartOffset: offset, StartTick: raw / 300})
		c.lastPCRRaw = raw
		// Keep globally expanded values monotonic while Epoch isolates semantics.
		c.pcrExpanded += PCRMod27 + raw
		return c.pcrExpanded / 300, c.Epochs[c.epoch], nil
	}
	delta, wrapped := modularDelta(raw, c.lastPCRRaw, PCRMod27)
	// A small modular backwards jump is a real discontinuity, not a wrap.
	if !wrapped && raw < c.lastPCRRaw && c.lastPCRRaw-raw > PCRMod27/2 {
		return c.newEpochPCR(raw, offset)
	}
	if wrapped {
		c.pcrWraps++
		c.Epochs[c.epoch].PCRWraps = c.pcrWraps
	}
	c.pcrExpanded += delta
	c.lastPCRRaw = raw
	return c.pcrExpanded / 300, c.Epochs[c.epoch], nil
}

func (c *Clock) newEpochPCR(raw uint64, offset int64) (uint64, Epoch, error) {
	c.epoch++
	c.pcrWraps = 0
	c.Epochs = append(c.Epochs, Epoch{ID: c.epoch, StartOffset: offset, StartTick: raw / 300})
	c.lastPCRRaw = raw
	c.pcrExpanded += PCRMod27
	return c.pcrExpanded / 300, c.Epochs[c.epoch], nil
}

// FeedPTS expands a 33-bit PTS value into monotonic 90 kHz ticks.
func (c *Clock) FeedPTS(raw uint64) (uint64, int, error) {
	if raw >= PTSMod90k {
		return 0, c.epoch, NewError(CodeConfig, -1, "pts raw %d exceeds modulus", raw)
	}
	if !c.havePTS {
		c.havePTS = true
		c.lastPTSRaw = raw
		c.ptsExpanded = raw
		return c.ptsExpanded, c.epoch, nil
	}
	delta, _ := modularDelta(raw, c.lastPTSRaw, PTSMod90k)
	c.ptsExpanded += delta
	c.lastPTSRaw = raw
	return c.ptsExpanded, c.epoch, nil
}

func modularDelta(curr, last, mod uint64) (delta uint64, wrapped bool) {
	if curr >= last {
		return curr - last, false
	}
	return curr + mod - last, true
}

// RequireAnchor marks alignment impossible when no PCR was ever received.
func (c *Clock) RequireAnchor() error {
	if c == nil || !c.TrustedAnchor {
		return NewError(CodeNoPCRAnchor, -1, "no adaptation-field PCR anchor was accepted")
	}
	return nil
}

func PCRToTicks(pcr27 uint64) uint64 { return pcr27 / 300 }
