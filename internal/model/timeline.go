// Package model defines the unified media time base shared by all modules.
package model

// RTPTimebase is the unified clock: 90 kHz, matching the RTP capture clock.
const RTPTimebase = 90000

// TimeRange is a half-open interval [Start, End) on the unified clock.
type TimeRange struct {
	Start int64
	End   int64
}

// Valid reports whether the range is non-empty and ordered.
func (r TimeRange) Valid() bool { return r.End > r.Start }

// Contains reports whether t falls inside the range.
func (r TimeRange) Contains(t int64) bool { return t >= r.Start && t < r.End }

// ColorMeta carries the colour tagging of a frame. Empty means unspecified;
// unspecified is never silently overwritten (see core.Inspect).
type ColorMeta struct {
	Primaries string
	Transfer  string
	Matrix    string
}

// Specified reports whether all three colour tags are present.
func (c ColorMeta) Specified() bool {
	return c.Primaries != "" && c.Transfer != "" && c.Matrix != ""
}

// Frame is one decoded-delivery frame on the unified time base.
type Frame struct {
	Seq       uint32 // delivery sequence number
	CaptureTS uint32 // raw 32-bit capture clock (may wrap)
	Time      int64  // unwrapped unified time, RTPTimebase ticks
	Keyframe  bool
	Width     uint16
	Height    uint16
	Color     ColorMeta
	Payload   []byte
	Hash      [32]byte // SHA-256 of Payload as delivered
}

// Unwrapper converts a wrapping 32-bit capture clock into a monotonic
// int64 unified time line. Deltas are computed in wrap-aware signed
// arithmetic, so a forward wrap is a small positive step while a jump
// backward of up to half the clock range surfaces as a negative step
// (a regression) for the caller to flag.
type Unwrapper struct {
	cur    int64
	last   uint32
	inited bool
}

// Next folds ts into the unwrapped line and returns the unified time.
func (u *Unwrapper) Next(ts uint32) int64 {
	if !u.inited {
		u.inited = true
		u.last = ts
		u.cur = int64(ts)
		return u.cur
	}
	delta := int32(ts - u.last) // wrap-aware signed delta
	u.last = ts
	u.cur += int64(delta)
	return u.cur
}
