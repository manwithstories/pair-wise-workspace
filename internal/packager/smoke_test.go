package packager

import (
	"bytes"
	"io"
	"testing"

	mp4 "github.com/abema/go-mp4"
)

// TestRoundtripWritesParseableBoxes writes an init segment and a media
// segment, then parses them back with go-mp4 to prove the box hierarchy and
// trun data offsets are well-formed.
func TestRoundtripWritesParseableBoxes(t *testing.T) {
	tracks := []TrackConfig{
		{TrackID: videoTrackID, Timescale: 90000, Video: &VideoConfig{
			Width: 320, Height: 180,
			SPS: []byte{0x67, 0x42, 0x00, 0x14},
			PPS: []byte{0x68, 0xce, 0x38, 0x80},
		}},
		{TrackID: audioTrackID, Timescale: 48000, Audio: &AudioConfig{
			SampleRate: 48000, Channels: 1,
		}},
	}
	initBytes, err := EncodeToBytes(func(ws io.WriteSeeker) error {
		return WriteInitSegment(ws, tracks)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mp4.ReadBoxStructure(bytes.NewReader(initBytes),
		func(h *mp4.ReadHandle) (interface{}, error) { return h.Expand() }); err != nil {
		t.Fatalf("init segment not parseable: %v", err)
	}

	seg := MediaSegment{Sequence: 1, Samples: map[uint32][]Sample{
		videoTrackID: {
			{Data: bytes.Repeat([]byte{0xAA}, 100), Duration: 3000, IsSync: true},
			{Data: bytes.Repeat([]byte{0xBB}, 80), Duration: 3000},
		},
		audioTrackID: {
			{Data: bytes.Repeat([]byte{0xCC}, 1920), Duration: 960},
		},
	}}
	segBytes, err := EncodeToBytes(func(ws io.WriteSeeker) error {
		return WriteMediaSegment(ws, seg)
	})
	if err != nil {
		t.Fatal(err)
	}
	var trunOffsets []int32
	_, err = mp4.ReadBoxStructure(bytes.NewReader(segBytes), func(h *mp4.ReadHandle) (interface{}, error) {
		if h.BoxInfo.Type == mp4.BoxTypeTrun() {
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			trunOffsets = append(trunOffsets, box.(*mp4.Trun).DataOffset)
		}
		return h.Expand()
	})
	if err != nil {
		t.Fatalf("media segment not parseable: %v", err)
	}
	if len(trunOffsets) != 2 {
		t.Fatalf("trun count = %d, want 2", len(trunOffsets))
	}
	moofSize := 8 + 16 + trafSize(2) + trafSize(1)
	wantVideo := int32(moofSize + 8)
	wantAudio := wantVideo + 180 // 100+80 video payload bytes
	if trunOffsets[0] != wantVideo || trunOffsets[1] != wantAudio {
		t.Fatalf("data offsets video=%d audio=%d, want %d %d",
			trunOffsets[0], trunOffsets[1], wantVideo, wantAudio)
	}
}
