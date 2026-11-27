package inspect

import (
	"testing"

	"relayaudit/internal/clock"
	"relayaudit/internal/fixture"
)

func scanFixture(t *testing.T, spec fixture.Spec) *Scanner {
	t.Helper()
	seg, err := fixture.Generate(spec)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	sc := NewScanner()
	for _, rec := range seg.Records {
		sc.Push(rec.Track, rec.PTS, rec.Payload, rec.Offset)
	}
	return sc
}

func TestAnnexBDetectsIDRWithParameterSets(t *testing.T) {
	payload := []byte{0, 0, 0, 1, 0x67, 1, 2, 0, 0, 1, 0x68, 3, 4, 0, 0, 0, 1, 0x65, 9}
	typ, sps, pps, ok := AnnexBFrames(payload)
	if !ok || typ != nalIDR || !sps || !pps {
		t.Fatalf("got type=%d sps=%v pps=%v ok=%v", typ, sps, pps, ok)
	}
}

func TestAnnexBDetectsNonIDRSlice(t *testing.T) {
	payload := []byte{0, 0, 0, 1, 0x41, 0x9a, 0x11}
	typ, _, _, ok := AnnexBFrames(payload)
	if !ok || typ != 1 {
		t.Fatalf("type=%d ok=%v", typ, ok)
	}
}

func TestAnnexBNotH264(t *testing.T) {
	if _, _, _, ok := AnnexBFrames([]byte("plain text, no start codes")); ok {
		t.Fatalf("text misidentified as H.264")
	}
}

func TestADTSHeader(t *testing.T) {
	// 48 kHz, 2 channels, frame_length = 12 bytes.
	hdr := []byte{0xFF, 0xF1, 0x4C, 0x80, 0x01, 0x9F, 0xFC}
	hz, ok := ADTSHeaderHz(hdr)
	if !ok || hz != 48000 {
		t.Fatalf("hz=%d ok=%v", hz, ok)
	}
	n, ok := ADTSLength(hdr)
	if !ok || n != 12 {
		t.Fatalf("length=%d ok=%v", n, ok)
	}
	if _, ok := ADTSHeaderHz([]byte{0xFF, 0xF9, 0x00}); ok {
		t.Fatalf("invalid layer accepted")
	}
}

func TestScannerIdentifiesAllFourTracks(t *testing.T) {
	sc := scanFixture(t, fixture.Spec{Seed: 73, Size: 1 << 19})
	st := sc.Summarise()
	if len(st.Tracks) != 4 {
		t.Fatalf("tracks = %v, want 4", st.Tracks)
	}
	if st.TrackCodec[1] != CodecH264 || st.TrackCodec[2] != CodecH264 {
		t.Fatalf("video tracks not H.264: %v", st.TrackCodec)
	}
	if st.TrackCodec[3] != CodecAAC || st.TrackCodec[4] != CodecAAC {
		t.Fatalf("audio tracks not AAC: %v", st.TrackCodec)
	}
	if st.IDRs == 0 || st.AACFrames == 0 {
		t.Fatalf("no frames identified: %+v", st)
	}
	for _, f := range sc.Frames() {
		if f.Type == FrameIDR && !f.SPSPPS {
			t.Fatalf("IDR at %d lacks parameter sets", f.Offset)
		}
	}
}

func TestScannerKeyIntervalIsTwoSeconds(t *testing.T) {
	sc := scanFixture(t, fixture.Spec{Seed: 21, Size: 1 << 21})
	for _, track := range []uint8{1, 2} {
		iv := sc.KeyIntervals(track)
		if len(iv) < 2 {
			t.Fatalf("track %d: only %d IDR intervals", track, len(iv))
		}
		for i, v := range iv {
			if v != 2*clock.Rate {
				t.Fatalf("track %d interval %d = %d ticks, want %d", track, i, v, 2*clock.Rate)
			}
		}
		if got := sc.MeanKeyInterval(track); got != 2*clock.Rate {
			t.Fatalf("track %d mean = %d", track, got)
		}
	}
}

func TestScannerIntervalsSurvivePTSWrap(t *testing.T) {
	sc := scanFixture(t, fixture.Spec{Seed: 5, Size: 1 << 21})
	if sc.WrapCount() == 0 {
		t.Fatalf("fixture did not cross the PTS wrap")
	}
	for track, iv := range map[uint8][]uint64{1: sc.KeyIntervals(1), 2: sc.KeyIntervals(2)} {
		for i, v := range iv {
			if v != 2*clock.Rate {
				t.Fatalf("track %d interval %d across wrap = %d, want %d", track, i, v, 2*clock.Rate)
			}
		}
	}
}

func TestScannerExplicitWrapIsMonotonic(t *testing.T) {
	sc := NewScanner()
	raw := []clock.Stamp{4294900000, 4294903600, 4294907200, 800, 4400, 8000}
	var prev uint64
	for i, r := range raw {
		f := sc.Push(1, r, []byte{0, 0, 0, 1, 0x41, 0x00}, int64(i))
		if i > 0 && f.Ticks <= prev {
			t.Fatalf("tick %d (%d) did not advance past %d", i, f.Ticks, prev)
		}
		prev = f.Ticks
	}
	if sc.WrapCount() != 1 {
		t.Fatalf("wraps = %d, want 1", sc.WrapCount())
	}
}

func TestScannerUnknownPayloadIsReportedRaw(t *testing.T) {
	sc := NewScanner()
	f := sc.Push(1, 0, []byte("not a media frame"), 0)
	if f.Type != FrameUnknown {
		t.Fatalf("type = %v, want raw/unknown", f.Type)
	}
	st := sc.Summarise()
	if st.TrackCodec[1] != CodecRaw {
		t.Fatalf("codec = %q", st.TrackCodec[1])
	}
}

func TestFrameTypeStrings(t *testing.T) {
	cases := map[FrameType]string{FrameIDR: "IDR", FrameNonIDR: "P/B", FrameAAC: "AAC", FrameUnknown: "raw"}
	for ft, want := range cases {
		if got := ft.String(); got != want {
			t.Fatalf("%d = %q, want %q", ft, got, want)
		}
	}
	if !FrameIDR.Keyframe() || FrameNonIDR.Keyframe() {
		t.Fatalf("keyframe classification wrong")
	}
}
