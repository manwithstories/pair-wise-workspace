// Package media defines the shared error model used by every parsing and
// transformation stage of mediasync.
//
// Every issue keeps the stage, the input type, the byte offset inside the
// original input and (for packet streams) the packet sequence number. Issues
// are either recoverable (processing continues, exit code 2) or fatal
// (parsing cannot continue, exit code 1).
package media

import (
	"fmt"
	"sort"
)

// Stage identifies the pipeline stage an issue belongs to.
type Stage string

const (
	StageInput       Stage = "input"
	StageSubtitle    Stage = "subtitle"
	StageTimebase    Stage = "timebase"
	StageMP4         Stage = "mp4"
	StageRTP         Stage = "rtp"
	StageSegment     Stage = "segment"
	StageAudio       Stage = "audio"
	StageFingerprint Stage = "fingerprint"
	StagePipeline    Stage = "pipeline"
)

// InputType identifies the kind of input that produced an issue.
type InputType string

const (
	InputSRT    InputType = "srt"
	InputWebVTT InputType = "webvtt"
	InputMP4    InputType = "mp4"
	InputRTP    InputType = "rtp"
	InputPCM    InputType = "pcm"
	InputTC     InputType = "timecode"
)

// Code is a machine-readable classification of an issue.
type Code string

const (
	CodeInvalidArgument  Code = "invalid_argument"
	CodeMalformed        Code = "malformed_record"
	CodeInvalidTimestamp Code = "invalid_timestamp"
	CodeTruncated        Code = "truncated_input"
	CodeOverlap          Code = "overlapping_cue"
	CodeNonMonotonic     Code = "non_monotonic"
	CodeOutOfRange       Code = "out_of_range"
	CodeMissingEntry     Code = "missing_entry"
	CodePacketLoss       Code = "packet_loss"
	CodeDuplicatePacket  Code = "duplicate_packet"
	CodeDuplicate        Code = "duplicate_entry"
	CodeWindowExceeded   Code = "reorder_window_exceeded"
	CodeOffsetInsideGap  Code = "offset_inside_gap"
	CodeNoKeyframe       Code = "boundary_without_keyframe"
	CodeSilenceEdge      Code = "silence_edge"
	CodeIncompatible     Code = "incompatible_timebase"
	CodeIncomplete       Code = "incomplete_data"
	CodeLimitExceeded    Code = "limit_exceeded"
	CodePathRejected     Code = "path_rejected"
)

// Issue is a structured, JSON-serialisable problem record.
type Issue struct {
	Stage     Stage     `json:"stage"`
	Input     InputType `json:"input"`
	Code      Code      `json:"code"`
	File      string    `json:"file,omitempty"`
	Record    int       `json:"record,omitempty"`
	Message   string    `json:"message"`
	Offset    int64     `json:"offset"`
	HasOffset bool      `json:"has_offset"`
	PacketSeq uint32    `json:"packet_seq"`
	HasSeq    bool      `json:"has_packet_seq"`
	Fatal     bool      `json:"fatal"`
}

// New builds a recoverable issue formatted with fmt.Sprintf semantics.
func New(stage Stage, input InputType, code Code, format string, args ...any) *Issue {
	return &Issue{Stage: stage, Input: input, Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error implements the error interface.
func (e *Issue) Error() string {
	loc := ""
	if e.File != "" {
		loc = e.File + ":"
	}
	if e.HasOffset {
		loc += fmt.Sprintf("@%d:", e.Offset)
	}
	if e.HasSeq {
		loc += fmt.Sprintf("seq=%d:", e.PacketSeq)
	}
	return fmt.Sprintf("%s%s[%s/%s] %s", loc, e.Stage, e.Input, e.Code, e.Message)
}

// WithFile attaches the source name.
func (e *Issue) WithFile(file string) *Issue { e.File = file; return e }

// WithOffset attaches a raw byte offset in the original input.
func (e *Issue) WithOffset(off int64) *Issue { e.Offset, e.HasOffset = off, true; return e }

// WithRecord attaches a one-based record number (subtitle cue, sample, ...).
func (e *Issue) WithRecord(rec int) *Issue { e.Record = rec; return e }

// WithSeq attaches the RTP packet sequence number.
func (e *Issue) WithSeq(seq uint32) *Issue { e.PacketSeq, e.HasSeq = seq, true; return e }

// AsFatal marks the issue as unrecoverable and returns it.
func (e *Issue) AsFatal() *Issue { e.Fatal = true; return e }

// Recoverable reports whether processing may continue past this issue.
func (e *Issue) Recoverable() bool { return !e.Fatal }

// Filter returns the issues that match pred, preserving order.
func Filter(issues []*Issue, pred func(*Issue) bool) []*Issue {
	out := make([]*Issue, 0, len(issues))
	for _, is := range issues {
		if pred(is) {
			out = append(out, is)
		}
	}
	return out
}

// CountWithOffset counts issues that carry a raw byte offset.
func CountWithOffset(issues []*Issue) int {
	n := 0
	for _, is := range issues {
		if is.HasOffset {
			n++
		}
	}
	return n
}

// FirstFatal returns the first fatal issue, or nil.
func FirstFatal(issues []*Issue) *Issue {
	for _, is := range issues {
		if is.Fatal {
			return is
		}
	}
	return nil
}

// SortStable orders issues by file, offset, record and sequence for
// deterministic reporting. Issues without an offset sort before those with one.
func SortStable(issues []*Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.HasOffset != b.HasOffset {
			return !a.HasOffset
		}
		if a.Offset != b.Offset {
			return a.Offset < b.Offset
		}
		if a.Record != b.Record {
			return a.Record < b.Record
		}
		if a.HasSeq != b.HasSeq {
			return !a.HasSeq
		}
		return a.PacketSeq < b.PacketSeq
	})
}
