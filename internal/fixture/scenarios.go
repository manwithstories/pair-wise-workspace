package fixture

import (
	"encoding/binary"
	"os"

	"github.com/pion/rtp"
)

// DemoSet describes the files produced for the acceptance demo.
type DemoSet struct {
	HealthyPath  string
	DamagedPath  string
	BoundaryPath string
	RTPPath      string
}

// HealthySpec is the three-track review timeline used in the happy path.
func HealthySpec() MP4Spec {
	return MP4Spec{
		Name: "healthy",
		Tracks: []TrackSpec{
			{
				Kind: "video", Timescale: 24_000, SampleCount: 240,
				SampleDelta: 1000, Width: 1280, Height: 720,
				IncludeColr: true, Primaries: 9, Transfer: 16, Matrix: 9,
				FullRange: false, IncludeSyncTable: true, KeyFrameEvery: 12,
			},
			{
				Kind: "audio", Timescale: 48_000, SampleCount: 300,
				SampleDelta: 1024, Channels: 2, SampleRate: 48_000,
			},
			{
				Kind: "markers", Timescale: 1000, SampleCount: 96,
				SampleDelta: 250,
			},
		},
	}
}

// DamagedSpec injects an impossible sample length and a truncation so the
// scanner locates at least two distinct damage segments.
func DamagedSpec() MP4Spec {
	spec := HealthySpec()
	spec.Name = "damaged"
	spec.Tracks[0].BadSampleLength = 77    // invalid length on video sample 77
	spec.Tracks[1].BadSampleLength = 130   // invalid length on audio sample 130
	return spec
}

// BoundarySpec exercises edit lists, a missing sync table and non-integral
// clip alignment.
func BoundarySpec() MP4Spec {
	spec := HealthySpec()
	spec.Name = "boundary"
	spec.Tracks[0].IncludeEditList = true
	// Drop the stss table on the video track so the scanner must flag a
	// missing-sync fault instead of guessing key frames.
	spec.Tracks[0].IncludeSyncTable = false
	spec.Tracks[1].IncludeEditList = true
	// A second audio layout with different channels/rate for metadata checks.
	spec.Tracks[1].Channels = 1
	spec.Tracks[1].SampleRate = 44_100
	return spec
}

// WriteDemoSet renders every demo file into dir and returns their paths.
func WriteDemoSet(dir string) (DemoSet, error) {
	set := DemoSet{
		HealthyPath:  dir + "/healthy.mp4",
		DamagedPath:  dir + "/damaged.mp4",
		BoundaryPath: dir + "/boundary.mp4",
		RTPPath:      dir + "/voice.rtp1",
	}
	if err := WriteMP4(set.HealthyPath, HealthySpec()); err != nil {
		return set, err
	}
	if err := WriteMP4(set.DamagedPath, DamagedSpec()); err != nil {
		return set, err
	}
	if err := WriteMP4(set.BoundaryPath, BoundarySpec()); err != nil {
		return set, err
	}
	if err := WriteRTPCapture(set.RTPPath, 180); err != nil {
		return set, err
	}
	return set, nil
}

// WriteRTPCapture writes the "RTP1" framed capture: magic, then repeated
// uint32 length + datagram records. One deliberate sequence gap is inserted.
func WriteRTPCapture(path string, packets int) error {
	const ssrc = 0xC0FFEE
	var out []byte
	out = append(out, 'R', 'T', 'P', '1')
	for i := 0; i < packets; i++ {
		seq := uint16(i)
		if i >= 60 {
			seq = uint16(i + 3) // gap of three sequence numbers after #59
		}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version: 2, PayloadType: 96,
				SequenceNumber: seq, Timestamp: uint32(i) * 160, SSRC: ssrc,
				Marker: i == packets-1,
			},
			Payload: []byte{0xDE, 0xAD},
		}
		raw, err := pkt.Marshal()
		if err != nil {
			return err
		}
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(raw)))
		out = append(out, hdr[:]...)
		out = append(out, raw...)
	}
	return os.WriteFile(path, out, 0o644)
}
