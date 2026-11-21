// Package analyze aligns audio and video sample clocks at rational
// precision, classifies clock anomalies and measures keyframe intervals.
package analyze

import (
	"fmt"
	"sort"

	"mediascan/internal/index"
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
// and collects per-track samples. Only one segment is resident at a time.
func BuildTracks(idx *index.Index, resolver interface {
	Resolve(string) (string, error)
}, init *stream.Init) ([]Track, error) {
	tracks := map[string]*Track{}
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
			return nil, err
		}
		if _, bad := scan.FirstUndecodable(); bad {
			continue // a corrupt segment contributes no trustworthy samples
		}
		kind := kindFor(entry.StreamID)
		if kind == "unknown" {
			continue
		}
		t := tracks[kind]
		if t == nil {
			t = &Track{Kind: kind, Timescale: entry.Timescale}
			tracks[kind] = t
		}
		if t.Timescale == 0 {
			t.Timescale = entry.Timescale
		}
		for _, traf := range scan.Trafs {
			if traf.TrackID != 0 {
				t.ID = traf.TrackID
			}
			for _, s := range traf.Samples {
				pts := int64(s.Decode) + s.CTO
				t.Samples = append(t.Samples, SyncSample{PTS: pts, Segment: entry.Name, Sync: s.Sync})
			}
		}
	}
	var out []Track
	for _, k := range []string{"video", "audio"} {
		if t := tracks[k]; t != nil {
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

func kindFor(streamID string) string {
	switch streamID {
	case "video", "audio":
		return streamID
	}
	return "unknown"
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
	jumpMS     int64 = 20
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
		havePrev bool
	)
	for _, vs := range video.Samples {
		vCommon, ok := mul(vs.PTS, scaleV)
		if !ok {
			return res, fmt.Errorf("%w: video timestamp", model.ErrTimeOverflow)
		}
		ai := nearest(audioCommon, vCommon)
		diff, ok := sub(vCommon, audioCommon[ai])
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
		// Classification uses a finer threshold than reporting so a small but
		// persistent offset is still recognized as clock drift, while an
		// isolated change of more than one video frame is a real jump.
		if abs > classifyMS {
			if havePrev {
				switch delta := ms - prevMs; {
				case delta > jumpMS || delta < -jumpMS:
					res.RealJumps++
				default:
					res.ClockSkews++
				}
			}
			prevMs, havePrev = ms, true
		}
	}
	if res.Pairs > 0 {
		res.MeanErrorMS = float64(sumAbs) / float64(res.Pairs)
	}
	res.WrapSpans = countWraps(video.Samples) + countWraps(audio.Samples)
	res.Faults = syncFaults(res, toleranceMS)
	return res, nil
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
// GOPs longer than maxGOPMS.
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
