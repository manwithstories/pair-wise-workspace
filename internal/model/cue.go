// Package model defines the unified cue representation shared by all layers.
// model is a leaf package: it must never import parse/serialize/timeline.
package model

import (
	"fmt"
	"math/big"
	"sort"
)

// Cue is one subtitle entry on a rational-millisecond timeline.
type Cue struct {
	ID         string   // stable identifier, e.g. "0007"
	Start      *big.Rat // start time in milliseconds
	End        *big.Rat // end time in milliseconds (End >= Start)
	Text       string   // payload text with inline tags preserved
	Tags       []string // inline tags extracted for lossless checks
	Layer      int      // overlap layer; dual-speaker cues coexist on layers
	StartFrame int64    // mapped start frame on the destination timeline
	EndFrame   int64    // mapped end frame; EndFrame >= StartFrame always
	Subframe   bool     // duration shorter than one frame after mapping
	SourceLine int      // 1-based line number in the source file
}

// Issue is a quarantined problem located at a source line.
type Issue struct {
	Line int    // 1-based source line, 0 if unknown
	Code string // machine-readable error code
	Msg  string
}

func (i Issue) String() string { return fmt.Sprintf("line %d: %s: %s", i.Line, i.Code, i.Msg) }

// Quarantine error codes.
const (
	CodeBadTimecode = "BAD_TIMECODE"
	CodeNegative    = "NEGATIVE_TIME"
	CodeTooLong     = "OVER_24H"
	CodeZeroLength  = "ZERO_LENGTH"
	CodeEndBefore   = "END_BEFORE_START"
	CodeMissing     = "MISSING_FIELD"
	CodeUndecodable = "UNDECODABLE_BYTES"
	CodeParse       = "PARSE_ERROR"
)

// Clone deep-copies a cue (rats are immutable values, safe to share).
func (c Cue) Clone() Cue {
	out := c
	if c.Tags != nil {
		out.Tags = append([]string(nil), c.Tags...)
	}
	return out
}

// DurationMs returns End-Start as a rational millisecond value.
func (c Cue) DurationMs() *big.Rat { return new(big.Rat).Sub(c.End, c.Start) }

// RatToFloat converts a rational to float64 milliseconds.
func RatToFloat(r *big.Rat) float64 {
	f, _ := r.Float64()
	return f
}

// MsRat builds a rational millisecond value from an integer.
func MsRat(ms int64) *big.Rat { return big.NewRat(ms, 1) }

// SortByStart orders cues by (start, id) stably for deterministic output.
func SortByStart(cues []Cue) {
	sort.SliceStable(cues, func(i, j int) bool {
		cmp := cues[i].Start.Cmp(cues[j].Start)
		if cmp != 0 {
			return cmp < 0
		}
		return cues[i].ID < cues[j].ID
	})
}
