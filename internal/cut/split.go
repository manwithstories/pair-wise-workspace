// Package cut plans legal cut boundaries and splits timed sample streams into
// half-open clip segments.
package cut

import (
	"math"
	"sort"

	"fieldcut/internal/clock"
	"fieldcut/internal/ingest"
)

// CandidateKind explains why a cut point was chosen.
type CandidateKind string

const (
	CandidateChapter      CandidateKind = "chapter"
	CandidateSilence      CandidateKind = "silence"
	CandidateRandomAccess CandidateKind = "random_access"
	CandidateErrorAnchor  CandidateKind = "error_anchor"
)

// Candidate is a proposed boundary from a chapter, silence interval or sync
// sample.
type Candidate struct {
	Time clock.Ns
	Kind CandidateKind
	Note string
}

// Point is a boundary after legal-sample rounding.
type Point struct {
	Requested clock.Ns      `json:"requested_ns"`
	Ns        clock.Ns      `json:"ns"`
	SkewNs    clock.Ns      `json:"skew_ns"`
	Kind      CandidateKind `json:"kind"`
	Note      string        `json:"note,omitempty"`
}

// Request configures deterministic boundary planning.
type Request struct {
	TrackID         int
	Duration        clock.Ns
	Candidates      []Candidate
	AudioSampleRate int
	VideoFPS        int // zero means audio-sample grid only
	Errors          []*clock.Error
}

// Plan contains boundaries, half-open segments and propagated errors.
type Plan struct {
	TrackID       int              `json:"track_id"`
	Points        []Point          `json:"points"`
	Segments      []clock.Interval `json:"segments"`
	MaxSkewNs     clock.Ns         `json:"max_skew_ns"`
	Errors        []*clock.Error   `json:"errors,omitempty"`
	ErrorAnchorNs []clock.Ns       `json:"cut_point_error_ns,omitempty"`
}

// PlanCuts sorts candidates, snaps them to legal samples and builds half-open
// segments. Input alignment errors are propagated into the plan.
func PlanCuts(req Request) (*Plan, error) {
	if req.Duration <= 0 {
		return nil, clock.Errorf(clock.CodeInvalidArgument, req.TrackID, "cut duration must be positive")
	}
	if req.AudioSampleRate <= 0 {
		return nil, clock.Errorf(clock.CodeConfig, req.TrackID, "audio sample rate must be positive")
	}
	raw := append([]Candidate(nil), req.Candidates...)
	// Error intervals are first-class anchors so cross-track timing faults are
	// visible to the cutter and cannot disappear before manifest writing.
	errorAnchors := make([]clock.Ns, 0, len(req.Errors))
	for _, e := range req.Errors {
		if e == nil {
			continue
		}
		if e.StartNs >= 0 && e.StartNs <= req.Duration {
			raw = append(raw, Candidate{Time: e.StartNs, Kind: CandidateErrorAnchor, Note: string(e.Code)})
			errorAnchors = append(errorAnchors, e.StartNs)
		}
		if e.EndNs > e.StartNs && e.EndNs <= req.Duration {
			raw = append(raw, Candidate{Time: e.EndNs, Kind: CandidateErrorAnchor, Note: string(e.Code)})
			errorAnchors = append(errorAnchors, e.EndNs)
		}
	}
	sort.SliceStable(raw, func(i, j int) bool {
		if raw[i].Time != raw[j].Time {
			return raw[i].Time < raw[j].Time
		}
		return raw[i].Kind < raw[j].Kind
	})

	points := []Point{{Requested: 0, Ns: 0, Kind: "start"}}
	var maxSkew clock.Ns
	for _, c := range raw {
		if c.Time <= 0 || c.Time >= req.Duration {
			continue
		}
		legal, skew, err := snap(c.Time, req.AudioSampleRate, req.VideoFPS)
		if err != nil {
			return nil, err
		}
		if legal <= points[len(points)-1].Ns || legal >= req.Duration {
			continue
		}
		points = append(points, Point{Requested: c.Time, Ns: legal, SkewNs: skew, Kind: c.Kind, Note: c.Note})
		if skew > maxSkew {
			maxSkew = skew
		}
	}
	end, endSkew, err := snap(req.Duration, req.AudioSampleRate, req.VideoFPS)
	if err != nil {
		return nil, err
	}
	if end > req.Duration {
		end = req.Duration
	}
	points = append(points, Point{Requested: req.Duration, Ns: end, SkewNs: absNs(req.Duration - end), Kind: "end"})
	if endSkew > maxSkew {
		maxSkew = endSkew
	}

	segments := make([]clock.Interval, 0, len(points)-1)
	for i := 0; i+1 < len(points); i++ {
		iv := clock.Interval{Start: points[i].Ns, End: points[i+1].Ns}
		if err := iv.Valid(); err != nil {
			return nil, err
		}
		segments = append(segments, iv)
	}
	sort.SliceStable(errorAnchors, func(i, j int) bool { return errorAnchors[i] < errorAnchors[j] })
	return &Plan{
		TrackID: req.TrackID, Points: points, Segments: segments, MaxSkewNs: maxSkew,
		Errors: append([]*clock.Error(nil), req.Errors...), ErrorAnchorNs: errorAnchors,
	}, nil
}

