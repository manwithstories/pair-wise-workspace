// Package verify re-expands a manifest, reconciles it against on-disk
// segments and reports per-representation deviation statistics. It is the
// single source of truth for the JSON report emitted by both `pack
// --report` and `verify --json`.
package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"dashpack/internal/align"
	"dashpack/internal/errs"
	"dashpack/internal/ingest"
	"dashpack/internal/mpd"
)

// RepReport holds per-representation reconciliation results.
type RepReport struct {
	ID             string `json:"id"`
	Bandwidth      int64  `json:"bandwidth"`
	Segments       int    `json:"segments"`
	First          string `json:"first"`
	Last           string `json:"last"`
	MaxDeviationMs int64  `json:"max_deviation_ms"`
}

// Report is the top-level verification result.
type Report struct {
	MPD             string      `json:"mpd"`
	Addressing      string      `json:"addressing"`
	Representations int         `json:"representations"`
	SegmentsPerRep  int         `json:"segments_per_rep"`
	ExpandFirst     string      `json:"expand_first"`
	ExpandLast      string      `json:"expand_last"`
	MaxDeviationMs  int64       `json:"max_deviation_ms"`
	DriftCount      int         `json:"drift_count"`
	ToleranceMs     int64       `json:"tolerance_ms"`
	Status          string      `json:"status"`
	Reps            []RepReport `json:"reps"`
}

// Run verifies mpdPath against its sibling segment directories. It always
// returns the report when the manifest itself is readable; a non-nil error
// (of type *errs.AppError) carries the failing exit code.
func Run(mpdPath string, toleranceMs int64) (*Report, error) {
	rep := &Report{MPD: mpdPath, ToleranceMs: toleranceMs, Status: "ok"}
	data, err := os.ReadFile(mpdPath)
	if err != nil {
		return nil, errs.New(errs.ExitManifest, errs.CodeManifestParse,
			fmt.Sprintf("cannot read manifest: %v", err), map[string]any{"mpd": mpdPath})
	}
	m, err := mpd.Parse(data)
	if err != nil {
		return nil, errs.New(errs.ExitManifest, errs.CodeManifestParse, err.Error(), nil)
	}
	if err := mpd.CheckRoundTrip(m); err != nil {
		return nil, errs.New(errs.ExitManifest, errs.CodeRoundTrip, err.Error(), nil)
	}
	baseDir := filepath.Dir(mpdPath)
	for _, p := range m.Periods {
		for _, as := range p.AdaptationSets {
			for i := range as.Representations {
				rr, err := verifyRep(baseDir, &as.Representations[i], rep)
				if err != nil {
					return rep, err
				}
				rep.Reps = append(rep.Reps, *rr)
				if rr.MaxDeviationMs > rep.MaxDeviationMs {
					rep.MaxDeviationMs = rr.MaxDeviationMs
				}
			}
		}
	}
	rep.Representations = len(rep.Reps)
	if rep.Representations == 0 {
		return rep, errs.New(errs.ExitManifest, errs.CodeManifestParse, "manifest has no Representation", nil)
	}
	rep.SegmentsPerRep = rep.Reps[0].Segments
	rep.ExpandFirst = rep.Reps[0].First
	rep.ExpandLast = rep.Reps[0].Last
	if rep.DriftCount > 0 {
		rep.Status = "timestamp_regression"
		return rep, errs.New(errs.ExitAlign, errs.CodeTimestampRegress,
			fmt.Sprintf("%d timestamp regressions", rep.DriftCount),
			map[string]any{"drift_count": rep.DriftCount})
	}
	if rep.MaxDeviationMs > toleranceMs {
		rep.Status = "alignment_exceeded"
		return rep, errs.New(errs.ExitAlign, errs.CodeAlignExceeded,
			fmt.Sprintf("max deviation %dms exceeds tolerance %dms", rep.MaxDeviationMs, toleranceMs),
			map[string]any{"max_deviation_ms": rep.MaxDeviationMs, "tolerance_ms": toleranceMs})
	}
	return rep, nil
}

