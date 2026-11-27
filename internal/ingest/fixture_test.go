package ingest

import (
	"bytes"
	"testing"
)

func TestGenerateProducesDecodableAssetShape(t *testing.T) {
	g, err := Generate(DefaultConfig(17))
	if err != nil {
		t.Fatal(err)
	}
	if g.VideoTotal != 600 {
		t.Fatalf("video packets = %d, want 600", g.VideoTotal)
	}
	// 24 seconds of audio in 20 ms packets.
	if g.AudioTotal != 1200 {
		t.Fatalf("audio packets = %d, want 1200", g.AudioTotal)
	}
	if len(g.Assets.AudioExtra) != 19 || string(g.Assets.AudioExtra[:8]) != "OpusHead" {
		t.Fatalf("bad OpusHead: %x", g.Assets.AudioExtra)
	}
	if len(g.Assets.VideoExtra) == 0 || g.Assets.VideoExtra[0] != 1 {
		t.Fatalf("bad avcC: %x", g.Assets.VideoExtra)
	}
	first := g.Video.Packets[0]
	if !first.Keyframe {
		t.Fatal("first video packet should be a keyframe")
	}
	// AVCC framing: the first 4 bytes are a NAL length that fits the packet.
	n := int(uint32(first.Data[0])<<24 | uint32(first.Data[1])<<16 | uint32(first.Data[2])<<8 | uint32(first.Data[3]))
	if n <= 0 || n+4 > len(first.Data) {
		t.Fatalf("first NAL length %d does not fit %d bytes", n, len(first.Data))
	}
	if g.Audio.Packets[0].Slot.End <= g.Audio.Packets[0].Slot.Start {
		t.Fatal("audio slot is not positive")
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	a, err := Generate(DefaultConfig(42))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(DefaultConfig(42))
	if err != nil {
		t.Fatal(err)
	}
	if a.AudioTotal != b.AudioTotal || a.VideoTotal != b.VideoTotal {
		t.Fatal("packet counts differ between runs")
	}
	for i := range a.Audio.Packets {
		if !bytes.Equal(a.Audio.Packets[i].Data, b.Audio.Packets[i].Data) {
			t.Fatalf("audio packet %d differs between runs", i)
		}
	}
	for i := range a.Video.Packets {
		if !bytes.Equal(a.Video.Packets[i].Data, b.Video.Packets[i].Data) {
			t.Fatalf("video packet %d differs between runs", i)
		}
	}
	if !bytes.Equal(a.Assets.AudioExtra, b.Assets.AudioExtra) ||
		!bytes.Equal(a.Assets.VideoExtra, b.Assets.VideoExtra) {
		t.Fatal("extradata differs between runs")
	}
}

func TestGenerateSeedsProduceDifferentAudio(t *testing.T) {
	a, err := Generate(DefaultConfig(1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Generate(DefaultConfig(2))
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for i := range a.Audio.Packets {
		if !bytes.Equal(a.Audio.Packets[i].Data, b.Audio.Packets[i].Data) {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seeds produced identical audio")
	}
}

func TestGenerateMixedDurations(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 250 // 10 seconds
	cfg.OpusDurationProfile = "mixed"
	g, err := Generate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, p := range g.Audio.Packets {
		seen[p.Duration] = true
	}
	if !seen[480] || !seen[960] || !seen[2880] {
		t.Fatalf("expected 10/20/60 ms frames, saw durations %v", seen)
	}
}

func TestGenerateRejectsOversizedConfig(t *testing.T) {
	cfg := DefaultConfig(17)
	cfg.VideoFrames = 100000
	if _, err := Generate(cfg); err == nil {
		t.Fatal("expected limit error for oversized fixture")
	}
}

func TestAnnexBToAVCCSplitsNALUnits(t *testing.T) {
	// Two NAL units: IDR (type 5) and SEI (type 6).
	annexB := []byte{
		0, 0, 0, 1, 0x65, 0xAA, 0xBB,
		0, 0, 0, 1, 0x06, 0xCC,
	}
	avcc, types, err := annexBToAVCC(annexB)
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 2 || types[0] != 5 || types[1] != 6 {
		t.Fatalf("NAL types = %v", types)
	}
	if len(avcc) != 4+3+4+2 {
		t.Fatalf("AVCC length = %d", len(avcc))
	}
	if _, _, err := annexBToAVCC([]byte{1, 2, 3}); err == nil {
		t.Fatal("expected error for data without NAL units")
	}
}