func absNs(v clock.Ns) clock.Ns {
	if v < 0 {
		return -v
	}
	return v
}

func snap(t clock.Ns, audioRate, fps int) (clock.Ns, clock.Ns, error) {
	target := t
	if fps > 0 {
		frame := int64(math.Round(float64(t) * float64(fps) / float64(clock.NsPerSecond)))
		frameNs, err := clock.TicksToNs(frame, 1, int64(fps))
		if err != nil {
			return 0, 0, err
		}
		target = frameNs
	}
	_, sampleNs, err := clock.RoundToSample(target, audioRate)
	if err != nil {
		return 0, 0, err
	}
	skew := absNs(sampleNs - t)
	return sampleNs, skew, nil
}

// TimedSample is a sample placed on the unified timeline.
type TimedSample struct {
	PTS      clock.Ns
	Duration clock.Ns
	Sync     bool
	Data     []byte
}

// SplitSamples assigns each sample by PTS to its containing half-open segment.
func SplitSamples(samples []TimedSample, segments []clock.Interval) ([][]TimedSample, []int) {
	out := make([][]TimedSample, len(segments))
	counts := make([]int, len(segments))
	for _, s := range samples {
		idx := sort.Search(len(segments), func(i int) bool { return s.PTS < segments[i].End })
		if idx < len(segments) && s.PTS >= segments[idx].Start && s.PTS < segments[idx].End {
			out[idx] = append(out[idx], s)
			counts[idx]++
		}
	}
	return out, counts
}

// Coverage verifies that segments tile [0,duration) without gaps or overlaps.
func Coverage(segments []clock.Interval, duration clock.Ns) error {
	if len(segments) == 0 {
		return clock.Errorf(clock.CodeCoverage, -1, "no cut segments")
	}
	if segments[0].Start != 0 {
		return clock.Errorf(clock.CodeCoverage, -1, "first segment starts at %d", segments[0].Start)
	}
	for i := 1; i < len(segments); i++ {
		if segments[i].Start != segments[i-1].End {
			return &clock.Error{Code: clock.CodeCoverage, TrackID: -1,
				StartNs: segments[i-1].End, EndNs: segments[i].Start,
				Message: "segment coverage gap or overlap"}
		}
	}
	if last := segments[len(segments)-1].End; last != duration {
		return clock.Errorf(clock.CodeCoverage, -1, "coverage ends at %d, want %d", last, duration)
	}
	return nil
}

// PackageFragment encapsulates one cut window's samples as a deterministic
// fragmented MP4. Keeping the wrapper here makes split-and-package one stage.
func PackageFragment(trackID int, timeScale uint32, color ingest.ColorInfo, samples []ingest.MP4Sample) ([]byte, error) {
	return ingest.BuildFragmentedMP4(&ingest.MP4Track{
		TrackID: trackID, TimeScale: timeScale, Color: color, Samples: samples,
	})
}
