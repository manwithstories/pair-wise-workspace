// Package clock defines the unified monotonic media time base shared by all
// pipeline stages, plus a linear drift model and timestamp repair that
// handles wraparound, jumps and encoder restarts without fabricating time.
package clock

import "fmt"

const (
	// Timescale is the unified media clock rate in Hz (RTP-style 90 kHz).
	Timescale = 90000
	// WrapModulus is the 32-bit timestamp modulus.
	WrapModulus = int64(1) << 32
)

// FaultKind classifies a located timeline fault.
type FaultKind int

const (
	FaultWrap     FaultKind = iota // 32-bit timestamp wraparound
	FaultJump                      // forward jump beyond MaxGap
	FaultRestart                   // encoder restart, re-anchored from trusted anchor
	FaultIsolated                  // no trusted anchor; segment isolated
)

func (k FaultKind) String() string {
	switch k {
	case FaultWrap:
		return "wrap"
	case FaultJump:
		return "jump"
	case FaultRestart:
		return "restart"
	case FaultIsolated:
		return "isolated"
	}
	return "unknown"
}

// Fault pinpoints a timeline fault at a source sequence number.
type Fault struct {
	Kind    FaultKind
	Seq     uint64
	Segment int
	Detail  string
}

func (f Fault) String() string {
	return fmt.Sprintf("seq=%d seg=%d %s: %s", f.Seq, f.Segment, f.Kind, f.Detail)
}

// Sample is a raw access-unit timestamp as read from a stream.
type Sample struct {
	Seq      uint64
	TS       uint32
	Keyframe bool
}

// FixedSample is a sample repaired onto the monotonic timeline. Time is -1
// for isolated samples (no trusted anchor; time is not fabricated).
type FixedSample struct {
	Seq      uint64
	Time     int64
	Keyframe bool
	Isolated bool
	Segment  int
}

// SegmentInfo summarizes one repaired timeline segment.
type SegmentInfo struct {
	ID        int
	Samples   int
	Isolated  bool
	Recovered bool // re-anchored from a trusted anchor after a fault
}

// RepairConfig controls timestamp repair.
type RepairConfig struct {
	NominalDelta int64                 // expected per-sample step on the unified clock
	MaxGap       int64                 // forward jump tolerance before a restart is declared
	Trusted      func(seq uint64) bool // trusted-anchor predicate; nil trusts nothing
}

// Repair unwraps 32-bit timestamps, repairs jumps and encoder restarts, and
// isolates segments lacking a trusted anchor. It is deterministic.
func Repair(samples []Sample, cfg RepairConfig) ([]FixedSample, []Fault, []SegmentInfo) {
	fixed := make([]FixedSample, 0, len(samples))
	var faults []Fault
	var segs []SegmentInfo
	if len(samples) == 0 {
		return fixed, faults, segs
	}
	trusted := func(seq uint64) bool { return cfg.Trusted != nil && cfg.Trusted(seq) }

	var offset int64 // unwrap offset applied to raw timestamps
	seg := 0
	anchored := true
	prevRaw := int64(samples[0].TS)
	prevTime := prevRaw
	segRecovered := false

	segStartIdx := 0
	openSeg := func(id int) {
		segs = append(segs, SegmentInfo{ID: id})
		segStartIdx = len(fixed)
	}
	finish := func(isolated bool) {
		segs[len(segs)-1].Samples = len(fixed) - segStartIdx
		segs[len(segs)-1].Isolated = isolated
		segs[len(segs)-1].Recovered = segRecovered
	}
	openSeg(0)

	for i, s := range samples {
		raw := int64(s.TS) + offset
		reanchored := false
		if i > 0 {
			// Wraparound: raw went hugely negative but adding one modulus
			// lands within the expected step window.
			if d := raw + WrapModulus - prevRaw; d > 0 && d <= cfg.MaxGap {
				offset += WrapModulus
				raw += WrapModulus
				faults = append(faults, Fault{FaultWrap, s.Seq, seg, "32-bit timestamp wraparound"})
			}
			delta := raw - prevRaw
			if delta < 0 || delta > cfg.MaxGap {
				kind := FaultJump
				if delta < 0 {
					kind = FaultRestart
				}
				finish(!anchored)
				seg++
				openSeg(seg)
				if trusted(s.Seq) {
					// Encoder restart with a trusted anchor: re-anchor,
					// keeping time monotonic across the boundary.
					anchored = true
					segRecovered = true
					raw = prevTime + cfg.NominalDelta
					offset = raw - int64(s.TS)
					reanchored = true
					faults = append(faults, Fault{kind, s.Seq, seg, "encoder restart re-anchored"})
				} else {
					anchored = false
					segRecovered = false
					faults = append(faults, Fault{FaultIsolated, s.Seq, seg, "no trusted anchor; segment isolated"})
				}
			}
		}
		if !anchored && trusted(s.Seq) {
			anchored = true
			segRecovered = true
			raw = prevTime + cfg.NominalDelta
			offset = raw - int64(s.TS)
			reanchored = true
			faults = append(faults, Fault{FaultRestart, s.Seq, seg, "segment re-anchored from trusted anchor"})
		}
		fs := FixedSample{Seq: s.Seq, Keyframe: s.Keyframe, Segment: seg}
		if anchored {
			if i == 0 || reanchored {
				fs.Time = raw
			} else {
				fs.Time = prevTime + (raw - prevRaw)
			}
			prevTime = fs.Time
			prevRaw = raw
		} else {
			fs.Time = -1
			fs.Isolated = true
			prevRaw = raw // track raw cadence even while isolated
		}
		fixed = append(fixed, fs)
	}
	finish(!anchored)
	return fixed, faults, segs
}

// DriftModel estimates and removes linear clock drift from anchor pairs.
type DriftModel struct {
	anchors [][2]int64 // {local, reference}
}

// AddAnchor records a (local, reference) clock pair.
func (d *DriftModel) AddAnchor(local, ref int64) { d.anchors = append(d.anchors, [2]int64{local, ref}) }

// PPM returns the estimated drift in parts per million (0 with <2 anchors).
func (d *DriftModel) PPM() float64 {
	if len(d.anchors) < 2 {
		return 0
	}
	a, b := d.anchors[0], d.anchors[len(d.anchors)-1]
	dl := b[0] - a[0]
	if dl == 0 {
		return 0
	}
	return (float64(b[1]-a[1])/float64(dl) - 1) * 1e6
}

// Correct maps a local timestamp onto the reference clock.
func (d *DriftModel) Correct(local int64) int64 {
	if len(d.anchors) < 2 {
		return local
	}
	a, b := d.anchors[0], d.anchors[len(d.anchors)-1]
	dl := b[0] - a[0]
	if dl == 0 {
		return local
	}
	return a[1] + (local-a[0])*(b[1]-a[1])/dl
}
