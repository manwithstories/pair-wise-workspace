// Package timeline maps rational-millisecond cues onto frame timelines.
package timeline

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"subtool/internal/model"
)

// FPS is a rational frame rate, e.g. 24000/1001.
type FPS struct {
	Num int64
	Den int64
}

// RoundMode selects how exact frame positions snap to integers.
type RoundMode string

const (
	RoundFloor   RoundMode = "floor"
	RoundNearest RoundMode = "nearest"
	RoundCeil    RoundMode = "ceil"
)

// ParseFPS accepts "24", "25", "50", "60", "24000/1001", "30000/1001".
func ParseFPS(s string) (FPS, error) {
	s = strings.TrimSpace(s)
	if strings.Contains(s, "/") {
		parts := strings.SplitN(s, "/", 2)
		n, err1 := strconv.ParseInt(parts[0], 10, 64)
		d, err2 := strconv.ParseInt(parts[1], 10, 64)
		if err1 != nil || err2 != nil || n <= 0 || d <= 0 {
			return FPS{}, fmt.Errorf("invalid fps %q", s)
		}
		return FPS{Num: n, Den: d}, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return FPS{}, fmt.Errorf("invalid fps %q", s)
	}
	return FPS{Num: n, Den: 1}, nil
}

func (f FPS) String() string {
	if f.Den == 1 {
		return strconv.FormatInt(f.Num, 10)
	}
	return fmt.Sprintf("%d/%d", f.Num, f.Den)
}

// IsDropFrame reports whether this rate uses NTSC drop-frame timecode.
func (f FPS) IsDropFrame() bool {
	return f.Num%30000 == 0 && f.Den == 1001 || f.Num%60000 == 0 && f.Den == 1001
}

// exactFrame returns the exact (fractional) frame position of a ms time.
func (f FPS) exactFrame(ms *big.Rat) *big.Rat {
	r := new(big.Rat).Mul(ms, big.NewRat(f.Num, f.Den))
	return r.Quo(r, big.NewRat(1000, 1))
}

// FrameAt maps rational ms to an integer frame under the round mode.
func (f FPS) FrameAt(ms *big.Rat, mode RoundMode) int64 {
	ex := f.exactFrame(ms)
	num, den := ex.Num(), ex.Denom()
	q := new(big.Int).Quo(num, den) // truncated toward zero; ms >= 0 here
	rem := new(big.Int).Rem(num, den)
	switch mode {
	case RoundCeil:
		if rem.Sign() != 0 {
			q.Add(q, big.NewInt(1))
		}
	case RoundNearest:
		// round half up
		if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(den) >= 0 {
			q.Add(q, big.NewInt(1))
		}
	}
	return q.Int64()
}

// MsAtFrame returns the rational ms at the start of a frame.
func (f FPS) MsAtFrame(frame int64) *big.Rat {
	return big.NewRat(frame*1000*f.Den, f.Num)
}

// Snap assigns StartFrame/EndFrame to a cue. EndFrame is never smaller
// than StartFrame; sub-frame durations are flagged, not dropped.
func (f FPS) Snap(c *model.Cue, mode RoundMode) {
	c.StartFrame = f.FrameAt(c.Start, mode)
	end := f.FrameAt(c.End, mode)
	if end < c.StartFrame {
		end = c.StartFrame
	}
	c.EndFrame = end
	c.Subframe = end == c.StartFrame
}

// SnapDriftMs returns |snap(start)-start| + |snap(end)-end| in ms,
// i.e. how far the frame grid pulled the cue boundaries.
func (f FPS) SnapDriftMs(c *model.Cue) float64 {
	d := new(big.Rat).Sub(f.MsAtFrame(c.StartFrame), c.Start)
	d.Abs(d)
	e := new(big.Rat).Sub(f.MsAtFrame(c.EndFrame), c.End)
	e.Abs(e)
	d.Add(d, e)
	v, _ := d.Float64()
	return v
}

// DropTimecode renders a frame number as SMPTE drop-frame timecode
// (29.97/59.94 only); non-drop rates get a plain HH:MM:SS:FF.
func DropTimecode(frame int64, fps FPS) string {
	fpsInt := (fps.Num + fps.Den - 1) / fps.Den // nominal integer rate
	if !fps.IsDropFrame() {
		return plainTimecode(frame, fpsInt, ":")
	}
	drop := int64(2)
	if fpsInt == 60 {
		drop = 4
	}
	// 29.97 DF: drop 2 frames every minute except every 10th.
	framesPer10Min := fpsInt*600 - drop*9
	framesPerMin := fpsInt - drop
	d := frame / framesPer10Min
	m := frame % framesPer10Min
	adj := int64(0)
	if m >= drop {
		adj = drop * ((m - drop) / framesPerMin)
	}
	return plainTimecode(frame+18*d+adj, fpsInt, ";")
}

func plainTimecode(frame, fpsInt int64, sep string) string {
	ff := frame % fpsInt
	total := frame / fpsInt
	ss := total % 60
	mm := (total / 60) % 60
	hh := total / 3600
	return fmt.Sprintf("%02d:%02d:%02d%s%02d", hh, mm, ss, sep, ff)
}
