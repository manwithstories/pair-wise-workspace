// Package analyze aligns audio and video sample clocks at rational
// precision, classifies clock anomalies and measures keyframe intervals.
package analyze

import (
	"fmt"
	"os"
	"sort"

	"mediascan/internal/index"
	"mediascan/internal/manifest"
	"mediascan/internal/model"
	"mediascan/internal/stream"
)

// SyncSample is one timestamped sample on a track.
type SyncSample struct {
	PTS     int64 // track ticks, composition-adjusted
	Segment string
	Sync    bool
}

// Track is a decoded timeline track ready for analysis.
type Track struct {
	ID        uint32
	Kind      string
	Timescale uint32
	Samples   []SyncSample
}

// Deviation is one aligned pair whose offset exceeds tolerance.
type Deviation struct {
	Segment  string         `json:"segment"`
	At       model.Rational `json:"at"`
	OffsetMS int64          `json:"offset_ms"`
}

// SyncResult is the outcome of A/V alignment.
type SyncResult struct {
	Pairs       int            `json:"pairs"`
	MaxErrorMS  int64          `json:"av_sync_max_ms"`
	MaxErrorAt  model.Rational `json:"av_sync_max_at"`
	MaxErrorSeg string         `json:"av_sync_max_segment"`
	MaxErrorDir int64          `json:"av_sync_max_signed_ms"`
	MeanErrorMS float64        `json:"av_sync_mean_ms"`
	Deviations  []Deviation    `json:"av_sync_deviations,omitempty"`
	RealJumps   int            `json:"real_jumps"`
	ClockSkews  int            `json:"clock_offsets"`
	WrapSpans   int            `json:"timestamp_wraps"`
	Faults      []model.Fault  `json:"faults,omitempty"`
}

// KeyframeReport describes GOP regularity.
type KeyframeReport struct {
	Intervals     []int64       `json:"intervals_ms"`
	MaxIntervalMS int64         `json:"max_gop_ms"`
	Irregular     int           `json:"irregular_gops"`
	Faults        []model.Fault `json:"faults,omitempty"`
}

