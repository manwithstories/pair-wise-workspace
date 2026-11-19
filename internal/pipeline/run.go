// Package pipeline wires the recovery stages together in dependency order:
// scan, index repair, clock recovery, dependency planning, segmentation and
// ladder alignment. It owns the run configuration and the metric computation so
// the CLI stays a thin argument parser.
package pipeline

import (
	"fmt"
	"sort"

	"archive.local/recover/internal/align"
	"archive.local/recover/internal/clock"
	"archive.local/recover/internal/errs"
	"archive.local/recover/internal/graph"
	"archive.local/recover/internal/index"
	"archive.local/recover/internal/media"
	"archive.local/recover/internal/model"
	"archive.local/recover/internal/report"
	"archive.local/recover/internal/segment"
	"archive.local/recover/internal/stream"
	"archive.local/recover/internal/testkit"
)

// Config is the effective, digestable run configuration.
type Config struct {
	Seed            int64   `json:"seed"`
	DurationNs      int64   `json:"duration_ns"`
	Bitrates        []int   `json:"bitrates"`
	Loss            float64 `json:"loss"`
	Case            string  `json:"case"`
	TargetSegmentNs int64   `json:"target_segment_ns"`
	MaxBoundaryNs   int64   `json:"max_boundary_ns"`
	MaxAlignNs      int64   `json:"max_alignment_ns"`
	WrapBits        uint    `json:"wrap_bits"`
	BreakNs         int64   `json:"drift_break_ns"`
}

// DefaultConfig mirrors the acceptance demo.
func DefaultConfig() Config {
	return Config{
		Seed: 17, DurationNs: 120_000_000_000,
		Bitrates: []int{800, 1600, 3200}, Loss: 0.03, Case: testkit.CaseSuccess,
		TargetSegmentNs: 4_000_000_000, MaxBoundaryNs: 40_000_000, MaxAlignNs: 40_000_000,
		WrapBits: 32, BreakNs: 20_000_000,
	}
}

// Result is everything a report needs.
type Result struct {
	Report *report.Report
	// LatencySamplesMs are per-packet reassembly latencies used for the P95.
	LatencySamplesMs []float64
}

