// Package model defines cross-module error categories and limits shared by
// every subsystem of the media delivery kernel.
package model

import (
	"errors"
	"fmt"
)

// Kind classifies an error across module boundaries.
type Kind uint8

const (
	KindUnknown       Kind = iota
	KindInvalidInput       // bad manifest, parameters or container fields
	KindCorruptMedia       // damaged MP4/RTP payload detected during demux
	KindOverflow           // arithmetic overflow or time-base conversion failure
	KindLimitExceeded      // configured bound (size, tracks, packets) exceeded
	KindConflict           // duplicate mapping, duplicate edit range, digest mismatch
	KindCancelled          // context cancellation propagated through the pipeline
	KindIO                 // output or storage failure
)

// Error is the unified error type used by all modules. It records the first
// error position (1-based line for manifests, 0-based packet/byte offset for
// media) so corruption can be traced back to its origin.
type Error struct {
	Kind     Kind
	Module   string // e.g. "manifest", "demux", "remux"
	Position int64  // line number or packet/byte offset; -1 when unknown
	Op       string // short operation label, e.g. "parse manifest"
	Msg      string
	Err      error
}

func (e *Error) Error() string {
	pos := ""
	if e.Position >= 0 {
		pos = fmt.Sprintf(" at position %d", e.Position)
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s%s: %s: %v", e.Module, e.Op, pos, e.Msg, e.Err)
	}
	return fmt.Sprintf("%s: %s%s: %s", e.Module, e.Op, pos, e.Msg)
}

func (e *Error) Unwrap() error { return e.Err }

// New builds a classified error with an unknown position.
func New(kind Kind, module, op, msg string) *Error {
	return &Error{Kind: kind, Module: module, Op: op, Position: -1, Msg: msg}
}

// At builds a classified error carrying the first error position.
func At(kind Kind, module, op string, position int64, msg string) *Error {
	return &Error{Kind: kind, Module: module, Op: op, Position: position, Msg: msg}
}

// Wrap attaches a cause while keeping the classification and position.
func Wrap(kind Kind, module, op string, position int64, msg string, cause error) *Error {
	return &Error{Kind: kind, Module: module, Op: op, Position: position, Msg: msg, Err: cause}
}

// KindOf returns the classification of err, or KindUnknown.
func KindOf(err error) Kind {
	var me *Error
	if errors.As(err, &me) {
		return me.Kind
	}
	return KindUnknown
}

// PositionOf returns the recorded first error position, or -1.
func PositionOf(err error) int64 {
	var me *Error
	if errors.As(err, &me) {
		return me.Position
	}
	return -1
}

// Limits centralises every configurable bound of the kernel. An oversized
// packet or container is rejected (KindLimitExceeded) rather than buffered.
type Limits struct {
	MaxInputLength  int64 // max input file size in bytes
	MaxTracks       int   // max tracks per material / output segment
	MaxPacketBytes  int   // max payload of a single packet
	MaxPackets      int   // max packets per material
	MaxQueuePackets int   // bounded pipeline queue capacity
}

// DefaultLimits returns conservative bounds suitable for local fixtures.
func DefaultLimits() Limits {
	return Limits{
		MaxInputLength:  256 << 20,
		MaxTracks:       16,
		MaxPacketBytes:  64 << 10,
		MaxPackets:      1 << 20,
		MaxQueuePackets: 64,
	}
}
