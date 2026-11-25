// Package pipeline connects ingest, timeline, codec negotiation, alignment and
// CMAF packaging. The CLI and tests use the same deterministic path.
package pipeline

import (
	"fmt"

	"mediad/internal/codec"
	"mediad/internal/fixture"
	"mediad/internal/ingest"
	"mediad/internal/manifest"
	"mediad/internal/mediaerr"
	"mediad/internal/segment"
	"mediad/internal/timeline"
)

// Built is the fully aligned, packaged representation set.
type Built struct {
	Alignment *segment.Alignment
	Master    manifest.Master
	Init      []byte
	Tracks    []segment.Track
	Reports   []*ingest.Report
	Packages  map[string]*segment.CMAFPackage
}

// Build scans and parses every rendition, unwraps timestamps, negotiates V1
// parsers, aligns common IDRs and builds deterministic CMAF packages.
func Build(seed int64, specs []fixture.RenditionSpec, data [][]byte) (*Built, error) {
	if len(specs) != len(data) || len(specs) != 2 {
		return nil, fmt.Errorf("pipeline requires two rendition specs and byte streams")
	}
	registry := codec.NewDefaultRegistry()
	tracks := make([]segment.Track, len(data))
	reports := make([]*ingest.Report, len(data))
	for r, raw := range data {
		s := ingest.NewScanner()
		records, err := s.Scan(raw)
		if err != nil {
			return nil, err
		}
		rep := s.Report()
		reports[r] = rep
		track, err := recordsToTrack(specs[r].ID, r, records, s.Tables, registry)
		if err != nil {
			return nil, err
		}
		tracks[r] = track
	}
	alignment, err := segment.AlignTracks(tracks)
	if err != nil {
		return nil, err
	}
	init, err := segment.BuildInit()
	if err != nil {
		return nil, err
	}
	b := &Built{
		Alignment: alignment,
		Init:      init,
		Tracks:    tracks,
		Reports:   reports,
		Packages:  map[string]*segment.CMAFPackage{},
	}
	b.Master = manifest.Master{}
	for _, spec := range specs {
		b.Master.Renditions = append(b.Master.Renditions, manifest.Rendition{
			ID: spec.ID, Bandwidth: spec.Bandwidth, Width: spec.Width, Height: spec.Height,
			Playlist: spec.ID + "/index.m3u8", InitFile: "init.mp4",
		})
	}
	for i := range alignment.Segments {
		for r := range specs {
			pkg, err := segment.BuildCMAFSegment(alignment.Segments[i], r, uint32(i+1), init)
			if err != nil {
				return nil, err
			}
			key := packageKey(r, i)
			b.Packages[key] = pkg
			mr := &b.Master.Renditions[r]
			mr.Segments = append(mr.Segments, manifest.Segment{
				Index:      i,
				File:       fmt.Sprintf("seg_%04d.m4s", i),
				DurationMS: alignment.Segments[i].DurationMS,
				ByteSize:   len(pkg.Media),
				SHA256:     pkg.SHA256,
				CutPTS:     alignment.Segments[i].StartPTS,
				SafeCut:    true,
			})
		}
	}
	return b, nil
}

func packageKey(rendition, index int) string {
	return fmt.Sprintf("r%d/seg%04d", rendition, index)
}

// PackageRelPath returns the deterministic relative segment path.
func PackageRelPath(spec fixture.RenditionSpec, index int) string {
	return fmt.Sprintf("%s/seg_%04d.m4s", spec.ID, index)
}

// InitRelPath returns the deterministic relative init path.
func InitRelPath(spec fixture.RenditionSpec) string {
	return spec.ID + "/init.mp4"
}

func recordsToTrack(id string, rendition int, records []ingest.Record, tables *ingest.PSIRegistry, registry *codec.Registry) (segment.Track, error) {
	track := segment.Track{ID: id}
	ptsUnwrap := map[uint16]*timeline.Unwrapper{}
	pcrUnwrap := map[uint16]*timeline.Unwrapper{}
	for _, rec := range records {
		cid, err := codecForRecord(tables, rec)
		if err != nil {
			return track, err
		}
		parser, err := registry.Negotiate(cid, 1)
		if err != nil {
			return track, err
		}
		if ptsUnwrap[rec.PID] == nil {
			ptsUnwrap[rec.PID] = &timeline.Unwrapper{}
		}
		pts := int64(0)
		dts := int64(0)
		if rec.PTS.Valid {
			v, err := ptsUnwrap[rec.PID].Unwrap33(rec.PTS.Value)
			if err != nil {
				return track, err
			}
			pts = v
		}
		if rec.DTS.Valid {
			d, err := ptsUnwrap[rec.PID].Unwrap33(rec.DTS.Value)
			if err != nil {
				return track, err
			}
			dts = d
		} else {
			dts = pts
		}
		if rec.PCR != nil {
			u := pcrUnwrap[rec.PID]
			if u == nil {
				u = &timeline.Unwrapper{}
				pcrUnwrap[rec.PID] = u
			}
			if _, err := timeline.UnwrapPCR(u, rec.PCR.Base, rec.PCR.Extension); err != nil {
				return track, err
			}
		}
		samples, err := parser.Parse(rec.Payload, codec.ParseContext{
			PID: rec.PID, Offset: rec.Offset, PTS: pts, DTS: dts,
		})
		if err != nil {
			return track, err
		}
		for _, sm := range samples {
			track.Frames = append(track.Frames, segment.Frame{
				Rendition: rendition,
				PID:       rec.PID,
				Offset:    rec.Offset,
				PTS:       sm.PTS,
				DTS:       sm.DTS,
				Keyframe:  sm.Keyframe,
				Codec:     sm.Codec,
				Payload:   sm.Payload,
			})
		}
	}
	return track, nil
}

func codecForRecord(tables *ingest.PSIRegistry, rec ingest.Record) (codec.Codec, error) {
	for _, pmt := range tables.PMTs {
		if st, ok := pmt.Streams[rec.PID]; ok {
			if c, ok := codec.FromStreamType(st); ok {
				return c, nil
			}
			return "", unsupportedAt(rec, st)
		}
	}
	switch {
	case rec.StreamID >= 0xe0 && rec.StreamID <= 0xef:
		return codec.CodecH264, nil
	case rec.StreamID >= 0xc0 && rec.StreamID <= 0xdf:
		return codec.CodecAAC, nil
	default:
		return "", unsupportedAt(rec, 0)
	}
}

func unsupportedAt(rec ingest.Record, streamType byte) error {
	return mediaerr.New(mediaerr.KindUnsupportedCodec, rec.Offset, rec.PID, false,
		"unsupported stream type=0x%02x stream_id=0x%02x", streamType, rec.StreamID)
}