// Run executes the full demo pipeline for the configured case.
func Run(cfg Config) (*Result, error) {
	fx, err := testkit.Generate(testkit.Options{
		Seed: cfg.Seed, DurationNs: cfg.DurationNs, Bitrates: cfg.Bitrates, Loss: cfg.Loss, Case: cfg.Case,
	})
	if err != nil {
		return nil, err
	}
	rep := report.New(cfg.Case, cfg.Seed)
	res := &Result{Report: rep}

	bitrateCount := len(fx.Renditions)
	rep.Timeline = &report.TimelineReport{Tracks: 2 * bitrateCount, DurationNs: cfg.DurationNs}
	rep.Issues = append(rep.Issues, errs.List{}...)

	var (
		syncErrors []float64
		rends      []align.Rendition
		totalSegs  int
	)

	for ri := range fx.Renditions {
		rd := &fx.Renditions[ri]
		masterName := fmt.Sprintf("%s.master.mp4", rd.Name)
		idxName := fmt.Sprintf("%s.keyidx", rd.Name)

		// Stage 1: probe the master and scan its video payload.
		pf, err := media.ParseFile(media.Reader(rd.Master), int64(len(rd.Master)))
		if err != nil {
			rep.Issues.AddFatal(errs.ClassInternal, errs.CodeNALUnknownCodec,
				fmt.Sprintf("%s: %v", masterName, err), errs.Location{File: masterName, Sample: -1, Offset: -1})
			continue
		}
		scan, err := stream.Scan(stream.H264, pf.Data)
		if err != nil {
			rep.Issues.AddFatal(errs.ClassInternal, errs.CodeNALUnknownCodec,
				fmt.Sprintf("%s: %v", masterName, err), errs.Location{File: masterName, Sample: -1, Offset: -1})
			continue
		}
		rep.Streams = append(rep.Streams, report.StreamSummary{
			File: masterName, Codec: "h264", Units: len(scan.Units),
			Keyframes: len(scan.Keyframes), Bytes: scan.Bytes, Truncated: scan.Truncated,
		})

		// Stage 2: parse and validate the sidecar index.
		parsed := index.Parse(rd.Index, idxName)
		repair := index.Validate(&parsed.Parsed, scan, idxName)
		collectIssues(rep, repair, idxName)
		if rep.Index == nil {
			rep.Index = &report.IndexReport{
				File: idxName, Original: repair.Original, Repaired: repair.Repaired,
				Pending: repair.Pending, Dropped: repair.Dropped, Missing: repair.Missing,
			}
		}

		// Stage 3: dependency plan for this rendition's two tracks.
		g, err := graph.New([]graph.Node{
			{ID: 1, Kind: model.KindVideo},
			{ID: 2, Kind: model.KindAudio},
		}, masterName)
		if err != nil {
			rep.Issues.AddFatal(errs.ClassInternal, errs.CodeDependencyMissing, err.Error(), errs.UnknownLocation())
			continue
		}
		plan := g.Sort()
		rep.Tracks = tracksReport(rep.Tracks, plan)
		if !plan.Recoverable(1) {
			// Video branch stopped; skip its contribution but keep the report.
			continue
		}

		// Stage 4: recover the video timeline.
		video := recoverTrack(clock.Input{
			TrackID: 1, Kind: model.KindVideo, Timescale: rd.VideoTimescale,
			RawPTS: rd.Clock, Keyframes: sampleKeyframes(scan), Anchors: rd.Anchors,
			WrapBits: cfg.WrapBits, BreakNs: cfg.BreakNs, File: masterName,
		}, rep, masterName)
		if video == nil {
			continue
		}
		audio := recoverTrack(clock.Input{
			TrackID: 2, Kind: model.KindAudio, Timescale: rd.AudioTimescale,
			RawPTS: rd.AudioClock, Keyframes: allSamples(len(rd.AudioClock)), Anchors: rd.AudioAnchors,
			WrapBits: cfg.WrapBits, BreakNs: cfg.BreakNs, File: masterName,
		}, rep, masterName)
		if audio == nil {
			continue
		}

		// Recovered keyframe positions, cross-checked against the scan.
		kf := intersectKeyframes(video.Keyframes, sampleKeyframes(scan))
		if len(kf) == 0 {
			kf = sampleKeyframes(scan)
		}

		// Stage 5: segment on keyframe boundaries.
		seg := segment.Build(video, kf, segment.Config{
			TargetNs: cfg.TargetSegmentNs, MaxBoundaryNs: cfg.MaxBoundaryNs,
		})
		collectIssues(rep, &index.RepairResult{Issues: seg.Issues}, masterName)
		totalSegs += seg.SegmentCount
		rends = append(rends, align.Rendition{Name: rd.Name, Bitrate: rd.Bitrate, Segments: seg})

		syncErrors = append(syncErrors, syncErrorMs(video.PTS, rd.ProgramNs))
	}
	res.LatencySamplesMs = fx.RTP.LatencyMs()

	// Stage 6: align representations and build the manifest.
	ladder := align.Ladder(rends, align.Config{MaxDeltaNs: cfg.MaxAlignNs})
	collectIssues(rep, &index.RepairResult{Issues: ladder.Issues}, "ladder")
	rep.Alignment = &report.AlignReport{
		MaxDeltaNs: float64(ladder.MaxDeviationNs) / 1e6,
		Pending:    ladder.Pending, Boundaries: len(ladder.Boundaries),
	}
	rep.Segments = manifest(rends, ladder)
	if totalSegs > 0 && ladder.SegmentCount > 0 {
		rep.Metrics.Segments = ladder.SegmentCount
	} else {
		rep.Metrics.Segments = totalSegs
	}

	// Stage 7: RTP loss recovery metrics.
	st := testkit.RecoverRTP(fx.RTP)
	rep.RTP = &report.RTPReport{
		Received: st.Received, Duplicates: st.Duplicates, Reordered: st.Reordered,
		Unique: st.UniquePackets, Gaps: st.Gaps, Lost: st.LostPackets,
		RecoveryRate: st.RecoveryRate, PayloadBytes: st.Recovered,
	}

	rep.Metrics.SyncErrorMs = percentileMax(syncErrors)
	rep.Metrics.LossRecoveryRate = st.RecoveryRate
	rep.Metrics.AlignmentDeltaMs = float64(ladder.MaxDeviationNs) / 1e6
	rep.Metrics.P95LatencyMs = testkit.P95(res.LatencySamplesMs)
	rep.Issues.Sort()
	rep.Finalize()
	return res, nil
}