// verifyRep expands one Representation and reconciles it with its segment
// directory.
func verifyRep(baseDir string, r *mpd.Representation, rep *Report) (*RepReport, error) {
	st := r.SegmentTemplate
	mode := mpd.AddressingMode(st.Media)
	if mode == "" {
		return nil, errs.New(errs.ExitManifest, errs.CodeManifestParse,
			fmt.Sprintf("representation %s: media template has no $Number$/$Time$", r.ID), nil)
	}
	rep.Addressing = mode
	startNumber := int64(1)
	if st.StartNumber != nil {
		startNumber = *st.StartNumber
	}
	pts, err := mpd.ExpandTimeline(st.SegmentTimeline, startNumber)
	if err != nil {
		return nil, errs.New(errs.ExitManifest, errs.CodeManifestParse,
			fmt.Sprintf("representation %s: %v", r.ID, err), nil)
	}
	dir := filepath.Join(baseDir, r.BaseURL)
	probed, err := ingest.ProbeDir(dir)
	if err != nil {
		return nil, errs.New(errs.ExitMissing, errs.CodeMissingSegment, err.Error(),
			map[string]any{"representation": r.ID, "dir": dir})
	}
	byName := make(map[string]ingest.SegmentMeta, len(probed.Segments))
	for _, s := range probed.Segments {
		byName[s.Name] = s
	}
	rr := &RepReport{ID: r.ID, Bandwidth: r.Bandwidth, Segments: len(pts)}
	var prevEnd int64 = -1
	var diskTotal, expTotal int64
	for i, p := range pts {
		name, err := expandName(st.Media, r, p)
		if err != nil {
			return nil, err
		}
		if i == 0 {
			rr.First = stem(name)
		}
		rr.Last = stem(name)
		seg, ok := byName[name]
		if !ok {
			return nil, errs.New(errs.ExitMissing, errs.CodeMissingSegment,
				fmt.Sprintf("representation %s: missing segment %s", r.ID, name),
				map[string]any{"representation": r.ID, "segment": name})
		}
		delete(byName, name)
		dev, derr := deviation(seg, p)
		if derr != nil {
			return nil, errs.New(errs.ExitParam, errs.CodeOverflow, derr.Error(), nil)
		}
		if dev > rr.MaxDeviationMs {
			rr.MaxDeviationMs = dev
		}
		start, _ := mpd.Rescale(seg.PTS, seg.Timescale, align.CommonTimescale)
		end, _ := mpd.Rescale(seg.PTS+seg.Duration, seg.Timescale, align.CommonTimescale)
		if prevEnd >= 0 && start < prevEnd {
			rep.DriftCount++
		}
		prevEnd = end
		diskTotal += end - start
		expTotal += p.D
	}
	if len(byName) != 0 {
		extra := make([]string, 0, len(byName))
		for n := range byName {
			extra = append(extra, n)
		}
		sort.Strings(extra)
		return nil, errs.New(errs.ExitMissing, errs.CodeSegmentCount,
			fmt.Sprintf("representation %s: %d unexpected segments on disk", r.ID, len(extra)),
			map[string]any{"representation": r.ID, "extra": extra[0]})
	}
	if driftMs := ticksToMs(abs64(diskTotal - expTotal)); driftMs > rep.ToleranceMs {
		return nil, errs.New(errs.ExitAlign, errs.CodeDurationMismatch,
			fmt.Sprintf("representation %s: total duration off by %dms", r.ID, driftMs),
			map[string]any{"representation": r.ID, "duration_deviation_ms": driftMs})
	}
	return rr, nil
}

// expandName renders the media file name of one expanded segment point.
func expandName(media string, r *mpd.Representation, p mpd.SegmentPoint) (string, error) {
	name, err := mpd.ExpandTemplate(media, mpd.TemplateVars{
		RepresentationID: r.ID,
		Bandwidth:        r.Bandwidth,
		Number:           p.Number,
		Time:             p.T,
	})
	if err != nil {
		return "", errs.New(errs.ExitManifest, errs.CodeManifestParse, err.Error(), nil)
	}
	return name, nil
}

// deviation returns the larger of start/duration deviation in milliseconds.
func deviation(seg ingest.SegmentMeta, p mpd.SegmentPoint) (int64, error) {
	start, err := mpd.Rescale(seg.PTS, seg.Timescale, align.CommonTimescale)
	if err != nil {
		return 0, err
	}
	dur, err := mpd.Rescale(seg.Duration, seg.Timescale, align.CommonTimescale)
	if err != nil {
		return 0, err
	}
	d := abs64(start - p.T)
	if dd := abs64(dur - p.D); dd > d {
		d = dd
	}
	return ticksToMs(d), nil
}

// ticksToMs converts common-timescale ticks to rounded milliseconds.
func ticksToMs(t int64) int64 {
	return (t + align.CommonTimescale/2000) / (align.CommonTimescale / 1000)
}

func stem(name string) string { return strings.TrimSuffix(name, filepath.Ext(name)) }

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
