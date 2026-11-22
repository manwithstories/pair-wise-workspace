// Package render generates deterministic off-screen review frames at a
// fixed interval. No real media is decoded: a frame is a digest of the
// layer summary active at its timestamp, seeded with the container
// timebase bytes.
package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"sort"

	mp4 "github.com/abema/go-mp4"

	"mediasync/internal/timeline"
)

// Interval is the fixed review-frame spacing in nanoseconds.
const Interval = int64(1_000_000_000) // 1s

// Generator emits frames at the fixed Interval and accumulates a
// running digest over all frame digests.
type Generator struct {
	tb    []byte
	next  int64
	h     hash.Hash
	Count int
}

// NewGenerator returns a Generator starting at time zero.
func NewGenerator() *Generator {
	return &Generator{tb: timebaseBytes(), h: sha256.New()}
}

// Advance emits one frame per interval for every frame time below t.
// active holds the events visible during the period, in any order;
// frame lines are appended to w.
func (g *Generator) Advance(t int64, active []timeline.Event, w io.Writer) error {
	sorted := make([]timeline.Event, len(active))
	copy(sorted, active)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.Track != b.Track {
			return a.Track < b.Track
		}
		return a.Label < b.Label
	})
	for g.next < t {
		d := g.digest(sorted)
		g.h.Write(d)
		if _, err := fmt.Fprintf(w, "F %d %d %x\n", g.next/Interval, g.next, d); err != nil {
			return err
		}
		g.Count++
		g.next += Interval
	}
	return nil
}

// Summary returns the hex digest over all emitted frame digests.
func (g *Generator) Summary() string { return fmt.Sprintf("%x", g.h.Sum(nil)) }

func (g *Generator) digest(evs []timeline.Event) []byte {
	h := sha256.New()
	h.Write(g.tb)
	var buf [16]byte
	binary.BigEndian.PutUint64(buf[0:8], uint64(g.next))
	h.Write(buf[0:8])
	for _, ev := range evs {
		if !visible(ev, g.next) {
			continue
		}
		binary.BigEndian.PutUint64(buf[0:8], uint64(ev.Start))
		binary.BigEndian.PutUint64(buf[8:16], uint64(ev.End))
		h.Write([]byte(ev.Track))
		h.Write(buf[:])
		h.Write([]byte(ev.Label))
	}
	return h.Sum(nil)
}

// visible reports whether ev is on screen at time t. Instant markers
// (End == Start) are visible for the whole interval they fall into.
func visible(ev timeline.Event, t int64) bool {
	if ev.End == ev.Start {
		return t <= ev.Start && ev.Start < t+Interval
	}
	return ev.Start <= t && t < ev.End
}

// timebaseBytes marshals a minimal mvhd box carrying the 90 kHz
// container timebase; the bytes seed every frame digest.
func timebaseBytes() []byte {
	var buf bytes.Buffer
	mvhd := &mp4.Mvhd{
		Timescale:   90000,
		Rate:        0x00010000,
		Volume:      0x0100,
		Matrix:      [9]int32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000},
		NextTrackID: 1,
	}
	if _, err := mp4.Marshal(&buf, mvhd, mp4.Context{}); err != nil {
		panic(err) // deterministic input; cannot fail
	}
	return buf.Bytes()
}
