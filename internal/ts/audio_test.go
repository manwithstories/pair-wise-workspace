package ts

import (
	"strings"
	"testing"

	"cinepipe/internal/fault"
)

func TestADTSHeaderRoundTrip(t *testing.T) {
	idx := SampleRateIndex(48000)
	if idx < 0 {
		t.Fatal("48000 unsupported")
	}
	h := BuildADTSHeader(1, uint8(idx), 2, 400)
	f, ok, reason := ParseADTSHeader(h)
	if !ok {
		t.Fatalf("parse: %s", reason)
	}
	if f.SampleRate != 48000 || f.Channels != 2 || f.FrameLen != 400 || f.Profile != "lc" {
		t.Fatalf("frame %+v", f)
	}
	if f.HeaderLen != 7 || f.HasCRC {
		t.Fatalf("header %+v", f)
	}
}

func TestADTSFiveOne(t *testing.T) {
	idx := SampleRateIndex(44100)
	h := BuildADTSHeader(1, uint8(idx), 6, 512)
	f, ok, reason := ParseADTSHeader(h)
	if !ok {
		t.Fatalf("parse: %s", reason)
	}
	if f.Channels != 6 || f.SampleRate != 44100 {
		t.Fatalf("frame %+v", f)
	}
}

func TestADTSRejectsBadSync(t *testing.T) {
	if _, ok, reason := ParseADTSHeader([]byte{0, 0, 0, 0, 0, 0, 0}); ok || reason == "" {
		t.Fatal("bad sync accepted")
	}
	if _, ok, reason := ParseADTSHeader(make([]byte, 4)); ok || reason == "" {
		t.Fatal("short header accepted")
	}
}

func TestAudioAssemblyLocks(t *testing.T) {
	idx := SampleRateIndex(48000)
	aa := NewAudioAssembly(0x102, StreamTypeAAC, "eng")
	desc, changed, errs := aa.Describe(BuildADTSHeader(1, uint8(idx), 2, 400), 0)
	if len(errs) != 0 || !changed {
		t.Fatalf("first frame: changed=%v errs=%v", changed, errs)
	}
	if !aa.Locked() || desc.SampleRate != 48000 || desc.Layout != LayoutStereo {
		t.Fatalf("desc %+v", desc)
	}
	// A second identical frame does not change the description.
	_, changed, errs = aa.Describe(BuildADTSHeader(1, uint8(idx), 2, 400), 500)
	if changed || len(errs) != 0 {
		t.Fatalf("second frame: changed=%v errs=%v", changed, errs)
	}
}

func TestAudioAssemblyDetectsParameterChange(t *testing.T) {
	idx48 := SampleRateIndex(48000)
	idx44 := SampleRateIndex(44100)
	aa := NewAudioAssembly(0x102, StreamTypeAAC, "eng")
	aa.Describe(BuildADTSHeader(1, uint8(idx48), 2, 400), 0)
	_, _, errs := aa.Describe(BuildADTSHeader(1, uint8(idx44), 6, 400), 500)
	if len(errs) != 1 || errs[0].Kind != fault.KindMetadata {
		t.Fatalf("expected metadata diagnostic: %v", errs)
	}
	if !strings.Contains(errs[0].Detail, "48000") {
		t.Fatalf("detail %q", errs[0].Detail)
	}
	// The original description is preserved, not rewritten.
	if d := aa.Description(); d.SampleRate != 48000 || d.Channels != 2 {
		t.Fatalf("description mutated: %+v", d)
	}
}

func TestAudioCompatibility(t *testing.T) {
	a := AudioTrack{Codec: "aac", SampleRate: 48000, Channels: 2, Layout: LayoutStereo}
	b := AudioTrack{Codec: "aac", SampleRate: 48000, Channels: 2, Layout: LayoutStereo}
	if !a.CompatibleWith(b) {
		t.Fatal("identical tracks should mix")
	}
	c := AudioTrack{Codec: "aac", SampleRate: 44100, Channels: 6, Layout: Layout51}
	if a.CompatibleWith(c) {
		t.Fatal("different rate/layout should not mix")
	}
	reasons := DescribeReasons(a, c)
	if len(reasons) != 3 || !strings.Contains(reasons[0], "sample_rate") {
		t.Fatalf("reasons %v", reasons)
	}
}

func TestAC3Description(t *testing.T) {
	aa := NewAudioAssembly(0x200, StreamTypeAC3, "deu")
	hdr := []byte{0x0B, 0x77, 0x00, 0x00, 0x0A, 0x30, 0x00}
	desc, changed, errs := aa.Describe(hdr, 0)
	if len(errs) != 0 || !changed {
		t.Fatalf("ac3: changed=%v errs=%v", changed, errs)
	}
	// fscod 0 = 48000; acmod 1 = mono, lfeon 1 adds one channel.
	if desc.SampleRate != 48000 || desc.Channels != 2 {
		t.Fatalf("ac3 desc %+v", desc)
	}
	if desc.Codec != "ac3" || desc.Language != "deu" {
		t.Fatalf("ac3 meta %+v", desc)
	}
}

func TestLayoutMapping(t *testing.T) {
	cases := map[int]string{1: LayoutMono, 2: LayoutStereo, 6: Layout51, 8: Layout71}
	for n, want := range cases {
		if got := layoutForChannels(n); got != want {
			t.Fatalf("%dch -> %q want %q", n, got, want)
		}
	}
}