func collectIssues(rep *report.Report, r *index.RepairResult, file string) {
	for _, i := range r.Issues {
		if i.Loc.File == "" {
			i.Loc.File = file
		}
		rep.Issues = append(rep.Issues, i)
	}
}
func tracksReport(cur *report.TracksReport, plan *graph.Plan) *report.TracksReport {
	if cur == nil {
		cur = &report.TracksReport{}
	}
	cur.Order = append(cur.Order, plan.Order...)
	for _, s := range plan.Stopped {
		cur.Stopped = append(cur.Stopped, report.StoppedInfo{
			TrackID: s.TrackID, Kind: s.Kind, Code: s.Code, Reason: s.Reason, Related: s.Related,
		})
	}
	return cur
}

func recoverTrack(in clock.Input, rep *report.Report, file string) *model.TrackTimeline {
	r, err := clock.Unwrap(in)
	if err != nil {
		rep.Issues.AddFatal(errs.ClassInternal, errs.CodeClockAnchorBad,
			fmt.Sprintf("track %d: %v", in.TrackID, err),
			errs.Location{File: file, TrackID: in.TrackID, Kind: in.Kind, Sample: -1, Offset: -1})
		return nil
	}
	for _, i := range r.Issues {
		if i.Loc.File == "" {
			i.Loc.File = file
		}
		rep.Issues = append(rep.Issues, i)
	}
	if rep.Timeline != nil {
		rep.Timeline.Samples += len(r.Track.PTS)
	}
	return r.Track
}

func sampleKeyframes(scan *stream.Result) []int {
	out := make([]int, 0, len(scan.Keyframes))
	for _, u := range scan.Keyframes {
		out = append(out, u)
	}
	sort.Ints(out)
	return out
}

func allSamples(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func intersectKeyframes(a, b []int) []int {
	set := make(map[int]bool, len(b))
	for _, v := range b {
		set[v] = true
	}
	var out []int
	for _, v := range a {
		if set[v] {
			out = append(out, v)
		}
	}
	return out
}

// syncErrorMs reports the largest absolute difference between recovered
// program time and ground truth, in milliseconds.
func syncErrorMs(got, want []int64) float64 {
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	var worst int64
	for i := 0; i < n; i++ {
		d := got[i] - want[i]
		if d < 0 {
			d = -d
		}
		if d > worst {
			worst = d
		}
	}
	return float64(worst) / 1e6
}

func percentileMax(v []float64) float64 {
	var m float64
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func manifest(rends []align.Rendition, ladder *align.Result) []report.SegmentReport {
	if ladder == nil {
		return nil
	}
	var out []report.SegmentReport
	for _, b := range ladder.Boundaries {
		sr := report.SegmentReport{
			Index: b.Index, StartNs: b.ProgramNs,
			DeviationMs: float64(b.DeviationNs) / 1e6,
			Pending:     b.Pending,
			Renditions:  map[string]report.RenditionSegment{},
		}
		for _, r := range rends {
			actual, ok := b.Actual[r.Name]
			if !ok {
				continue
			}
			sample := -1
			if r.Segments != nil {
				for _, c := range r.Segments.Cuts {
					if c.StartNs == actual {
						sample = c.KeyframeSample
						sr.EndNs = c.EndNs
						break
					}
				}
			}
			sr.Renditions[r.Name] = report.RenditionSegment{
				Bitrate: r.Bitrate, StartNs: actual, KeyframeSample: sample,
			}
		}
		out = append(out, sr)
	}
	return out
}
