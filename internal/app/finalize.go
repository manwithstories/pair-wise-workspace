// Package app wires the relay subsystems into the terminal acceptance use cases.
package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/relayteam/relay/internal/align"
	"github.com/relayteam/relay/internal/checkpoint"
	"github.com/relayteam/relay/internal/fixture"
	"github.com/relayteam/relay/internal/manifest"
	"github.com/relayteam/relay/internal/segment"
	"github.com/relayteam/relay/internal/timeline"
	"github.com/relayteam/relay/internal/verify"
)

// BuildOutputs deterministically rebuilds derived segment files and HLS
// manifests from verified committed chunks, then returns the tree digest.
type segmentMarkerKey struct {
	stream string
	index  int
}

func BuildOutputs(root string, mgr *checkpoint.Manager, report *align.Report) (string, error) {
	segRoot := filepath.Join(root, "segments")
	if err := os.RemoveAll(segRoot); err != nil {
		return "", err
	}
	if err := os.MkdirAll(segRoot, 0o755); err != nil {
		return "", err
	}

	cutter := segment.New(fixture.SegmentTicks)
	writers := make(map[string]*os.File)
	discontinuity := make(map[segmentMarkerKey]bool)
	closeWriters := func() {
		for _, f := range writers {
			_ = f.Close()
		}
	}

	err := mgr.StreamCommitted(func(rec timeline.Record) error {
		frag, err := cutter.Assign(rec)
		if err != nil {
			return err
		}
		key := rec.Track + "/" + rec.Rendition
		safe := fixture.SafeName(key)
		dir := filepath.Join(segRoot, safe)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		name := fmt.Sprintf("seg-%05d.ts", frag.SegmentIndex)
		path := filepath.Join(dir, name)
		f, ok := writers[path]
		if !ok {
			f, err = os.Create(path)
			if err != nil {
				return err
			}
			writers[path] = f
		}
		if _, err := f.WriteString(rec.Payload); err != nil {
			return err
		}
		if rec.Transition != nil {
			discontinuity[segmentMarkerKey{stream: key, index: frag.SegmentIndex}] = true
		}
		return nil
	})
	if err != nil {
		closeWriters()
		return "", err
	}
	closeWriters()

	medias, err := buildMediaModels(report, discontinuity)
	if err != nil {
		return "", err
	}
	if err := manifest.CheckEqualDuration(medias); err != nil {
		return "", err
	}
	if _, _, err := manifest.WriteAll(root, medias); err != nil {
		return "", err
	}
	mgr.Index.Completed = true
	if err := mgr.Save(); err != nil {
		return "", err
	}
	return verify.DigestTree(root, verify.ManifestDigestMatcher)
}

func buildMediaModels(report *align.Report, discontinuity map[segmentMarkerKey]bool) ([]manifest.Media, error) {
	if report == nil {
		return nil, fmt.Errorf("alignment report is empty")
	}
	byName := make(map[string]fixture.RenditionSpec, len(fixture.Renditions))
	for _, r := range fixture.Renditions {
		byName[r.Name] = r
	}
	out := make([]manifest.Media, 0, len(report.Keys))
	for _, key := range report.Keys {
		rep := report.Representations[key]
		meta := byName[rep.Rendition]
		safe := fixture.SafeName(key)
		m := manifest.Media{
			Key: key, File: safe + ".m3u8", Track: rep.Track,
			Rendition: rep.Rendition, Bandwidth: meta.Bandwidth,
			Codecs: meta.Codecs, Resolution: meta.Resolution,
		}
		for _, sp := range rep.Spans {
			uri := fmt.Sprintf("segments/%s/seg-%05d.ts", safe, sp.SegmentIndex)
			disc := discontinuity[segmentMarkerKey{stream: key, index: sp.SegmentIndex}]
			m.Segments = append(m.Segments, manifest.Segment{
				URI: uri, DurationTicks: sp.End90k - sp.Start90k,
				Title:         fmt.Sprintf("%s segment %d", key, sp.SegmentIndex),
				Discontinuity: disc,
			})
		}
		out = append(out, m)
	}
	return out, nil
}
