package demux

import (
	"os"
	"path/filepath"
	"strings"

	"archivecheck/internal/media"
)

// ParseSource dispatches a source package to the correct demuxer by file
// extension. .rtp files are H.264 RTP capture streams; everything else is
// parsed as BMFF (plain or fragmented MP4/MOV).
func ParseSource(path, segment string, cfg ParseConfig) ([]TrackSamples, *SourceReport, error) {
	if strings.EqualFold(filepath.Ext(path), ".rtp") {
		return parseRTPFile(path, segment)
	}
	return ParseBMFF(path, segment, cfg)
}

func parseRTPFile(path, segment string) ([]TrackSamples, *SourceReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &SourceReport{Segment: segment, Container: ContainerRTP},
			media.NewFault(media.KindIO, segment, 1, -1, "open rtp source: %s", err)
	}
	defer f.Close()
	samples, report, err := ReadRTPStream(f, segment, DefaultRTPConfig())
	if err != nil {
		return nil, report, err
	}
	meta := TrackMeta{
		Track: 1, Codec: CodecAVC1, Handler: "vide",
		TimeBase: media.TimeBaseVideo90k, Samples: len(samples),
	}
	for _, s := range samples {
		if s.Keyframe {
			meta.Keyframes = append(meta.Keyframes, s.Index+1)
		}
	}
	report.Tracks = []TrackMeta{meta}
	return []TrackSamples{{Meta: meta, Samples: samples}}, report, nil
}
