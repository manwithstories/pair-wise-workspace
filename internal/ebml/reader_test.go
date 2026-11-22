package ebml

import (
	"testing"

	"archivecheck/internal/fault"
)

func sampleTracks() []testTrack {
	return []testTrack{
		{Number: 1, Kind: TrackVideo, Codec: "V_VP8", Default: 40_000_000},
		{Number: 2, Kind: TrackAudio, Codec: "A_PCM/INT/LIT", Default: 32_000_000},
		{Number: 3, Kind: TrackSubtitle, Codec: "S_TEXT/UTF8"},
	}
}

func simpleBlock(track uint64, rel int16, n int, key bool) []byte {
	h := EncodeBlockHeader(track, rel, 1, "", nil)
	if !key {
		h[3] &= 0x7F
	}
	payload := make([]byte, n)
	for i := range payload {
		payload[i] = byte(i)
	}
	return append(h, payload...)
}

func scanBlocks(t *testing.T, raw []byte, opts Options) (*BlockReader, *fault.Set, error) {
	t.Helper()
	fs := &fault.Set{}
	if opts.Faults == nil {
		opts.Faults = fs
	}
	r := NewReader(memReaderAt(raw), int64(len(raw)), opts)
	br := NewBlockReader(r)
	err := br.Scan()
	return br, opts.Faults, err
}

