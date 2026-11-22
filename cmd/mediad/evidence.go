package main

import (
	"bytes"
	"os"
	"path/filepath"

	"mediad/internal/audio"
	"mediad/internal/fault"
	"mediad/internal/fixture"
	"mediad/internal/index"
	"mediad/internal/manifest"
	"mediad/internal/probe"
	"mediad/internal/worker"
)

func readHLS(dir string) (*manifest.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, fixture.ManifestHLS))
	if err != nil {
		return nil, err
	}
	m, _, err := manifest.ParseHLS(bytes.NewReader(data), fixture.ManifestHLS)
	return m, err
}

func readDASH(dir string) (*manifest.Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, fixture.ManifestDASH))
	if err != nil {
		return nil, err
	}
	m, _, err := manifest.ParseDASH(data, fixture.ManifestDASH)
	return m, err
}

func roundTripScore(hls, dash *manifest.Manifest) (float64, error) {
	hlsOut, err := manifest.SerializeHLS(hls)
	if err != nil {
		return 0, err
	}
	hlsBack, _, err := manifest.ParseHLS(bytes.NewReader(hlsOut), "hls-roundtrip")
	if err != nil {
		return 0, err
	}
	dashOut, err := manifest.SerializeDASH(dash)
	if err != nil {
		return 0, err
	}
	dashBack, _, err := manifest.ParseDASH(dashOut, "dash-roundtrip")
	if err != nil {
		return 0, err
	}
	hm, ht := manifest.RoundTripScore(hls, hlsBack)
	dm, dt := manifest.RoundTripScore(dash, dashBack)
	if ht+dt == 0 {
		return 0, nil
	}
	return 100 * float64(hm+dm) / float64(ht+dt), nil
}

// assembleIndex prefers a shipped index, falling back to a full segment scan
// when it is absent or incomplete. Undeterminable entries stay in the index
// but are excluded from coverage denominators.
func assembleIndex(shipped *fixture.IndexManifest, res *worker.Result, segs []fixture.Segment, inputs []worker.SegmentInput) *index.Index {
	if shipped != nil {
		entries := make([]index.Entry, 0, len(shipped.Entries))
		for _, e := range shipped.Entries {
			entries = append(entries, index.Entry{
				Segment: e.URI, Start: e.Start, Keyframe: e.Keyframe,
				Determined: true, Source: "index",
			})
		}
		idx := &index.Index{Entries: index.Merge(entries)}
		// Cross-check only keyframe positions: every scan-confirmed keyframe
		// must exist in the shipped index with the same start.
		if scan := res.KeyIndex; scan != nil {
			have := map[int64]bool{}
			for _, e := range scan.Keyframes() {
				have[e.Start] = true
			}
			for _, e := range idx.Keyframes() {
				if !have[e.Start] {
					idx.Incomplete = true
				}
			}
		}
		if !idx.Incomplete {
			return idx
		}
	}
	refs := make([]index.SegmentRef, 0, len(segs))
	for i, s := range segs {
		refs = append(refs, index.SegmentRef{
			URI: s.URI, RepresentationID: "seg", Sequence: inputs[i].Segment.Sequence,
		})
	}
	byURI := map[string]int{}
	for i, s := range segs {
		byURI[s.URI] = i
	}
	return index.Rebuild(refs, func(ref index.SegmentRef) (*probe.MediaInfo, *fault.List) {
		i := byURI[ref.URI]
		return inputs[i].Media, nil
	})
}

// keyframeCoverage counts determinable keyframe positions that the index
// resolves exactly. Only positions confirmed by the fragment scan enter the
// denominator; undeterminable segments are reported separately.
func keyframeCoverage(idx *index.Index, starts []int64) (int, int) {
	if idx == nil {
		return 0, 0
	}
	have := map[int64]bool{}
	for _, e := range idx.Keyframes() {
		have[e.Start] = true
	}
	hits := 0
	for _, start := range starts {
		if have[start] {
			hits++
		}
	}
	return hits, len(starts)
}

// writeOutputs stores normalised PCM bytes for independent verification.
func writeOutputs(dir string, res *worker.Result, opts worker.Options) error {
	outDir, err := fixture.SafeJoin(dir, OutputDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, o := range res.Outputs {
		if !o.OutputKnown || o.AudioResult == nil || o.Gap {
			continue
		}
		path, err := fixture.SafeJoin(dir, filepath.Join(OutputDir, o.URI+".pcm"))
		if err != nil {
			return err
		}
		if err := os.WriteFile(path, audio.EncodePCM16(o.AudioResult.Samples), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func segmentEvidenceFor(b fixture.Batch, res *worker.Result, opts worker.Options) []segmentEvidence {
	var out []segmentEvidence
	for i, o := range res.Outputs {
		ev := segmentEvidence{Path: filepath.ToSlash(filepath.Join(string(b.Case), o.URI)), Role: "input"}
		ev.SHA256 = string(o.InputHash)
		ev.Chunk = i / opts.ChunkSize
		out = append(out, ev)
		if o.OutputKnown && !o.Gap {
			out = append(out, segmentEvidence{
				Path:   filepath.ToSlash(filepath.Join(string(b.Case), OutputDir, o.URI+".pcm")),
				SHA256: string(o.OutputHash), Role: "output", Chunk: i / opts.ChunkSize,
			})
		}
	}
	return out
}

// manifestSegment maps a generated segment onto its HLS record so the
// pipeline sees real manifest fields (sequence numbers and timeline).
func manifestSegment(m *manifest.Manifest, seg fixture.Segment, i int) manifest.Segment {
	if m != nil {
		for _, rep := range m.Representations {
			if i < len(rep.Segments) {
				return rep.Segments[i]
			}
		}
	}
	return manifest.Segment{
		URI: seg.URI, Sequence: seg.Sequence, Index: i,
		Start: seg.StartTicks, Duration: seg.Duration, DurationKnown: true,
		RepresentationID: "seg",
	}
}

// readMedia probes one segment. A corrupt segment still returns faults with
// exact offsets so the pipeline can report them.
func readMedia(seg fixture.Segment, timescales map[uint32]uint32) (*probe.MediaInfo, *fault.List) {
	info, fl, err := probe.ParseMedia(bytes.NewReader(seg.Bytes), int64(len(seg.Bytes)), seg.URI, timescales)
	if err != nil {
		var l fault.List
		l.Add(fault.MustNew(fault.KindBoxHeader, fault.Location{Segment: seg.URI, Offset: 0}, err.Error(), true, false))
		return nil, &l
	}
	return info, fl
}
