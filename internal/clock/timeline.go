package clock

import (
	"math"
	"sort"
)

const nanosPerSecond int64 = 1_000_000_000

// Result reports how one raw timestamp was mapped onto the unified
// integer-nanosecond axis. Flags are additive: a duplicate is also a
// regression, and a wrap is never a regression.
type Result struct {
	Raw       uint64
	NS        int64
	Wrapped   bool
	Duplicate bool
	Regressed bool
}

// TrackClock unwraps a single media track's timestamps onto a monotonic
// nanosecond axis. The zero point is the first accepted sample, optionally
// shifted by Origin.
type TrackClock struct {
	Name      string
	Timescale uint64
	// Modulus is the wrap width of the raw counter (0 disables unwrapping).
	// For RTP video at 90 kHz this is 1<<32; for 33-bit audio it is 1<<33.
	Modulus uint64
	Origin  int64

	started   bool
	lastRaw   uint64
	wraps     uint64
	baseNS    int64
	lastNS    int64
	WrapCount int
	DupCount  int
	RegCount  int
}

// NewTrackClock validates the timescale up front so a bad manifest is
// rejected before any sample is accepted.
func NewTrackClock(name string, timescale, modulus uint64) (*TrackClock, error) {
	if timescale == 0 {
		return nil, New(KindInvalid, CodeInvalidDuration, name, "zero timescale")
	}
	if modulus != 0 && modulus < 2 {
		return nil, New(KindInvalid, CodeUnsupported, name, "modulus too small")
	}
	return &TrackClock{Name: name, Timescale: timescale, Modulus: modulus}, nil
}

// Convert maps raw onto the ns axis. An extended counter can exceed int64
// nanoseconds after a very large number of wraps; overflow is reported as a
// regression rather than wrapped into a negative time.
func (c *TrackClock) Convert(raw uint64) Result {
	res := Result{Raw: raw}
	if !c.started {
		c.started = true
		c.lastRaw = raw
		c.baseNS = c.Origin - scale(raw, c.Timescale)
		c.lastNS = c.Origin
		res.NS = c.Origin
		return res
	}
	delta := raw - c.lastRaw
	lo, hi := c.Modulus/2, c.Modulus
	if c.Modulus != 0 && raw < c.lastRaw && (c.lastRaw-raw) > lo {
		// Forward wrap: raw restarted near zero.
		c.wraps++
		c.WrapCount++
		res.Wrapped = true
	} else if raw == c.lastRaw {
		c.DupCount++
		res.Duplicate = true
		res.Regressed = true
		res.NS = c.lastNS
		return res
	} else if raw < c.lastRaw && c.Modulus != 0 && delta >= hi {
		// Impossible for a correct modulus; treat as corruption.
		c.RegCount++
		res.Regressed = true
		return res
	} else if raw < c.lastRaw {
		c.RegCount++
		res.Regressed = true
		// Do not move the track cursor backwards; report the mapped value
		// so an upper layer can decide whether this is a late packet.
		ext := c.wraps*c.Modulus + raw
		res.NS = c.baseNS + scale(ext, c.Timescale)
		return res
	}
	ext := c.wraps*c.Modulus + raw
	ns, ok := addScale(c.baseNS, ext, c.Timescale)
	if !ok {
		// The axis position cannot be represented; refuse rather than emit
		// a wrapped, meaningless timestamp.
		c.RegCount++
		res.Regressed = true
		return res
	}
	res.NS = ns
	c.lastRaw = raw
	c.lastNS = ns
	return res
}

// LastNS returns the last committed axis position.
func (c *TrackClock) LastNS() int64 { return c.lastNS }

// Overhead counts events that changed the mapping.
func (c *TrackClock) Overhead() (wraps, dups, regs int) {
	return c.WrapCount, c.DupCount, c.RegCount
}

// scale converts a raw counter value to nanoseconds, rounding down. It is
// overflow-safe for any uint64 below MaxInt64/1e9 seconds.
func scale(v uint64, timescale uint64) int64 {
	q := v / timescale
	r := v % timescale
	return int64(q)*nanosPerSecond + int64(r)*nanosPerSecond/int64(timescale)
}

// addScale adds base + scale(v, timescale), reporting overflow instead of
// silently wrapping.
func addScale(base int64, v uint64, timescale uint64) (int64, bool) {
	q := v / timescale
	r := v % timescale
	if q > uint64(math.MaxInt64/nanosPerSecond) {
		return 0, false
	}
	part := int64(q)*nanosPerSecond + int64(r)*nanosPerSecond/int64(timescale)
	if part > 0 && base > math.MaxInt64-part {
		return 0, false
	}
	if part < 0 && base < math.MinInt64-part {
		return 0, false
	}
	return base + part, true
}