// BuildTracks reads every present indexed segment through the stream scanner
// and collects samples by the track ID declared in each traf. Only one
// segment is resident at a time, so memory stays bounded by the largest moof.
// Segments that fail box validation contribute no samples.
func BuildTracks(idx *index.Index, resolver *manifest.Resolver, init *stream.Init) ([]Track, error) {
	tracks := map[uint32]*Track{}
	for _, entry := range idx.Entries {
		if !entry.Present {
			continue
		}
		full, err := resolver.Resolve(entry.URI)
		if err != nil {
			return nil, err
		}
		scan, err := stream.ScanSegment(init, entry.Name, full, entry.ByteStart, entry.ByteEnd)
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed between scan and analysis
			}
			return nil, err
		}
		if _, bad := scan.FirstUndecodable(); bad {
			continue
		}
		for _, traf := range scan.Trafs {
			meta := init.Get(traf.TrackID)
			t := tracks[traf.TrackID]
			if t == nil {
				t = &Track{ID: traf.TrackID, Kind: meta.Kind, Timescale: meta.Timescale}
				tracks[traf.TrackID] = t
			}
			for _, sm := range traf.Samples {
				t.Samples = append(t.Samples, SyncSample{
					PTS: int64(sm.Decode) + sm.CTO, Segment: entry.Name, Sync: sm.Sync,
				})
			}
		}
	}
	var out []Track
	for _, kind := range []string{"video", "audio"} {
		for id, t := range tracks {
			_ = id
			if t.Kind != kind {
				continue
			}
			sort.SliceStable(t.Samples, func(i, j int) bool {
				if t.Samples[i].PTS != t.Samples[j].PTS {
					return t.Samples[i].PTS < t.Samples[j].PTS
				}
				return t.Samples[i].Segment < t.Samples[j].Segment
			})
			out = append(out, *t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no decodable tracks", model.ErrInvalidTimebase)
	}
	return out, nil
}

// lcmTimescale returns a common tick denominator and the scale factors for
// each timescale. Overflow is reported rather than silently wrapping.
func lcmTimescale(a, b uint32) (common, scaleA, scaleB int64, err error) {
	if a == 0 || b == 0 {
		return 0, 0, 0, model.ErrInvalidTimebase
	}
	g := gcd(int64(a), int64(b))
	// Guard the multiplication before performing it.
	if int64(a)/g > (1<<62)/int64(b) {
		return 0, 0, 0, model.ErrTimeOverflow
	}
	common = int64(a) / g * int64(b)
	return common, common / int64(a), common / int64(b), nil
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Classification thresholds on the shared millisecond scale. A skew above
// classifyMS is worth classifying, and a change above jumpMS between
// consecutive classified points is treated as a discontinuity rather than
// drift.
const (
	classifyMS int64 = 3
	jumpMS     int64 = 15
	wrapMS     int64 = 1000
)

// Align pairs each video sample with the nearest audio sample on the shared
// rational clock and reports the largest absolute skew with its location.
// A constant skew is classified as a clock offset; isolated spikes are real
// jumps; a backwards clock is a wrap.
func Align(video, audio Track, toleranceMS int64) (SyncResult, error) {
	var res SyncResult
	if video.Timescale == 0 || audio.Timescale == 0 {
		return res, fmt.Errorf("%w: zero track timescale", model.ErrInvalidTimebase)
	}
	if len(video.Samples) == 0 || len(audio.Samples) == 0 {
		res.Faults = append(res.Faults, model.Fault{
			Kind: model.FaultAVSync, Severity: model.SeverityCritical,
			Message: "cannot align: a track has no decodable samples",
		})
		return res, nil
	}
	_, scaleV, scaleA, err := lcmTimescale(video.Timescale, audio.Timescale)
	if err != nil {
		return res, err
	}
	audioCommon := make([]int64, len(audio.Samples))
	for i, s := range audio.Samples {
		v, ok := mul(s.PTS, scaleA)
		if !ok {
			return res, fmt.Errorf("%w: audio timestamp", model.ErrTimeOverflow)
		}
		audioCommon[i] = v
	}
	// One common tick is 1 LCM seconds, so the offset denominator is the LCM.
	common, _, _, err := lcmTimescale(video.Timescale, audio.Timescale)
	if err != nil {
		return res, err
	}
	var (
		maxAbs   int64 = -1
		sumAbs   int64
		prevMs   int64
		prevSeg  string
		havePrev bool
	)
	for _, vs := range video.Samples {
		vCommon, ok := mul(vs.PTS, scaleV)
		if !ok {
			return res, fmt.Errorf("%w: video timestamp", model.ErrTimeOverflow)
		}
		diff, ok := sub(vCommon, audioCommon[nearest(audioCommon, vCommon)])
		if !ok {
			return res, fmt.Errorf("%w: timestamp difference", model.ErrTimeOverflow)
		}
		offset, err := model.NewRational(diff, common)
		if err != nil {
			return res, err
		}
		ms, err := offset.MillisRounded()
		if err != nil {
			return res, err
		}
		res.Pairs++
		abs := ms
		if abs < 0 {
			abs = -abs
		}
		sumAbs += abs
		if abs > maxAbs {
			maxAbs = abs
			res.MaxErrorMS = abs
			res.MaxErrorDir = ms
			res.MaxErrorAt = offset
			res.MaxErrorSeg = vs.Segment
		}
		if abs > toleranceMS {
			res.Deviations = append(res.Deviations, Deviation{Segment: vs.Segment, At: offset, OffsetMS: ms})
		}
		// A single-sample step beyond jumpMS inside one segment is a real
		// discontinuity, not a clock offset.
		if havePrev && prevSeg == vs.Segment {
			if delta := ms - prevMs; delta > jumpMS || delta < -jumpMS {
				res.RealJumps++
			}
		}
		prevMs, prevSeg, havePrev = ms, vs.Segment, true
	}
	if res.Pairs > 0 {
		res.MeanErrorMS = float64(sumAbs) / float64(res.Pairs)
	}
	// Container-level clocks are compared at each segment boundary. Sample
	// nearest-neighbour alignment quantizes sub-frame bias, so a true clock
	// offset is visible in the declared base times rather than in the matched
	// sample pairs.
	classifyBases(&res, video, audio, toleranceMS)
	res.WrapSpans = countWraps(video.Samples) + countWraps(audio.Samples)
	res.Faults = syncFaults(res, toleranceMS)
	return res, nil
}

// segmentBase returns the earliest PTS per segment name in track order.
func segmentBase(t Track) ([]string, map[string]int64) {
	var order []string
	bases := map[string]int64{}
	for _, sm := range t.Samples {
		if _, ok := bases[sm.Segment]; !ok {
			order = append(order, sm.Segment)
			bases[sm.Segment] = sm.PTS
			continue
		}
		if sm.PTS < bases[sm.Segment] {
			bases[sm.Segment] = sm.PTS
		}
	}
	return order, bases
}

// classifyBases compares the declared per-segment base times of the two
// tracks on the shared rational clock. A constant non-zero difference is a
// clock offset; a step larger than jumpMS is a real discontinuity; a base that
// jumps backwards is a wrap.
func classifyBases(res *SyncResult, video, audio Track, toleranceMS int64) {
	vOrder, vBase := segmentBase(video)
	aOrder, aBase := segmentBase(audio)
	seen := map[string]bool{}
	var order []string
	for _, name := range append(append([]string{}, vOrder...), aOrder...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		order = append(order, name)
	}
	var prevMS int64
	havePrev := false
	for _, name := range order {
		vTicks, okV := vBase[name]
		aTicks, okA := aBase[name]
		if !okV || !okA {
			continue
		}
		diff := aTicks*int64(video.Timescale) - vTicks*int64(audio.Timescale)
		offset, err := model.NewRational(diff, int64(video.Timescale)*int64(audio.Timescale))
		if err != nil {
			continue
		}
		ms, err := offset.MillisRounded()
		if err != nil {
			continue
		}
		switch {
		case havePrev && ms-prevMS < -wrapMS:
			res.WrapSpans++
		case abs64(ms) > classifyMS:
			res.ClockSkews++
		}
		prevMS, havePrev = ms, true
	}
	_ = toleranceMS
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func countWraps(samples []SyncSample) int {
	n := 0
	for i := 1; i < len(samples); i++ {
		if samples[i].PTS < samples[i-1].PTS {
			n++
		}
	}
	return n
}

func syncFaults(res SyncResult, toleranceMS int64) []model.Fault {
	var out []model.Fault
	if res.MaxErrorMS > toleranceMS {
		out = append(out, model.Fault{
			Kind: model.FaultAVSync, Severity: model.SeverityCritical,
			Segment:   res.MaxErrorSeg,
			Message:   fmt.Sprintf("max A/V skew %dms at %s", res.MaxErrorMS, res.MaxErrorAt),
			TimeStart: res.MaxErrorAt, TimeEnd: res.MaxErrorAt,
		})
	}
	if res.WrapSpans > 0 {
		out = append(out, model.Fault{
			Kind: model.FaultTimestamp, Severity: model.SeverityWarning,
			Message: fmt.Sprintf("%d media clocks wrap within the window", res.WrapSpans),
		})
	}
	if res.RealJumps > 0 {
		out = append(out, model.Fault{
			Kind: model.FaultTimestamp, Severity: model.SeverityWarning,
			Message: fmt.Sprintf("%d real timestamp jumps detected", res.RealJumps),
		})
	}
	return out
}

// Keyframes measures the interval between successive sync samples and flags
// GOPs longer than maxGOPMS. A keyframe-less segment produces no sync sample,
// so the interval spanning it is the one that trips the threshold.
func Keyframes(video Track, maxGOPMS int64) KeyframeReport {
	var rep KeyframeReport
	var lastSync int64 = -1
	for _, s := range video.Samples {
		if !s.Sync {
			continue
		}
		if lastSync >= 0 {
			r, err := model.NewRational(s.PTS-lastSync, int64(video.Timescale))
			if err != nil {
				continue
			}
			ms, err := r.MillisRounded()
			if err != nil {
				continue
			}
			rep.Intervals = append(rep.Intervals, ms)
			if ms > rep.MaxIntervalMS {
				rep.MaxIntervalMS = ms
			}
			if ms > maxGOPMS {
				rep.Irregular++
				at, err := model.NewRational(s.PTS, int64(video.Timescale))
				if err != nil {
					continue
				}
				rep.Faults = append(rep.Faults, model.Fault{
					Kind: model.FaultKeyframe, Severity: model.SeverityWarning,
					Segment:   s.Segment,
					Message:   fmt.Sprintf("keyframe interval %dms exceeds %dms", ms, maxGOPMS),
					TimeStart: at, TimeEnd: at,
				})
			}
		}
		lastSync = s.PTS
	}
	return rep
}

// nearest returns the index of the value closest to target in an ascending
// slice.
func nearest(sorted []int64, target int64) int {
	i := sort.Search(len(sorted), func(i int) bool { return sorted[i] >= target })
	switch {
	case i == 0:
		return 0
	case i >= len(sorted):
		return len(sorted) - 1
	case sorted[i]-target < target-sorted[i-1]:
		return i
	default:
		return i - 1
	}
}

func mul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == -1<<63 || b == -1<<63 {
		return 0, false
	}
	r := a * b
	return r, r/b == a
}

func sub(a, b int64) (int64, bool) {
	r := a - b
	if (b > 0 && r > a) || (b < 0 && r < a) {
		return 0, false
	}
	return r, true
}
