package fixture

import (
	"fmt"

	"github.com/pairwise/mediad/internal/manifest"
)

// DASHManifest builds the three-bitrate DASH fixture.
func (b *Bundle) DASHManifest() (*manifest.DASHManifest, []byte, error) {
	m := &manifest.DASHManifest{
		Type:                      "static",
		MediaPresentationDuration: "PT24S",
		MinBufferTime:             "PT2S",
		Period: manifest.DASHPeriod{
			ID:       "0",
			Duration: "PT24S",
			AdaptationSet: manifest.DASHAdaptationSet{
				ContentType: "video",
				SegmentTpl: manifest.DASHSegmentTpl{
					Timescale:      90000,
					Initialization: "init-$RepresentationID$.mp4",
					Media:          "seg-$RepresentationID$-$Number%03d$.m4s",
					StartNumber:    1,
					Duration:       180000,
				},
				Representations: []manifest.DASHRepresentation{
					{ID: "high", Bandwidth: 5_000_000, Width: 1920, Height: 1080, Codecs: "avc1.640028", MimeType: "video/mp4"},
					{ID: "low", Bandwidth: 800_000, Width: 640, Height: 360, Codecs: "avc1.42c01e", MimeType: "video/mp4"},
					{ID: "mid", Bandwidth: 2_200_000, Width: 1280, Height: 720, Codecs: "avc1.4d401f", MimeType: "video/mp4"},
				},
			},
		},
	}
	data, err := manifest.SerializeDASH(m)
	if err != nil {
		return nil, nil, err
	}
	return m, data, nil
}

// HLSManifest builds twelve segments with exactly two discontinuities.
func (b *Bundle) HLSManifest() (*manifest.HLSPlaylist, []byte) {
	p := &manifest.HLSPlaylist{Version: 4, TargetDuration: 2, MediaSequence: 0, EndList: false}
	for global := 0; global < 12; global++ {
		track := trackSpecs[global/4]
		inTrack := global % 4
		uri := fmt.Sprintf("segments/%s-segment-%03d.m4s", track.id, inTrack)
		p.Segments = append(p.Segments, manifest.HLSSegment{
			Index:               global,
			URI:                 uri,
			Duration:            2.0,
			DiscontinuityBefore: global == 4 || global == 8,
		})
	}
	p.EndList = true
	data := manifest.SerializeHLS(p)
	return p, data
}
