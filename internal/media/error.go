package media

import (
	"errors"
	"fmt"
)

// Exit codes are part of the CLI contract.
const (
	ExitOK          = 0
	ExitInternal    = 1
	ExitCorrupt     = 2
	ExitRecoverable = 3
)

// Stage identifies the first processing stage that rejected a batch.
type Stage string

const (
	StageInput    Stage = "input"
	StageFraming  Stage = "framing"
	StageRTP      Stage = "rtp"
	StageRTCP     Stage = "rtcp"
	StageClock    Stage = "clock"
	StageLoss     Stage = "loss-policy"
	StageAudio    Stage = "audio"
	StageColor    Stage = "color"
	StageMP4      Stage = "mp4"
	StageWAL      Stage = "wal"
	StageRecover  Stage = "recover"
	StageReport   Stage = "report"
	StageCommit   Stage = "commit"
	StageInjected Stage = "injected"
)

// Error is the graded, machine-reportable error used across subsystems.
type Error struct {
	Code           int
	Stage          Stage
	Message        string
	PacketSequence *uint16
	RecordIndex    *int
	SSRC           *uint32
	Err            error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %s", e.Stage, e.Message)
	if e.RecordIndex != nil {
		msg += fmt.Sprintf(" record=%d", *e.RecordIndex)
	}
	if e.PacketSequence != nil {
		msg += fmt.Sprintf(" rtp_seq=%d", *e.PacketSequence)
	}
	if e.SSRC != nil {
		msg += fmt.Sprintf(" ssrc=%08x", *e.SSRC)
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

func newRigError(code int, stage Stage, msg string, err error) *Error {
	return &Error{Code: code, Stage: stage, Message: msg, Err: err}
}

func Internal(stage Stage, format string, args ...any) *Error {
	return newRigError(ExitInternal, stage, fmt.Sprintf(format, args...), nil)
}

func InternalWrap(stage Stage, err error, format string, args ...any) *Error {
	return newRigError(ExitInternal, stage, fmt.Sprintf(format, args...), err)
}

func Corrupt(stage Stage, format string, args ...any) *Error {
	return newRigError(ExitCorrupt, stage, fmt.Sprintf(format, args...), nil)
}

func CorruptWrap(stage Stage, err error, format string, args ...any) *Error {
	return newRigError(ExitCorrupt, stage, fmt.Sprintf(format, args...), err)
}

func Policy(stage Stage, format string, args ...any) *Error {
	return newRigError(ExitCorrupt, stage, fmt.Sprintf(format, args...), nil)
}

func Injected(stage Stage, format string, args ...any) *Error {
	return newRigError(ExitRecoverable, stage, fmt.Sprintf(format, args...), nil)
}

func WithPacketSequence(err *Error, seq uint16) *Error {
	err.PacketSequence = &seq
	return err
}

func WithRecordIndex(err *Error, idx int) *Error {
	err.RecordIndex = &idx
	return err
}

func WithSSRC(err *Error, ssrc uint32) *Error {
	err.SSRC = &ssrc
	return err
}

// ExitCode maps arbitrary errors to the stable CLI exit-code contract.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var re *Error
	if errors.As(err, &re) {
		return re.Code
	}
	return ExitInternal
}

// AsRigError exposes a graded error when present.
func AsRigError(err error) (*Error, bool) {
	var re *Error
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}
