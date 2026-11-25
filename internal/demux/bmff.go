package demux

import (
	"errors"
	"os"

	"archivecheck/internal/media"
)

// TrackSamples binds a track's metadata to its ordered coded samples.
type TrackSamples struct {
	Meta    TrackMeta
	Samples []Sample
}

// ParseBMFF parses an MP4, MOV or fragmented-MP4 file. Coded bytes are copied
// out verbatim; the parser never decodes them. Non-fatal faults (a truncated
// tail, a missing index) are returned in the report, while fatal faults
// quarantine the owning track or segment and are returned as the error.
func ParseBMFF(path, segment string, cfg ParseConfig) ([]TrackSamples, *SourceReport, error) {
	report := &SourceReport{Segment: segment, Container: ContainerBMFF}
	var faults media.FaultSet

	f, err := os.Open(path)
	if err != nil {
		return nil, report, media.NewFault(media.KindIO, segment, 0, -1, "open source: %s", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, report, media.NewFault(media.KindIO, segment, 0, -1, "stat source: %s", err)
	}
	fileSize := fi.Size()

	toplevel := map[string]rawBox{}
	if err := walkBoxes(f, 0, fileSize, segment, &faults, func(b rawBox) error {
		toplevel[b.info.Type.String()] = b
		_, _ = f.Seek(b.end, os.SEEK_SET)
		return nil
	}); err != nil {
		report.Faults = faults.All()
		return nil, report, err
	}

	if _, ok := toplevel["moof"]; ok {
		report.Container = ContainerFragmented
		tr := parseFragmented(f, segment, fileSize, toplevel, cfg, &faults, report)
		report.Faults = faults.All()
		if faults.HasFatal() {
			return tr, report, errors.New(faults.Error())
		}
		return tr, report, nil
	}

	moov, ok := toplevel["moov"]
	if !ok {
		f := media.NewFault(media.KindBadLength, segment, 0, 0, "no moov box found in source")
		faults.Add(f)
		report.Faults = faults.All()
		return nil, report, f
	}
	mdat, hasMdat := toplevel["mdat"]
	mdatEnd := fileSize
	if hasMdat {
		mdatEnd = mdat.end
	}

	traks, err := collectBoxes(f, moov, "trak")
	if err != nil {
		return nil, report, err
	}

	var out []TrackSamples
	for ti, trak := range traks {
		trackNo := ti + 1
		ts, fatal := parseTrack(f, segment, trackNo, trak, fileSize, mdatEnd, cfg, &faults, report)
		if fatal != nil {
			// Fatal track fault: isolate this track but keep processing the rest.
			continue
		}
		if ts != nil {
			out = append(out, *ts)
		}
	}

	report.Faults = faults.All()
	report.Tracks = make([]TrackMeta, 0, len(out))
	for _, ts := range out {
		report.Tracks = append(report.Tracks, ts.Meta)
		report.Samples += len(ts.Samples)
	}
	if faults.HasFatal() {
		return out, report, errors.New(faults.Error())
	}
	return out, report, nil
}

// collectBoxes returns every descendant box of parent whose type matches.
func collectBoxes(r *os.File, parent rawBox, types ...string) ([]rawBox, error) {
	want := map[string]bool{}
	for _, t := range types {
		want[t] = true
	}
	var found []rawBox
	var recurse func(rawBox) error
	recurse = func(b rawBox) error {
		if !containerBoxes[b.info.Type.String()] {
			return nil // leaf box: payload is data, never nested boxes
		}
		return walkBoxes(r, b.payload, b.end, "", &media.FaultSet{}, func(child rawBox) error {
			if want[child.info.Type.String()] {
				found = append(found, child)
			}
			return recurse(child)
		})
	}
	if err := recurse(parent); err != nil {
		return nil, err
	}
	return found, nil
}

// parseTrack extracts one trak box's tables and assembles its samples.
func parseTrack(f *os.File, segment string, trackNo int, trak rawBox,
	fileSize, mdatEnd int64, cfg ParseConfig, faults *media.FaultSet, report *SourceReport) (*TrackSamples, error) {

	meta := TrackMeta{Track: trackNo, TimeBase: media.TimeBaseVideo90k}
	if boxes, _ := collectBoxes(f, trak, "mdhd"); len(boxes) > 0 {
		if b, err := readPayloadAt(f, boxes[0].payload, boxes[0].end-boxes[0].payload, cfg.MaxSampleBytes); err == nil {
			off := 12
			if b[0] == 1 {
				off = 20
			}
			if ts := be32(b[off:]); ts > 0 {
				meta.TimeBase = media.Rational{Num: int64(ts), Den: 1}
			}
		}
	}
	if boxes, _ := collectBoxes(f, trak, "hdlr"); len(boxes) > 0 {
		if b, err := readPayloadAt(f, boxes[0].payload, boxes[0].end-boxes[0].payload, 256); err == nil && len(b) >= 12 {
			meta.Handler = string(b[8:12])
		}
	}

	stbls, _ := collectBoxes(f, trak, "stbl")
	if len(stbls) == 0 {
		faults.Add(media.NewFault(media.KindBadLength, segment, trackNo, trak.payload, "trak has no stbl box"))
		return nil, errors.New("missing stbl")
	}
	stbl := stbls[0]
	children := map[string]rawBox{}
	_ = walkBoxes(f, stbl.payload, stbl.end, segment, faults, func(b rawBox) error {
		children[b.info.Type.String()] = b
		_, _ = f.Seek(b.end, os.SEEK_SET)
		return nil
	})

	if stsd, ok := children["stsd"]; ok {
		meta.Codec = firstChildCodec(f, stsd, cfg)
	}
	if !codecSupported(meta.Codec) {
		faults.Add(media.NewFault(media.KindUnsupported, segment, trackNo, trak.payload,
			"codec %q is not supported for lossless remux, isolating track", meta.Codec))
		return nil, errors.New("unsupported codec")
	}

	samples, err := assembleSamples(f, segment, trackNo, meta, children, fileSize, mdatEnd, cfg, faults, report)
	if err != nil {
		return nil, err
	}
	meta.Samples = len(samples)
	if stss, ok := children["stss"]; ok {
		if b, e := readPayloadAt(f, stss.payload, stss.end-stss.payload, cfg.MaxSampleBytes); e == nil {
			if nums, e2 := parseStss(b); e2 == nil {
				meta.Keyframes = make([]int, len(nums))
				for i, n := range nums {
					meta.Keyframes[i] = int(n)
				}
			}
		}
	}
	return &TrackSamples{Meta: meta, Samples: samples}, nil
}

func firstChildCodec(f *os.File, stsd rawBox, cfg ParseConfig) string {
	var codec string
	_ = walkBoxes(f, stsd.payload+8, stsd.end, "", &media.FaultSet{}, func(b rawBox) error {
		if codec == "" {
			codec = b.info.Type.String()
		}
		return nil
	})
	return codec
}

func codecSupported(codec string) bool {
	switch codec {
	case CodecAVC1, CodecMP4A, CodecText:
		return true
	}
	return false
}

func be32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