func TestScanKnownSizeCluster(t *testing.T) {
	raw := buildMatroska(t, sampleTracks(), [][]byte{
		simpleBlock(1, 0, 10, true),
		simpleBlock(2, 0, 20, true),
		simpleBlock(3, 20000, 5, true),
		simpleBlock(1, 32000, 11, false),
	}, buildOpts{})

	br, fs, err := scanBlocks(t, raw, Options{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if fs.Len() != 0 {
		t.Fatalf("unexpected faults: %v", fs.Faults())
	}
	h := br.Header()
	if h.TimestampScale != 1_000_000 {
		t.Fatalf("timestamp scale %d", h.TimestampScale)
	}
	if len(h.Tracks) != 3 || h.Tracks[1].Kind != TrackVideo || h.Tracks[2].Kind != TrackAudio || h.Tracks[3].Kind != TrackSubtitle {
		t.Fatalf("tracks: %+v", h.Tracks)
	}
	if h.Tracks[1].PixelWidth != 64 || h.Tracks[2].SampleRate != 48000 {
		t.Fatalf("track details: %+v", h.Tracks)
	}
	if len(br.blocks) != 4 {
		t.Fatalf("blocks=%d", len(br.blocks))
	}
	b := br.blocks[3]
	if b.Track != 1 || b.Time.Millis() != 32 || b.Keyframe {
		t.Fatalf("block3: %+v", b)
	}
	if br.blocks[0].FrameDigest == 0 {
		t.Fatal("missing digest")
	}
}

func TestUnknownSizeSegmentAndCluster(t *testing.T) {
	blocks := [][]byte{
		simpleBlock(1, 0, 8, true),
		simpleBlock(2, 1000, 8, true),
	}
	raw := buildMatroska(t, sampleTracks(), blocks, buildOpts{segmentUnknown: true, unknownCluster: true})

	br, fs, err := scanBlocks(t, raw, Options{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if fs.Len() != 0 {
		t.Fatalf("unknown-size walk produced faults: %v", fs.Faults())
	}
	if len(br.blocks) != 2 {
		t.Fatalf("blocks=%d want 2", len(br.blocks))
	}
}

func TestTruncatedElementReportsOffsetAndStopsAtEOF(t *testing.T) {
	full := buildMatroska(t, sampleTracks(), [][]byte{simpleBlock(1, 0, 16, true)}, buildOpts{})
	cut := len(full) - 10
	raw := full[:cut]
	br, fs, err := scanBlocks(t, raw, Options{})
	if err != nil {
		t.Fatalf("scan must tolerate truncation: %v", err)
	}
	if fs.Count(fault.KindTruncated) == 0 {
		t.Fatalf("no truncation fault: %v", fs.Kinds())
	}
	for _, b := range br.blocks {
		if b.PayloadOff+int64(b.FrameBytes) > int64(cut) {
			t.Fatalf("emitted partial block beyond source end at 0x%x", b.Offset)
		}
	}
	f := fs.First()
	if !f.HasOffset || f.Offset.Value > uint64(cut) {
		t.Fatalf("fault offset %v outside source", f)
	}
}

func TestTruncatedHeaderAtEnd(t *testing.T) {
	full := buildMatroska(t, sampleTracks(), nil, buildOpts{})
	raw := full[:len(full)-2]
	_, fs, err := scanBlocks(t, raw, Options{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if fs.Count(fault.KindTruncated) == 0 {
		t.Fatalf("no truncation: %v", fs.Kinds())
	}
}

func TestLacedBlockSizes(t *testing.T) {
	sizes := []int64{3, 5, 7}
	h := EncodeBlockHeader(2, 0, 3, "xiph", sizes)
	body := make([]byte, 3+5+7)
	for i := range body {
		body[i] = byte(i)
	}
	raw := buildMatroska(t, sampleTracks(), [][]byte{append(h, body...)}, buildOpts{})
	br, fs, err := scanBlocks(t, raw, Options{})
	if err != nil || fs.Len() != 0 {
		t.Fatalf("scan=%v faults=%v", err, fs.Faults())
	}
	if len(br.blocks) != 1 {
		t.Fatalf("blocks=%d", len(br.blocks))
	}
	got := br.blocks[0]
	if got.Lace.Frames != 3 || got.Lace.Kind != "xiph" || got.FrameBytes != 15 {
		t.Fatalf("lace: %+v frames=%d", got.Lace, got.FrameBytes)
	}
	if got.Lace.Sizes[0] != 3 || got.Lace.Sizes[2] != 7 {
		t.Fatalf("sizes: %v", got.Lace.Sizes)
	}
}

func TestEBMLLacingDelta(t *testing.T) {
	h := EncodeBlockHeader(2, 0, 3, "ebml", []int64{4, 6, 10})
	body := make([]byte, 4+6+10)
	raw := buildMatroska(t, sampleTracks(), [][]byte{append(h, body...)}, buildOpts{})
	br, fs, err := scanBlocks(t, raw, Options{})
	if err != nil || fs.Len() != 0 {
		t.Fatalf("scan=%v faults=%v", err, fs.Faults())
	}
	if len(br.blocks) != 1 {
		t.Fatalf("blocks=%d", len(br.blocks))
	}
	got := br.blocks[0]
	if got.Lace.Kind != "ebml" || got.FrameBytes != 20 {
		t.Fatalf("lace: %+v bytes=%d", got.Lace, got.FrameBytes)
	}
	if got.Lace.Sizes[0] != 4 || got.Lace.Sizes[1] != 6 || got.Lace.Sizes[2] != 10 {
		t.Fatalf("sizes: %v", got.Lace.Sizes)
	}
}

func TestBadLacingDescribed(t *testing.T) {
	h := EncodeBlockHeader(2, 0, 3, "xiph", []int64{3, 5, 7})
	raw := buildMatroska(t, sampleTracks(), [][]byte{append(h, make([]byte, 4)...)}, buildOpts{})
	br, fs, err := scanBlocks(t, raw, Options{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if fs.Count(fault.KindBadElement) == 0 && fs.Count(fault.KindTruncated) == 0 {
		t.Fatalf("expected a lacing fault: %v", fs.Kinds())
	}
	if len(br.blocks) != 0 {
		t.Fatal("malformed block must not be emitted")
	}
}

func TestBlockOutsideCluster(t *testing.T) {
	raw := []byte{0xA3, 0x85, 0x81, 0x00, 0x00, 0x80, 0x00}
	_, fs, err := scanBlocks(t, raw, Options{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if fs.Count(fault.KindBadElement) == 0 {
		t.Fatalf("kinds: %v", fs.Kinds())
	}
}

func TestTrackTimestampScaleApplied(t *testing.T) {
	tracks := []testTrack{{Number: 1, Kind: TrackVideo, Codec: "V_VP8", Scale: "1.5", Default: 40_000_000}}
	raw := buildMatroska(t, tracks, [][]byte{
		simpleBlock(1, 500, 4, true),
	}, buildOpts{clusterTick: 100_000})
	br, fs, err := scanBlocks(t, raw, Options{})
	if err != nil || fs.Len() != 0 {
		t.Fatalf("scan=%v faults=%v", err, fs.Faults())
	}
	// (100ms cluster + 0.5ms) * 1.5 = 150.75ms.
	if got := br.blocks[0].Time.Millis(); got != 150.75 {
		t.Fatalf("track scale time=%v want 150.75", got)
	}
}

func TestNeverReadsPastFileEnd(t *testing.T) {
	raw := buildMatroska(t, sampleTracks(), [][]byte{simpleBlock(1, 0, 4, true)}, buildOpts{})
	br, fs, err := scanBlocks(t, raw, Options{})
	if err != nil || fs.Len() != 0 {
		t.Fatalf("scan=%v faults=%v", err, fs.Faults())
	}
	for _, b := range br.blocks {
		if b.PayloadOff+int64(b.FrameBytes) > int64(len(raw)) {
			t.Fatal("read past file end")
		}
	}
}

func TestWalkOrderAndDepth(t *testing.T) {
	raw := buildMatroska(t, sampleTracks(), [][]byte{simpleBlock(1, 0, 4, true)}, buildOpts{})
	r := NewReader(memReaderAt(raw), int64(len(raw)), Options{})
	var enters, exits int
	var sawSegment, sawCluster bool
	err := r.Walk(func(ph Phase, el Element) error {
		switch ph {
		case PhaseEnter:
			enters++
			if el.ID == IDSegment {
				sawSegment = true
			}
			if el.ID == IDCluster {
				sawCluster = true
				if el.Depth != 1 {
					t.Fatalf("cluster depth=%d", el.Depth)
				}
			}
		case PhaseExit:
			exits++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawSegment || !sawCluster || enters != exits {
		t.Fatalf("segment=%v cluster=%v enters=%d exits=%d", sawSegment, sawCluster, enters, exits)
	}
}
