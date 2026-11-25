// Package pipeline wires manifest parsing, scanning, repair, indexing
// and streaming execution into one deterministic group run. The audit
// package rebuilds the index through this same mapping but performs its
// own independent verification afterwards.
package pipeline

import (
	"encoding/json"
	"fmt"
	"os"

	"archivectl/internal/index"
	"archivectl/internal/layout"
	"archivectl/internal/manifest"
	"archivectl/internal/repair"
	"archivectl/internal/scan"
)

// Analysis is the repaired, in-memory view of one group's inputs.
type Analysis struct {
	InputDigest   string
	HLS           *manifest.Manifest
	DASH          *manifest.Manifest
	SegmentCount  int
	Discontinuity map[int]bool
	Timelines     map[string]*repair.TrackTimeline
	Fragments     map[string]map[int]*scan.Fragment
	RTP           *repair.RTPReport
	Conflicts     []index.MetadataConflict
}

// Analyze digests inputs, parses manifests and metadata, scans and
// repairs every indexed track, and runs RTP recovery.
func Analyze(root, group string) (*Analysis, error) {
	in := layout.InDir(root, group)
	digest, err := index.DigestTree(in)
	if err != nil {
		return nil, fmt.Errorf("pipeline: input digest: %w", err)
	}
	a := &Analysis{
		InputDigest:   digest,
		Discontinuity: map[int]bool{},
		Timelines:     map[string]*repair.TrackTimeline{},
		Fragments:     map[string]map[int]*scan.Fragment{},
	}

	hls, err := manifest.ParseHLSFile(layout.HLSPath(root, group))
	if err != nil {
		return nil, err
	}
	a.HLS = hls
	for _, s := range hls.Segments {
		if s.Discontinuity {
			a.Discontinuity[s.Index] = true
		}
	}
	a.SegmentCount = len(hls.Segments)

	dash, err := manifest.ParseDASHFile(layout.DASHPath(root, group))
	if err != nil {
		return nil, err
	}
	a.DASH = dash

	if err := a.scanTracks(root, group); err != nil {
		return nil, err
	}
	if err := a.runRTP(root, group); err != nil {
		return nil, err
	}
	a.readMetadata(root, group)
	return a, nil
}

func (a *Analysis) scanTracks(root, group string) error {
	for _, track := range layout.Tracks {
		byIndex := map[int]*scan.Fragment{}
		var segs []repair.Segment
		for i := 0; i < a.SegmentCount; i++ {
			p := layout.SourcePath(root, group, track, i)
			rep, err := scan.ScanFile(p, track)
			if err != nil {
				return err
			}
			if rep.Record.HasFatal() || len(rep.Fragments) == 0 {
				// Unexpected structural damage in an indexed segment.
				return rep.Record
			}
			frag := rep.Fragments[0]
			byIndex[i] = frag
			segs = append(segs, repair.Segment{
				ID: layout.SourceRel(track, i), Index: i, TrackID: track,
				Timescale: layout.TimescaleFor(track), Fragment: frag,
				Discontinuity: a.Discontinuity[i],
			})
		}
		a.Fragments[track] = byIndex
		a.Timelines[track] = repair.RepairTrack(track, layout.TimescaleFor(track), segs)
	}
	return nil
}

func (a *Analysis) runRTP(root, group string) error {
	f, err := os.Open(layout.CapturePath(root, group))
	if err != nil {
		return err
	}
	defer f.Close()
	pkts, err := repair.ParseCapture(f)
	if err != nil {
		return err
	}
	var groups []repair.FECGroup
	if b, err := os.ReadFile(layout.FECPath(root, group)); err == nil {
		if err := json.Unmarshal(b, &groups); err != nil {
			return fmt.Errorf("pipeline: parse fec.json: %w", err)
		}
	}
	a.RTP = repair.RepairRTP(pkts, groups)
	return nil
}

func (a *Analysis) readMetadata(root, group string) {
	b, err := os.ReadFile(layout.TracksMetaPath(root, group))
	if err != nil {
		return
	}
	var mf manifest.MetadataFile
	if err := json.Unmarshal(b, &mf); err != nil {
		return
	}
	for _, tm := range mf.Tracks {
		r := manifest.ReconcileMetadata(tm.Track, tm.Hints...)
		if r.HasMetadataConflict() {
			a.Conflicts = append(a.Conflicts, index.MetadataConflict{TrackID: r.ID, Detail: r.MetadataConflict})
		}
	}
}
