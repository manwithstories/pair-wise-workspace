package ingest

import (
	"fmt"
	"strings"
)

// GenParams controls deterministic synthetic segment generation.
type GenParams struct {
	Seed        int64
	Reps        int64
	DurationSec int64
	SegSec      int64
}

// timescale ladder; every 4th rendition switches timescale mid-stream to
// exercise per-segment rescaling.
var tsLadder = []int64{48000, 90000, 44100, 12800}

const midTimescale = 16000

// Synthesize builds Reps renditions with distinct timescales, start offsets
// and bitrates. Boundary timestamps carry a deterministic jitter of at most
// +/-4 ms so that snapping to the common grid stays well within tolerance;
// the first and last boundaries are exact.
func Synthesize(p GenParams) ([]Track, error) {
	if p.Reps < 1 || p.Reps > 8 {
		return nil, fmt.Errorf("ingest: reps %d out of range [1,8]", p.Reps)
	}
	if p.DurationSec <= 0 || p.SegSec <= 0 || p.DurationSec%p.SegSec != 0 {
		return nil, fmt.Errorf("ingest: duration %d not divisible by seg %d", p.DurationSec, p.SegSec)
	}
	n := p.DurationSec / p.SegSec
	originMs := p.Reps*p.SegSec*1000 + 2500
	tracks := make([]Track, p.Reps)
	for i := int64(0); i < p.Reps; i++ {
		ts := tsLadder[i%int64(len(tsLadder))]
		ts2 := ts
		if i%4 == 3 {
			ts2 = midTimescale
		}
		half := n / 2
		originMsRep := originMs - i*p.SegSec*1000
		segs := make([]SegmentMeta, n)
		for j := int64(0); j < n; j++ {
			sts := ts
			if j >= half {
				sts = ts2
			}
			pts := basePTS(originMsRep, j, p.SegSec, sts)
			if j > 0 && j != half {
				pts += jitterMs(p.Seed, i, j) * sts / 1000
			}
			end := basePTS(originMsRep, j+1, p.SegSec, sts)
			if j+1 < n && j+1 != half {
				end += jitterMs(p.Seed, i, j+1) * sts / 1000
			}
			segs[j] = SegmentMeta{
				Number:    1 + i*100 + j,
				Timescale: sts,
				PTS:       pts,
				Duration:  end - pts,
				Bitrate:   400000 << i,
			}
		}
		tracks[i] = Track{
			ID:          fmt.Sprintf("r%d", i+1),
			StartNumber: 1 + i*100,
			Bandwidth:   400000 << i,
			Height:      LadderHeight(int(i)),
			Segments:    segs,
		}
		tracks[i].Width = tracks[i].Height * 16 / 9
	}
	return tracks, nil
}

// basePTS is the un-jittered PTS of boundary b expressed in the given
// timescale; originMsRep is the rendition's first-boundary time in ms.
func basePTS(originMsRep, b, segSec, ts int64) int64 {
	return originMsRep*ts/1000 + b*segSec*ts
}

// jitterMs returns a deterministic jitter in [-4, 4] ms.
func jitterMs(seed, rep, boundary int64) int64 {
	x := uint64(seed)*0x100000001b3 ^ uint64(rep+1)*0x9e3779b1 ^ uint64(boundary)*0x85ebca77
	return int64(splitmix64(x)%9) - 4
}

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// RenderSegment renders the deterministic on-disk content of one synthetic
// segment: a metadata header line plus a short pseudo payload.
func RenderSegment(m SegmentMeta, seed int64) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "DPK1 number=%d ts=%d pts=%d dur=%d br=%d\n",
		m.Number, m.Timescale, m.PTS, m.Duration, m.Bitrate)
	x := splitmix64(uint64(seed) ^ uint64(m.PTS)*0x9e3779b97f4a7c15 ^ uint64(m.Number))
	for k := 0; k < 4; k++ {
		x = splitmix64(x)
		fmt.Fprintf(&b, "%016x", x)
	}
	b.WriteString("\n")
	return []byte(b.String())
}