// DriftPoint is one observation of a media clock against a reference clock,
// both expressed on the unified ns axis.
type DriftPoint struct {
	RefNS   int64
	MediaNS int64
}

// FitConfig bounds what counts as a uniquely determined fit.
type FitConfig struct {
	MinPoints     int
	MinSpanNS     int64
	MaxResidualNS int64
}

// DefaultFitConfig demands enough baseline to separate slope from offset.
func DefaultFitConfig() FitConfig {
	return FitConfig{MinPoints: 4, MinSpanNS: 1_000_000, MaxResidualNS: 2_000_000}
}

// DriftFit is the least-squares relation MediaNS = Offset + Slope*RefNS.
// SlopePPB is the slope expressed in parts per billion of real time.
type DriftFit struct {
	SlopePPB      int64
	OffsetNS      int64
	Points        int
	MaxResidualNS int64
}

// Correction returns the ns to add to a media timestamp observed at refNS so
// that it aligns with the reference axis.
func (f DriftFit) Correction(mediaNS int64, refNS int64) int64 {
	predicted := f.OffsetNS + scaleSlope(f.SlopePPB, refNS)
	return predicted - mediaNS
}

// FitDrift performs an ordinary least-squares fit. When the slope cannot be
// pinned down — too few points, no baseline, duplicate reference times, or
// residuals outside the configured bound — it returns KindUnrecoverable and
// never rewrites the input silently.
func FitDrift(points []DriftPoint, cfg FitConfig) (DriftFit, error) {
	if len(points) < cfg.MinPoints {
		return DriftFit{}, New(KindUnrecoverable, CodeDriftAmbiguous, "",
			"not enough points")
	}
	pts := make([]DriftPoint, len(points))
	copy(pts, points)
	sort.Slice(pts, func(i, j int) bool { return pts[i].RefNS < pts[j].RefNS })
	for i := 1; i < len(pts); i++ {
		if pts[i].RefNS == pts[i-1].RefNS {
			if pts[i].MediaNS != pts[i-1].MediaNS {
				return DriftFit{}, New(KindUnrecoverable, CodeDriftAmbiguous, "",
					"conflicting observations at same reference time")
			}
			return DriftFit{}, New(KindUnrecoverable, CodeDriftAmbiguous, "",
				"duplicate reference time: slope undetermined")
		}
	}
	span := pts[len(pts)-1].RefNS - pts[0].RefNS
	if span < cfg.MinSpanNS {
		return DriftFit{}, New(KindUnrecoverable, CodeDriftAmbiguous, "",
			"baseline too short to determine slope")
	}

	n := float64(len(pts))
	var mx, my float64
	for _, p := range pts {
		mx += float64(p.RefNS)
		my += float64(p.MediaNS)
	}
	mx /= n
	my /= n
	var num, den float64
	for _, p := range pts {
		dx := float64(p.RefNS) - mx
		num += dx * (float64(p.MediaNS) - my)
		den += dx * dx
	}
	if den == 0 || math.IsInf(den, 0) || math.IsNaN(den) {
		return DriftFit{}, New(KindUnrecoverable, CodeDriftAmbiguous, "",
			"degenerate reference axis")
	}
	slope := num / den
	if math.IsInf(slope, 0) || math.IsNaN(slope) {
		return DriftFit{}, New(KindUnrecoverable, CodeDriftAmbiguous, "",
			"non-finite slope")
	}
	offset := my - slope*mx
	slopePPB := int64(math.Round(slope * float64(nanosPerSecond)))
	offsetNS := int64(math.Round(offset))

	var maxRes float64
	for _, p := range pts {
		pred := float64(offsetNS) + float64(scaleSlope(slopePPB, p.RefNS))
		res := math.Abs(float64(p.MediaNS) - pred)
		if res > maxRes {
			maxRes = res
		}
	}
	if int64(maxRes) > cfg.MaxResidualNS {
		return DriftFit{}, New(KindUnrecoverable, CodeDriftAmbiguous, "",
			"residual exceeds bound: drift not uniquely determined")
	}
	return DriftFit{
		SlopePPB:      slopePPB,
		OffsetNS:      offsetNS,
		Points:        len(pts),
		MaxResidualNS: int64(maxRes),
	}, nil
}

func scaleSlope(ppb int64, refNS int64) int64 {
	return int64((float64(refNS) * float64(ppb)) / float64(nanosPerSecond))
}
