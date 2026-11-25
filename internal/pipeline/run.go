// Package pipeline orchestrates the full data flow: manifest -> track
// graph -> sample scan -> timestamp repair -> edit mapping -> index ->
// remux, with bounded concurrency and deterministic result ordering.
package pipeline

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"mediaqa/internal/graph"
	"mediaqa/internal/index"
	"mediaqa/internal/manifest"
	"mediaqa/internal/media"
	mp4scan "mediaqa/internal/mp4"
	"mediaqa/internal/repair"
	"mediaqa/internal/report"
	"mediaqa/internal/timeline"
)

// Config bounds resources and locates the work directory.
type Config struct {
	WorkDir string // every input path must resolve inside this directory
	OutDir  string // repaired MP4s and indexes (inside WorkDir)
	Workers int    // concurrency bound; <=0 selects 4
}

// TrackResult is the per-track processing outcome.
type TrackResult struct {
	TrackID       string
	Kind          string
	Timescale     int64
	Samples       []mp4scan.Sample // repaired, valid samples only
	Mapper        *timeline.Mapper
	Index         *index.Index
	IndexBytes    []byte
	RepairedCount int
	InvalidCount  int
	RemuxPath     string
	IndexPath     string

	remuxed []mp4scan.Sample // sample table of the remuxed output
}

// Result aggregates one pipeline run.
type Result struct {
	Errs   media.List
	Tracks []TrackResult // manifest order, deterministic
}

// slot is one track's deterministic result cell.
type slot struct {
	tr   TrackResult
	errs *media.List
}

// Run executes the pipeline over a parsed manifest.
func Run(m *manifest.Manifest, cfg Config) (*Result, error) {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	ids := make([]string, len(m.Tracks))
	deps := map[string][]string{}
	for i, tr := range m.Tracks {
		ids[i] = tr.ID
		deps[tr.ID] = tr.DependsOn
	}
	if _, err := graph.Order(ids, deps); err != nil {
		return nil, err
	}

	// Scan each unique source file once.
	scans := map[string]*mp4scan.Result{}
	scanErrs := map[string]*media.List{}
	res := &Result{}
	for _, tr := range m.Tracks {
		path, err := resolvePath(cfg.WorkDir, tr.Path)
		if err != nil {
			continue // reported later by processTrack
		}
		if _, ok := scans[path]; !ok {
			el := &media.List{}
			s, err := mp4scan.Scan(path, el)
			if err != nil {
				el.Addf(media.SeverityFatal, tr.ID, "scan-io", media.Rational{}, media.Rational{}, "%v", err)
				s = &mp4scan.Result{}
			}
			scans[path] = s
			scanErrs[path] = el
		}
	}

	// Process tracks with bounded concurrency; slot-based collection
	// keeps the merged output deterministic.
	slots := make([]slot, len(m.Tracks))
	sem := make(chan struct{}, cfg.Workers)
	var wg sync.WaitGroup
	for i, tr := range m.Tracks {
		wg.Add(1)
		go func(i int, tr manifest.Track) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			slots[i] = processTrack(tr, cfg, scans, scanErrs)
		}(i, tr)
	}
	wg.Wait()
	for _, s := range slots {
		if s.errs != nil {
			for _, e := range s.errs.All() {
				res.Errs.Add(e)
			}
		}
		res.Tracks = append(res.Tracks, s.tr)
	}
	return res, nil
}

// resolvePath joins p to workDir and verifies the result stays inside.
func resolvePath(workDir, p string) (string, error) {
	if !filepath.IsLocal(p) {
		return "", fmt.Errorf("path %q escapes the work directory", p)
	}
	full := filepath.Join(workDir, p)
	clean := filepath.Clean(full)
	base := filepath.Clean(workDir)
	if clean != base && !strings.HasPrefix(clean, base+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the work directory", p)
	}
	return clean, nil
}

func processTrack(tr manifest.Track, cfg Config, scans map[string]*mp4scan.Result, scanErrs map[string]*media.List) slot {
	el := &media.List{}
	out := slot{errs: el}
	out.tr.TrackID = tr.ID
	out.tr.Kind = tr.Kind

	path, err := resolvePath(cfg.WorkDir, tr.Path)
	if err != nil {
		el.Addf(media.SeverityFatal, tr.ID, "bad-path", media.Rational{}, media.Rational{}, "%v", err)
		return out
	}
	// Propagate scan-stage errors for this file.
	for _, e := range scanErrs[path].All() {
		e.TrackID = tr.ID
		el.Add(e)
	}
	scan := scans[path]
	var ft *mp4scan.Track
	for i := range scan.Tracks {
		if scan.Tracks[i].Kind == tr.Kind {
			ft = &scan.Tracks[i]
			break
		}
	}
	if ft == nil {
		el.Addf(media.SeverityFatal, tr.ID, "no-track",
			media.Rational{}, media.Rational{}, "no %s track in %s", tr.Kind, tr.Path)
		return out
	}
	out.tr.Timescale = ft.Timescale

	// Timestamp repair in decode order.
	repaired := repair.Stamps(ft.Samples, ft.Timescale, tr.ID, el)
	for _, e := range el.All() {
		if e.Code == "repaired-stamp" {
			out.tr.RepairedCount++
		}
	}
	for _, s := range ft.Samples {
		if s.Corrupt {
			out.tr.InvalidCount++
		}
	}
	out.tr.Samples = repaired

	// Edit mapping.
	edits := make([]timeline.Edit, len(tr.Edits))
	for i, e := range tr.Edits {
		edits[i] = timeline.Edit{MediaStart: e.MediaStart, Duration: e.Duration}
	}
	mapper, err := timeline.NewMapper(edits)
	if err != nil {
		el.Addf(media.SeverityFatal, tr.ID, "bad-edit", media.Rational{}, media.Rational{}, "%v", err)
		return out
	}
	out.tr.Mapper = mapper

	// Remux: copy valid sample payloads, verifying offsets and lengths.
	if err := out.tr.remux(cfg, tr, path, repaired, ft, el); err != nil {
		el.Addf(media.SeverityFatal, tr.ID, "remux", media.Rational{}, media.Rational{}, "%v", err)
		return out
	}

	// Random-access index over the remuxed file: offsets/sizes come from
	// the remuxed sample table, timestamps from the repaired originals so
	// the edit mapping applies to the original media timeline.
	if len(out.tr.remuxed) > 0 {
		combined := make([]mp4scan.Sample, len(out.tr.remuxed))
		for i, s := range out.tr.remuxed {
			s.DTS = repaired[i].DTS
			s.PTS = repaired[i].PTS
			s.Duration = repaired[i].Duration
			combined[i] = s
		}
		ix := index.Build(tr.ID, ft.Timescale, combined, mapper.MediaToPres)
		out.tr.Index = ix
		out.tr.IndexBytes = ix.Marshal()
		idxPath := filepath.Join(cfg.OutDir, tr.ID+".idx")
		if err := report.AtomicWrite(idxPath, out.tr.IndexBytes); err != nil {
			el.Addf(media.SeverityFatal, tr.ID, "index-write", media.Rational{}, media.Rational{}, "%v", err)
			return out
		}
		out.tr.IndexPath = idxPath
	}
	return out
}
