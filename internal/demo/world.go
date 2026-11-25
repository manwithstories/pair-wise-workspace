// Package demo deterministically generates the self-contained archive
// fixture workspace (clean and fault-injected damaged profiles) and the
// resume-check orchestration used by the CLI.
package demo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"archivectl/internal/layout"
	"archivectl/internal/manifest"
	"archivectl/internal/scan"
)

// ProfileBoundaryV1 is the supported fault profile name.
const ProfileBoundaryV1 = "boundary-v1"

// Oracle records exactly what damage was injected so verification can
// distinguish expected fixtures from unexpected corruption.
type Oracle struct {
	Profile                  string `json:"profile"`
	Group                    string `json:"group"`
	Segments                 int    `json:"segments"`
	Seed                     int64  `json:"seed"`
	Discontinuities          []int  `json:"discontinuities"`
	RTP                      rtpOutcome `json:"rtp"`
	ExpectStructuralFault    bool   `json:"expect_structural_fault"`
	ExpectMetadataConflict   bool   `json:"expect_metadata_conflict"`
	ExpectedUnrecoveredGaps  int    `json:"expected_unrecovered_gaps"`
}

// Generate creates <root>/clean and <root>/damaged workspaces.
func Generate(root string, segments int, seed int64, profile string) error {
	if profile != ProfileBoundaryV1 {
		return fmt.Errorf("demo: unknown fault profile %q (want %s)", profile, ProfileBoundaryV1)
	}
	if segments < 4 {
		return fmt.Errorf("demo: segments must be >= 4, got %d", segments)
	}
	for _, group := range []string{"clean", "damaged"} {
		damaged := group == "damaged"
		disc := map[int]bool{}
		var discList []int
		if damaged {
			discList = []int{segments / 4, (2 * segments) / 3}
			for _, i := range discList {
				disc[i] = true
			}
		}
		if err := generateGroup(root, group, segments, seed, damaged, disc, discList, profile); err != nil {
			return err
		}
	}
	return nil
}

func generateGroup(root, group string, n int, seed int64, damaged bool, disc map[int]bool, discList []int, profile string) error {
	in := layout.InDir(root, group)
	if err := os.MkdirAll(in, 0o755); err != nil {
		return err
	}
	for _, t := range layout.Tracks {
		if err := writeTrackSegments(root, group, t, n, damaged, disc); err != nil {
			return err
		}
	}
	if err := writeHLS(root, group, n, disc); err != nil {
		return err
	}
	if err := writeDASH(root, group, n); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(in, "video", "init.mp4"), ftypBox(), 0o644); err != nil {
		return err
	}
	if err := writeTracksMeta(root, group, damaged); err != nil {
		return err
	}
	oc, err := writeRTP(root, group, damaged, seed)
	if err != nil {
		return err
	}

	o := Oracle{Profile: profile, Group: group, Segments: n, Seed: seed, Discontinuities: discList, RTP: oc}
	if damaged {
		o.ExpectStructuralFault = true
		o.ExpectMetadataConflict = true
		o.ExpectedUnrecoveredGaps = 1
		if err := writeStructuralFixture(root, group); err != nil {
			return err
		}
	}
	b, _ := json.MarshalIndent(o, "", "  ")
	return os.WriteFile(filepath.Join(layout.GroupDir(root, group), "oracle.json"), b, 0o644)
}

func writeHLS(root, group string, n int, disc map[int]bool) error {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:2\n")
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:260\n")
	b.WriteString("#EXT-X-MAP:URI=\"video/init.mp4\"\n")
	for i := 0; i < n; i++ {
		if disc[i] {
			b.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		b.WriteString("#EXTINF:1.000,\n")
		fmt.Fprintf(&b, "video/%s\n", layout.SegmentName(i))
	}
	return os.WriteFile(layout.HLSPath(root, group), []byte(b.String()), 0o644)
}

func writeDASH(root, group string, n int) error {
	tmpl := `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static">
  <Period>
    <AdaptationSet contentType="video">
      <Representation id="video" codecs="avc1.64001f">
        <SegmentTemplate timescale="90000" startNumber="0" presentationTimeOffset="180000"
          media="video/seg_$Number%%03d$.m4s" initialization="video/init.mp4">
          <SegmentTimeline><S d="90000" r="%d"/></SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
    <AdaptationSet contentType="audio">
      <Representation id="audio" codecs="mp4a.40.2" audioSamplingRate="48000">
        <AudioChannelConfiguration value="2"/>
        <SegmentTemplate timescale="48000" startNumber="0"
          media="audio/seg_$Number%%03d$.m4s">
          <SegmentTimeline><S d="48000" r="%d"/></SegmentTimeline>
        </SegmentTemplate>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>
`
	doc := fmt.Sprintf(tmpl, n-1, n-1)
	return os.WriteFile(layout.DASHPath(root, group), []byte(doc), 0o644)
}

func writeTracksMeta(root, group string, damaged bool) error {
	f := manifest.MetadataFile{Tracks: []manifest.TrackMetadata{
		{Track: manifest.TrackDesc{ID: "video", Kind: manifest.TrackVideo, Codec: "avc1.64001f", Timescale: 90000, ColorInfo: "bt709"}},
		{Track: manifest.TrackDesc{ID: "audio", Kind: manifest.TrackAudio, Codec: "mp4a.40.2", Timescale: 48000, Channels: 2, SampleRate: 48000}},
	}}
	if damaged {
		f.Tracks = append(f.Tracks, manifest.TrackMetadata{
			Track: manifest.TrackDesc{ID: "meta-conflict", Kind: manifest.TrackAudio, Timescale: 48000, Channels: 6, ChannelLayout: "stereo", SampleRate: 48000},
			Hints: []manifest.MetadataHint{{Source: "sidecar-b", SampleRate: 44100}},
		})
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(layout.TracksMetaPath(root, group), b, 0o644)
}

func writeStructuralFixture(root, group string) error {
	b := scan.BuildFragment(scan.FragmentSpec{
		TrackID: 9, Sequence: 1, BaseDecodeTime: 1000, TFDTv1: true,
		Samples: []scan.SampleSpec{{Duration: 3000, Size: 8}, {Duration: 3000, Size: 8}},
	})
	cut := scan.TruncateInsideTrun(b, 6)
	if cut == nil {
		return fmt.Errorf("demo: could not truncate trun fixture")
	}
	p := layout.StructuralPath(root, group)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, cut, 0o644)
}

func ftypBox() []byte {
	b := make([]byte, 16)
	b[3] = 16
	copy(b[4:8], "ftyp")
	copy(b[8:12], "iso5")
	return b
}
