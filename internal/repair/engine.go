package repair

import (
	"mediacheck/internal/fault"
	"mediacheck/internal/probe"
)

// Engine turns probe tracks into independent recovery results.
type Engine struct {
	cfg      Config
	probeCfg probe.Config
}

// New creates a recovery engine. Segments and tolerance are shared with the
// fanout stage so recovery and manifest agree on the clock.
func New(cfg Config, probeCfg probe.Config) *Engine {
	cfg = cfg.Normalize()
	probeCfg = probeCfg.Normalize()
	probeCfg.SegmentMS = cfg.SegmentMS
	return &Engine{cfg: cfg, probeCfg: probeCfg}
}

// Config exposes the normalized configuration.
func (e *Engine) Config() Config { return e.cfg }

// Analyze probes and then repairs a file, keeping every track independent.
func (e *Engine) Analyze(path string) (*probe.File, *Result, error) {
	pf, err := probe.Analyze(path, e.probeCfg)
	if err != nil {
		return nil, nil, err
	}
	res := e.Recover(pf)
	return pf, res, nil
}

// Recover builds a Result from an already-probed file.
func (e *Engine) Recover(pf *probe.File) *Result {
	res := &Result{Path: pf.Path, Issues: append([]*fault.Fault{}, pf.Issues...)}
	for _, tr := range pf.Tracks {
		trRes := scanTrack(pf.Path, tr, e.cfg)
		res.Tracks = append(res.Tracks, trRes)
	}
	for _, tr := range pf.Tracks {
		res.Issues = append(res.Issues, tr.Issues...)
	}
	res.Boundary = crossCheck(res.Tracks, e.cfg)
	res.Status = summarize(res.Tracks, res.Boundary)
	if pf.Truncated {
		res.Issues = append(res.Issues, fault.At(fault.CodeTruncated, fault.SeverityPartial, "repair",
			pf.FirstUnreadable, "tail was truncated"))
	}
	return res
}

// summarize collapses per-track status and boundary agreement into one status
// that the CLI maps to an exit code.
func summarize(tracks []*TrackResult, b BoundaryReport) Status {
	if len(tracks) == 0 {
		return ResultFailed
	}
	usable := 0
	failed := 0
	for _, t := range tracks {
		switch t.Status {
		case StatusUnrecoverable:
			failed++
		default:
			if t.usable() {
				usable++
			}
		}
	}
	if usable == 0 {
		return ResultFailed
	}
	if failed > 0 || !b.Accept {
		return ResultPartial
	}
	for _, t := range tracks {
		if t.Status == StatusRecovered {
			return ResultPartial
		}
	}
	return ResultComplete
}

// TrackByID returns the result for a track id.
func (r *Result) TrackByID(id uint32) *TrackResult {
	for _, t := range r.Tracks {
		if t.TrackID == id {
			return t
		}
	}
	return nil
}

// IssuesAll flattens file and per-track faults for reporting.
func (r *Result) IssuesAll() []*fault.Fault {
	out := append([]*fault.Fault{}, r.Issues...)
	out = append(out, r.Boundary.Issues...)
	return out
}

// usableTracks returns tracks contributing to the manifest.
func (r *Result) usableTracks() []*TrackResult {
	var out []*TrackResult
	for _, t := range r.Tracks {
		if t.usable() {
			out = append(out, t)
		}
	}
	return out
}
