package fixture

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pion/rtp"

	"mediacore/internal/clock"
	"mediacore/internal/manifest"
)

// writeManifests renders the HLS playlists and the DASH MPD. The DASH
// timelines start 20ms later than the HLS ones so alignment reporting
// has a real, in-tolerance value to measure.
func writeManifests(dir string, res *Result) error {
	seg := func(i int, uri string, gap bool) manifest.Segment {
		return manifest.Segment{Index: i, URI: uri, Dur: clock.Must(SegSeconds, 1), Gap: gap}
	}

	video := manifest.Track{TrackMeta: res.VideoMeta}
	for i := 0; i < VideoSegs; i++ {
		video.Segments = append(video.Segments, seg(i, fmt.Sprintf("media/v1-%d.m4s", i+1), false))
	}
	audio := manifest.Track{TrackMeta: res.AudioMeta}
	audioNum := 0
	for i := 0; i < AudioSegs; i++ {
		if i == GapAudioSegment {
			audio.Segments = append(audio.Segments, seg(i, "media/a1-gap.m4s", true))
			continue
		}
		audioNum++
		audio.Segments = append(audio.Segments, seg(i, fmt.Sprintf("media/a1-%d.m4s", audioNum), false))
	}
	replay := manifest.Track{TrackMeta: res.ReplayMeta}
	for i := 0; i < ReplaySegs; i++ {
		replay.Segments = append(replay.Segments, seg(i, fmt.Sprintf("media/v2-%d.m4s", i+1), false))
	}

	for name, tr := range map[string]manifest.Track{
		"video.m3u8":  video,
		"audio.m3u8":  audio,
		"replay.m3u8": replay,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(manifest.FormatHLS(tr)), 0o644); err != nil {
			return err
		}
		res.Manifests = append(res.Manifests, name)
	}

	mpd := `<?xml version="1.0"?>
<MPD mediaPresentationDuration="PT16S">
  <Period>
    <AdaptationSet id="v1" contentType="video" codecs="avc1">
      <Representation id="v1">
        <SegmentTemplate timescale="90000" media="media/v1-$Number$.m4s" startNumber="0">
          <SegmentTimeline>
            <S t="1800" d="180000" r="7"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet id="a1" contentType="audio" codecs="mp4a">
      <Representation id="a1">
        <SegmentTemplate timescale="48000" media="media/a1-$Number$.m4s" startNumber="0">
          <SegmentTimeline>
            <S t="960" d="96000" r="3"/>
            <S t="480960" d="96000" r="2"/>
          </SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.mpd"), []byte(mpd), 0o644); err != nil {
		return err
	}
	res.Manifests = append(res.Manifests, "manifest.mpd")
	return nil
}

// writeRTP stores an RTP packet sequence whose timestamps straddle the
// 32-bit wrap point (timestamp overflow sample).
func writeRTP(dir string) error {
	stamps := []uint32{0xFFFFFFF0, 0xFFFFFFFC, 0x00000008, 0x00000014}
	var out []byte
	for i, ts := range stamps {
		p := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: uint16(100 + i), Timestamp: ts, SSRC: 7},
			Payload: []byte{byte(i)},
		}
		raw, err := p.Marshal()
		if err != nil {
			return err
		}
		out = append(out, byte(len(raw)>>8), byte(len(raw)))
		out = append(out, raw...)
	}
	return os.WriteFile(filepath.Join(dir, "rtp.bin"), out, 0o644)
}
