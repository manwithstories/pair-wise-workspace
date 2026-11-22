package normalize

import (
	"strings"
	"testing"

	"mediaroute/internal/clock"
)

func TestLayouts(t *testing.T) {
	for _, tc := range []struct {
		l   Layout
		ch  int
		str string
	}{{Mono, 1, "mono"}, {Stereo, 2, "stereo"}, {Surround51, 6, "5.1"}} {
		if tc.l.Channels() != tc.ch || tc.l.String() != tc.str {
			t.Fatalf("layout %v", tc.l)
		}
		got, err := LayoutFromChannels(tc.ch)
		if err != nil || got != tc.l {
			t.Fatalf("from channels %d: %v %v", tc.ch, got, err)
		}
	}
	if _, err := LayoutFromChannels(4); err == nil {
		t.Fatal("4 channels accepted")
	}
}

func TestNormalizeAudio(t *testing.T) {
	house := AudioTrack{Rate: clock.Rate48000, Layout: Stereo}
	d, err := NormalizeAudio(house, AudioTrack{Rate: clock.Rate48000, Layout: Stereo})
	if err != nil || d.Note != "passthrough" {
		t.Fatalf("passthrough: %+v %v", d, err)
	}
	d, err = NormalizeAudio(house, AudioTrack{Rate: clock.Rate44100, Layout: Mono})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.Note, "44100->48000") || !strings.Contains(d.Note, "mono->stereo") {
		t.Fatalf("note %q", d.Note)
	}
	if _, err := NormalizeAudio(house, AudioTrack{Rate: 22050, Layout: Mono}); err == nil {
		t.Fatal("unsupported rate accepted")
	}
	// 5.1 to stereo is a metadata-level remap decision.
	d, err = NormalizeAudio(house, AudioTrack{Rate: clock.Rate48000, Layout: Surround51})
	if err != nil || !strings.Contains(d.Note, "5.1->stereo") {
		t.Fatalf("5.1: %+v %v", d, err)
	}
}

func TestResolveColorPriority(t *testing.T) {
	c709 := ColorMeta{"bt709", "bt709", "bt709"}
	c601 := ColorMeta{"bt601", "bt601", "smpte170m"}

	// Container wins over bitstream; conflict recorded.
	d := ResolveColor(3, c709, c601)
	if d.Source != SourceContainer || !d.Conflict || d.Chosen != c709 {
		t.Fatalf("conflict: %+v", d)
	}
	if !strings.Contains(d.String(), "conflict") {
		t.Fatalf("decision log %q", d.String())
	}
	// Bitstream used when container is silent.
	d = ResolveColor(4, ColorMeta{}, c601)
	if d.Source != SourceBitstream || d.Conflict || d.Chosen != c601 {
		t.Fatalf("bitstream: %+v", d)
	}
	// Default when both are missing.
	d = ResolveColor(5, ColorMeta{}, ColorMeta{})
	if d.Source != SourceDefault || d.Chosen != DefaultColor {
		t.Fatalf("default: %+v", d)
	}
	// Agreement is not a conflict.
	d = ResolveColor(6, c709, c709)
	if d.Conflict {
		t.Fatalf("agreement flagged: %+v", d)
	}
}
