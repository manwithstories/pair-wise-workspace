package adapter

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fixture"
	"mediad/internal/media"
)

// buildFixture returns a one-camera fixture tree and its media directory.
func buildFixture(t *testing.T, opts fixture.Options) (string, *fixture.Info) {
	t.Helper()
	root := t.TempDir()
	opts.Root = root
	if opts.Cams == 0 {
		opts.Cams = 1
	}
	if opts.Segments == 0 {
		opts.Segments = 2
	}
	info, err := fixture.Generate(opts)
	if err != nil {
		t.Fatalf("fixture.Generate: %v", err)
	}
	return root, info
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCMAFParseInitAndSegment(t *testing.T) {
	root, info := buildFixture(t, fixture.Options{Seed: 1, Profile: fixture.ProfileClean})
	cam := info.CameraList[0]
	a := &CMAFV1{}
	init, err := a.ParseInit(readFile(t, filepath.Join(root, "media", cam.Name, "init.mp4")), media.SourcePos{})
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	if len(init.Tracks) != 1 {
		t.Fatalf("tracks=%d", len(init.Tracks))
	}
	if init.Tracks[0].Codec != "avc1" {
		t.Fatalf("codec=%q", init.Tracks[0].Codec)
	}
	if len(init.Tracks[0].SampleEntry) == 0 {
		t.Fatal("sample entry not preserved")
	}
	keys, err := MapTracks(init, RenditionDesc{Camera: cam.Name, Language: "und", Role: "main", Codec: "avc1", Type: "video"})
	if err != nil {
		t.Fatal(err)
	}
	data := readFile(t, filepath.Join(root, "media", cam.Name, "seg0.m4s"))
	samples, err := a.ParseSegment(SegmentRequest{
		Data: data, SegmentIndex: 0, Init: init, Keys: keys, Refs: map[uint32]int64{},
		Limits: media.DefaultLimits(),
	})
	if err != nil {
		t.Fatalf("ParseSegment: %v", err)
	}
	if len(samples) != info.FramesPerFrag {
		t.Fatalf("samples=%d want %d", len(samples), info.FramesPerFrag)
	}
	if samples[1].DTS <= samples[0].DTS {
		t.Fatal("decode times not increasing")
	}
	// Encoded bytes must be carried through unchanged.
	for i, s := range samples {
		want := fixture.RTPSamplePayload(cam, 0, i)
		if !bytes.Equal(s.Data, want) {
			t.Fatalf("sample %d bytes altered by adapter", i)
		}
	}
}

func TestCMAFTruncatedSegmentIsolated(t *testing.T) {
	root, info := buildFixture(t, fixture.Options{Seed: 2, Profile: fixture.ProfileCorrupt, Segments: 180, TruncateMoof: 17})
	cam := info.CameraList[0]
	a := &CMAFV1{}
	init, err := a.ParseInit(readFile(t, filepath.Join(root, "media", cam.Name, "init.mp4")), media.SourcePos{})
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := MapTracks(init, RenditionDesc{Camera: cam.Name, Role: "main", Codec: "avc1"})
	truncated := readFile(t, filepath.Join(root, "media", cam.Name, "seg90.m4s"))
	_, err = a.ParseSegment(SegmentRequest{
		Data: truncated, SegmentIndex: 90, Init: init, Keys: keys,
		Limits: media.DefaultLimits(),
	})
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if got := media.CodeOf(err); got != media.CodeTruncatedBox {
		t.Fatalf("code=%s want ERR_TRUNCATED_BOX (err=%v)", got, err)
	}
	if !media.Isolated(err) {
		t.Fatal("truncation error should be classified as isolated")
	}
}

func TestCMAFDetectsPayloadTruncation(t *testing.T) {
	// Cutting bytes from the mdat (not the moof) must still be caught: the
	// declared sample extent runs past the available bytes.
	root, info := buildFixture(t, fixture.Options{Seed: 3, Profile: fixture.ProfileClean})
	cam := info.CameraList[0]
	a := &CMAFV1{}
	init, _ := a.ParseInit(readFile(t, filepath.Join(root, "media", cam.Name, "init.mp4")), media.SourcePos{})
	keys, _ := MapTracks(init, RenditionDesc{Camera: cam.Name, Role: "main", Codec: "avc1"})
	data := readFile(t, filepath.Join(root, "media", cam.Name, "seg0.m4s"))
	cut := append([]byte(nil), data[:len(data)-32]...)
	_, err := a.ParseSegment(SegmentRequest{Data: cut, Init: init, Keys: keys, Limits: media.DefaultLimits()})
	if err == nil || media.CodeOf(err) != media.CodeTruncatedBox {
		t.Fatalf("expected ERR_TRUNCATED_BOX, got %v", err)
	}
}

func TestMapTracksConflict(t *testing.T) {
	init := &InitSegment{Tracks: []*InitTrack{
		{ID: 1, Kind: media.KindVideo, Codec: "avc1", Timescale: 90000},
		{ID: 2, Kind: media.KindVideo, Codec: "avc1", Timescale: 90000},
	}}
	_, err := MapTracks(init, RenditionDesc{Camera: "cam1", Role: "main", Codec: "avc1", Type: "video"})
	if err == nil || media.CodeOf(err) != media.CodeTrackConflict {
		t.Fatalf("expected ERR_TRACK_CONFLICT, got %v", err)
	}
}

func TestMapTracksAudioRole(t *testing.T) {
	init := &InitSegment{Tracks: []*InitTrack{
		{ID: 1, Kind: media.KindAudio, Codec: "mp4a", Timescale: 48000},
	}}
	keys, err := MapTracks(init, RenditionDesc{Camera: "cam1", Role: "main", Codec: "mp4a", Type: "audio"})
	if err != nil {
		t.Fatal(err)
	}
	if keys[1].Role != "audio" {
		t.Fatalf("audio role=%q", keys[1].Role)
	}
}

func TestRequiredTrackMissing(t *testing.T) {
	mapped := map[uint32]media.TrackKey{
		1: {Camera: "cam1", Language: "und", Codec: "avc1", Role: "main"},
	}
	req := []media.TrackKey{{Camera: "cam1", Language: "und", Codec: "hevc", Role: "main"}}
	err := CheckRequired(mapped, req, media.SourcePos{})
	if err == nil || media.CodeOf(err) != media.CodeRequiredTrack {
		t.Fatalf("expected ERR_REQUIRED_TRACK_MISSING, got %v", err)
	}
	// Same key with a matching codec must pass.
	if err := CheckRequired(mapped, []media.TrackKey{{Camera: "cam1", Language: "und", Codec: "avc1", Role: "main"}}, media.SourcePos{}); err != nil {
		t.Fatalf("expected match, got %v", err)
	}
}

func TestTrackSetCollision(t *testing.T) {
	set := NewTrackSet()
	a := map[uint32]media.TrackKey{1: {Camera: "cam1", Language: "und", Codec: "avc1", Role: "main"}}
	b := map[uint32]media.TrackKey{7: {Camera: "cam1", Language: "und", Codec: "avc1", Role: "main"}}
	if err := set.Add("src-a", a); err != nil {
		t.Fatal(err)
	}
	if err := set.Add("src-b", b); err == nil {
		t.Fatal("expected collision")
	} else if media.CodeOf(err) != media.CodeTrackConflict {
		t.Fatalf("code=%s", media.CodeOf(err))
	}
}

func TestRegistryUnsupportedVersion(t *testing.T) {
	reg := DefaultRegistry()
	if _, err := reg.Get(ContainerCMAF, 99); err == nil || media.CodeOf(err) != media.CodeUnsupported {
		t.Fatalf("expected ERR_UNSUPPORTED, got %v", err)
	}
	if _, err := reg.Get(ContainerRTP, 1); err != nil {
		t.Fatalf("rtp adapter missing: %v", err)
	}
}

func TestRegistryVersionedAdapters(t *testing.T) {
	reg := DefaultRegistry()
	for _, tc := range []struct {
		c Container
		v int
	}{{ContainerCMAF, 1}, {ContainerTS, 1}, {ContainerRTP, 1}} {
		a, err := reg.Get(tc.c, tc.v)
		if err != nil {
			t.Fatalf("%s@v%d: %v", tc.c, tc.v, err)
		}
		if a.Kind() != tc.c || a.Version() != tc.v {
			t.Fatalf("adapter identity mismatch for %s@v%d", tc.c, tc.v)
		}
	}
}

// buildTS constructs a minimal single-stream TS segment with one PES packet
// carrying the supplied PTS/DTS, so the TS adapter can be exercised without
// external muxers.
func buildTS(t *testing.T, pts, dts int64, payload []byte) []byte {
	t.Helper()
	pmtPID := uint16(0x1000)
	streamPID := uint16(0x1001)

	psisec := func(tableID byte, body []byte) []byte {
		sec := make([]byte, 3+len(body)+4)
		sec[0] = tableID
		length := len(body) + 4
		sec[1] = 0xB0 | byte(length>>8)
		sec[2] = byte(length)
		copy(sec[3:], body)
		return sec
	}
	// PAT body: transport_stream_id(2), version(1), section(1), last(1),
	// then one program entry: program_number(2) + PID(2).
	patBody := []byte{0x00, 0x01, 0xC1, 0x00, 0x00, 0x00, 0x01, byte(pmtPID >> 8), byte(pmtPID)}
	pat := psisec(0x00, patBody)
	// PMT body: program_number(2), version(1), section(1), last(1),
	// PCR_PID(2), program_info_length(2), then one H.264 stream entry.
	pmtBody := []byte{0x00, 0x01, 0xC1, 0x00, 0x00,
		byte(pmtPID >> 8), byte(pmtPID), 0xF0, 0x00,
		0x1B, byte(streamPID >> 8), byte(streamPID), 0xF0, 0x00}
	pmt := psisec(0x02, pmtBody)

	packetize := func(pid uint16, pusi bool, payload []byte) []byte {
		pkt := make([]byte, tsPacketSize)
		pkt[0] = tsSyncByte
		pkt[1] = byte(pid >> 8)
		if pusi {
			pkt[1] |= 0x40
		}
		pkt[2] = byte(pid)
		afc := byte(0x10)
		if len(payload) < tsPacketSize-4 {
			afc = 0x30
		}
		pkt[3] = afc
		off := 4
		if afc == 0x30 {
			stuff := tsPacketSize - 4 - 1 - len(payload)
			pkt[off] = byte(stuff)
			if stuff > 0 {
				pkt[off+1] = 0x00
				for i := 0; i < stuff-1; i++ {
					pkt[off+2+i] = 0xFF
				}
			}
			off += 1 + stuff
		}
		copy(pkt[off:], payload)
		return pkt
	}

	enc := func(v int64) []byte {
		return []byte{
			byte(0x20 | ((v>>30)&0x07)<<1 | 1),
			byte(v >> 22),
			byte((v>>14)&0xFE | 1),
			byte(v >> 7),
			byte((v<<1)&0xFE | 1),
		}
	}
	pes := []byte{0x00, 0x00, 0x01, 0xE0, 0x00, 0x00, 0x84, 0xC0, 0x0A}
	pes = append(pes, enc(pts)...)
	pes = append(pes, enc(dts)...)
	pes = append(pes, payload...)

	var out []byte
	out = append(out, packetize(pidPAT, true, append([]byte{0x00}, pat...))...)
	out = append(out, packetize(pmtPID, true, append([]byte{0x00}, pmt...))...)
	out = append(out, packetize(streamPID, true, pes)...)
	return out
}

func TestTSParseInitAndSegment(t *testing.T) {
	a := &TSV1{}
	payload := bytes.Repeat([]byte{0xAB}, 64)
	data := buildTS(t, 90000, 90000, payload)
	init, err := a.ParseInit(data, media.SourcePos{})
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}
	if len(init.Tracks) != 1 || init.Tracks[0].Codec != "avc1" {
		t.Fatalf("tracks=%+v", init.Tracks)
	}
	keys, err := MapTracks(init, RenditionDesc{Camera: "cam1", Role: "main", Codec: "avc1"})
	if err != nil {
		t.Fatal(err)
	}
	samples, err := a.ParseSegment(SegmentRequest{Data: data, Init: init, Keys: keys, Limits: media.DefaultLimits()})
	if err != nil {
		t.Fatalf("ParseSegment: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples=%d", len(samples))
	}
	if samples[0].PTS != 90000 || samples[0].DTS != 90000 {
		t.Fatalf("timing wrong: pts=%d dts=%d", samples[0].PTS, samples[0].DTS)
	}
	if !bytes.Equal(samples[0].Data, payload) {
		t.Fatal("TS payload bytes altered")
	}
}

func TestTSTruncatedRejected(t *testing.T) {
	a := &TSV1{}
	data := buildTS(t, 90000, 90000, bytes.Repeat([]byte{0x01}, 64))
	cut := data[:len(data)-10]
	_, err := a.ParseInit(cut, media.SourcePos{})
	if err == nil || media.CodeOf(err) != media.CodeTruncatedBox {
		t.Fatalf("expected ERR_TRUNCATED_BOX, got %v", err)
	}
}
