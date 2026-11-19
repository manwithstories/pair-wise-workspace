// Package pipeline wires the archivist stages together: parse, scan, reorder,
// unwrap and plan. It owns no CLI concerns so it can be tested directly.
package pipeline

import (
	"fmt"
	"path/filepath"
	"time"

	"archivearchivist/internal/clock"
	"archivearchivist/internal/graph"
	"archivearchivist/internal/model"
	"archivearchivist/internal/mp4"
	"archivearchivist/internal/recover"
	"archivearchivist/internal/report"
	"archivearchivist/internal/rtp"
)

// Input locates the archive documents.
type Input struct {
	TimelinePath string
	CapturePath  string
	// BaseDir resolves relative track sources; defaults to the timeline's
	// directory.
	BaseDir string
}

// Options tunes scanning and recovery.
type Options struct {
	Recover recover.Config
	Window  int
}

// Analysis is everything inspect and recover report.
type Analysis struct {
	Timeline *model.Timeline      `json:"timeline"`
	Scans    []*mp4.Scan          `json:"scans"`
	Capture  *rtp.Report          `json:"capture,omitempty"`
	Clocks   map[string]ClockView `json:"clocks"`
	Graph    *graph.Graph         `json:"-"`
	Plan     recover.Result       `json:"plan"`
	Metrics  report.Metrics       `json:"metrics"`
	Faults   model.FaultList      `json:"faults"`

	clockResults map[string]clock.Result
	scanByTrack  map[string]*mp4.Scan
}

// Load parses the timeline and scans every referenced source. A track whose
// source is missing yields a fault and a nil scan rather than aborting.
func Load(in Input) (*Analysis, error) {
	tl, err := model.LoadTimeline(in.TimelinePath)
	if err != nil {
		return nil, err
	}
	base := in.BaseDir
	if base == "" {
		base = filepath.Dir(in.TimelinePath)
	}
	a := &Analysis{Timeline: tl, Clocks: map[string]ClockView{}}
	// Several tracks legitimately share one master file; scan each file once
	// and hand every referring track its own view of the same result.
	cache := map[string]*mp4.Scan{}
	for _, t := range tl.Tracks {
		path := t.Source
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		if cached, ok := cache[path]; ok {
			a.scanByTrack[t.ID] = cached
			continue
		}
		sc, err := mp4.ScanFile(path, t.ID)
		if err != nil {
			a.Faults.Add(model.Fault{
				Code: model.CodeInvalidInput, Severity: model.SevFatal, Track: t.ID,
				Detail: fmt.Sprintf("cannot read source %s: %v", path, err),
			})
			continue
		}
		if a.scanByTrack == nil {
			a.scanByTrack = map[string]*mp4.Scan{}
		}
		cache[path] = sc
		a.scanByTrack[t.ID] = sc
		a.Scans = append(a.Scans, sc)
		a.Faults.Add(sc.Faults...)
	}
	return a, nil
}

// Run executes the full analysis. It is safe to call repeatedly: every stage
// works on fresh copies and the report is deterministic for the same input.
// The wall-clock duration of the whole pipeline is recorded as a single-run
// latency sample unless Benchmark overwrites it with repeated measurements.
func Run(in Input, opts Options) (*Analysis, error) {
	started := time.Now()
	a, err := Load(in)
	if err != nil {
		return nil, err
	}
	window := opts.Window
	if window <= 0 {
		window = a.Timeline.Capture.Window
	}
	if window <= 0 {
		window = 64
	}

	// Capture: reorder, dedup and account for loss.
	if in.CapturePath != "" {
		pkts, err := rtp.ReadCapture(in.CapturePath)
		if err != nil {
			return nil, fmt.Errorf("capture: %w", err)
		}
		rep := rtp.Reorder(pkts, rtp.Options{Window: window})
		a.Capture = &rep
		a.Faults.Add(rep.Faults...)
	}

	// Clock: unwrap the transport stream that feeds the first track with a
	// declared modulus, and record its normalized head for boundary sync.
	rate := a.Timeline.Capture.ClockRate
	if rate == 0 {
		rate = 90000
	}
	if a.Capture != nil && len(a.Capture.Stream) > 0 {
		samples := make([]clock.Sample, 0, len(a.Capture.Stream))
		for _, p := range a.Capture.Stream {
			ts := int64(p.Timestamp)
			if p.HasExt {
				ts = p.ExtTimestamp
			}
			samples = append(samples, clock.Sample{
				Seq: uint32(p.Seq), Orig: ts,
				Arrival: p.ArrivalUS, HasArr: p.ArrivalUS != 0,
			})
		}
		// The modulus follows the track that owns the capture: a 32-bit RTP
		// timestamp field wraps at 2^32 unless the sender supplied the full
		// 33-bit clock in a header extension.
		track := captureTrack(a.Timeline)
		modulus := model.Modulus33
		if tr := a.Timeline.TrackByID(track); tr != nil {
			modulus = tr.Modulus()
		}
		if !captureHasExtension(a.Capture.Stream) {
			modulus = 1 << 32
		}
		res := clock.Unwrap(samples, clock.Options{ClockRate: rate, Modulus: modulus})
		a.Clocks[track] = ClockView{
			Track: track, Wraps: res.Wraps, DriftPPM: res.DriftPPM,
			DriftReview: res.DriftExceeded, MaxSkewUS: res.MaxSkewUS,
			Regressions: res.Regressions,
		}
		v := a.Clocks[track]
		for i, p := range res.Points {
			if i >= maxClockPoints {
				v.PointsTruncated = true
				break
			}
			v.Points = append(v.Points, ClockPoint{Seq: p.Seq, Orig: p.Orig, Norm: p.Norm, Epoch: p.Epoch, Wrapped: p.Wrapped})
		}
		if len(res.Points) > 0 {
			v.OriginalHead = res.Points[0].Orig
			v.NormalizedHead = res.Points[0].Norm
		}
		a.Clocks[track] = v
		a.clockResults = map[string]clock.Result{track: res}
		a.Faults.Add(res.Faults...)
	}

	g, gf := graph.Build(a.Timeline)
	a.Graph = g
	a.Faults.Add(gf...)

	a.Plan = recover.Run(recover.Input{
		Timeline: a.Timeline, Scans: a.scanByTrack, Capture: a.Capture,
		Clocks: a.clockResults, Graph: g,
	}, opts.Recover)
	a.Faults.Add(a.Plan.Faults...)

	a.Metrics = summarize(a)
	a.Metrics.Iterations = 1
	a.Metrics.LatencyP95US = time.Since(started).Microseconds()
	a.Metrics.LatencyMeanUS = a.Metrics.LatencyP95US
	a.Faults.Sort()
	return a, nil
}
