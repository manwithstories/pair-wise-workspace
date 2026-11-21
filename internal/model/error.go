// Package model defines the shared error type and identifiers used by every
// subsystem. Errors always carry enough provenance (track, packet sequence,
// media interval) to explain the failing artifact without re-running.
package model

import (
	"errors"
	"fmt"
	"strings"

	"mediad/internal/clock"
)

// Category classifies a failure for the CLI exit code and for the manifest.
type Category string

const (
	CategoryUsage     Category = "usage"
	CategoryMetadata  Category = "metadata"
	CategoryTransport Category = "transport"
	CategoryTimeline  Category = "timeline"
	CategoryRender    Category = "render"
	CategoryScheduler Category = "scheduler"
	CategoryManifest  Category = "manifest"
	CategoryInternal  Category = "internal"
)

// Interval is a half-open media interval [Start, End) in a rational timebase.
type Interval struct {
	Start    int64
	End      int64
	Timebase clock.Timebase
}

// Valid reports whether the interval is ordered and non-empty.
func (i Interval) Valid() bool {
	return i.End >= i.Start && i.Timebase.Valid()
}

// Duration is End-Start in ticks.
func (i Interval) Duration() int64 { return i.End - i.Start }

// String renders the interval in ticks and milliseconds for terminals.
func (i Interval) String() string {
	return fmt.Sprintf("[%d,%d) ticks @%s (%d..%d ms)",
		i.Start, i.End, i.Timebase, i.Timebase.ToMillis(i.Start), i.Timebase.ToMillis(i.End))
}

// Error is the single error model shared by metadata, transport, timeline,
// scheduler, render and manifest.
type Error struct {
	Category Category
	Op       string
	// Track is the normalized track key ("v0", "a0", ...) when applicable.
	Track string
	// PacketSeq is the RTP sequence number when applicable; -1 means absent.
	PacketSeq int64
	// HasPacketSeq disambiguates sequence number zero from "not applicable".
	HasPacketSeq bool
	// Interval is the media interval affected by the failure, if known.
	Interval *Interval
	// BlockingDep names the first dependency that blocked downstream work.
	BlockingDep string
	Detail      string
	Err         error
}

// New builds a categorized error.
func New(cat Category, op, detail string) *Error {
	return &Error{Category: cat, Op: op, Detail: detail, PacketSeq: -1}
}

// Wrap builds a categorized error around err.
func Wrap(cat Category, op string, err error) *Error {
	return &Error{Category: cat, Op: op, Err: err, PacketSeq: -1}
}

// WithTrack attaches a normalized track key.
func (e *Error) WithTrack(track string) *Error {
	e.Track = track
	return e
}

// WithPacket attaches the offending RTP sequence number.
func (e *Error) WithPacket(seq uint32) *Error {
	e.PacketSeq = int64(seq)
	e.HasPacketSeq = true
	return e
}

// WithInterval attaches the affected media interval.
func (e *Error) WithInterval(iv Interval) *Error {
	e.Interval = &iv
	return e
}

// WithBlockingDep records the first blocking dependency and never overwrites
// an earlier one: the first cause is the one operators need.
func (e *Error) WithBlockingDep(dep string) *Error {
	if dep != "" && e.BlockingDep == "" {
		e.BlockingDep = dep
	}
	return e
}

// Error implements error.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("mediad: ")
	b.WriteString(string(e.Category))
	if e.Op != "" {
		b.WriteString(": ")
		b.WriteString(e.Op)
	}
	if e.Track != "" {
		b.WriteString(": track=")
		b.WriteString(e.Track)
	}
	if e.HasPacketSeq {
		fmt.Fprintf(&b, ": packet=%d", e.PacketSeq)
	}
	if e.Interval != nil {
		b.WriteString(": interval=")
		b.WriteString(e.Interval.String())
	}
	if e.BlockingDep != "" {
		b.WriteString(": blocked_by=")
		b.WriteString(e.BlockingDep)
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap supports errors.Is/As.
func (e *Error) Unwrap() error { return e.Err }

// CategoryOf returns the category of the first categorized error in the chain.
func CategoryOf(err error) Category {
	var me *Error
	if errors.As(err, &me) {
		return me.Category
	}
	if err != nil {
		return CategoryInternal
	}
	return ""
}

// FirstBlocking returns the first non-empty blocking dependency in the chain.
func FirstBlocking(err error) string {
	var me *Error
	if errors.As(err, &me) {
		return me.BlockingDep
	}
	return ""
}

// ExitCode maps an error to the CLI contract: 0 success, 1 usage, 2 data.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if CategoryOf(err) == CategoryUsage {
		return 1
	}
	return 2
}

// TrackKey normalizes track identifiers so metadata, timeline and manifest
// agree on the same spelling.
func TrackKey(kind string, id uint32) string {
	switch kind {
	case "video":
		return fmt.Sprintf("v%d", id)
	case "audio":
		return fmt.Sprintf("a%d", id)
	default:
		return fmt.Sprintf("t%d", id)
	}
}
