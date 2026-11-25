package mp4

import (
	"bytes"
	"testing"
)

func samples(n int, fill byte) []Sample {
	var out []Sample
	for i := 0; i < n; i++ {
		out = append(out, Sample{
			Data:     bytes.Repeat([]byte{fill + byte(i)}, 32+i),
			Duration: 960,
		})
	}
	return out
}

func TestRoundTripPerfect(t *testing.T) {
	src := samples(6, 0x10)
	rep, table, err := RoundTrip(src, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Percent() != 100.0 {
		t.Fatalf("roundtrip=%.2f%% first diff at %s", rep.Percent(), rep.FirstDiff)
	}
	if table.Timescale != 48000 {
		t.Fatalf("timescale=%d", table.Timescale)
	}
	if len(table.Samples) != 6 {
		t.Fatalf("samples=%d, want 6", len(table.Samples))
	}
	for i, s := range table.Samples {
		if !bytes.Equal(s, src[i].Data) {
			t.Fatalf("sample %d payload mismatch", i)
		}
	}
}

func TestVariableDurationsRoundTrip(t *testing.T) {
	src := []Sample{
		{Data: []byte{1}, Duration: 960},
		{Data: []byte{2}, Duration: 120}, // variable duration
		{Data: []byte{3}, Duration: 960},
	}
	rep, table, err := RoundTrip(src, 48000)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Percent() != 100.0 {
		t.Fatalf("roundtrip=%.2f%% first diff %s", rep.Percent(), rep.FirstDiff)
	}
	// stts must compress the two 960-tick runs around the 120-tick unit.
	if len(table.Deltas) != 3 {
		t.Fatalf("stts entries=%d, want 3: %+v", len(table.Deltas), table.Deltas)
	}
}

func TestFirstDiffLocated(t *testing.T) {
	src := samples(4, 0x20)
	data, err := Mux(src, 48000)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the first sample payload inside mdat (last bytes of file).
	data[len(data)-1] ^= 0xFF
	table, err := Demux(data)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(table.Samples[len(table.Samples)-1], src[len(src)-1].Data) {
		t.Fatal("corruption not visible in demuxed samples")
	}
}

func TestKeyframeHitAllAnchored(t *testing.T) {
	src := samples(5, 0x30)
	_, table, err := RoundTrip(src, 48000)
	if err != nil {
		t.Fatal(err)
	}
	anchors := Anchors(table)
	if len(anchors) != 5 {
		t.Fatalf("anchors=%v, want 5", anchors)
	}
	if hit := KeyframeHit(anchors, []uint32{1, 2, 3, 4, 5}); hit != 100.0 {
		t.Fatalf("keyframe_hit=%.2f%%, want 100", hit)
	}
	if hit := KeyframeHit(anchors, []uint32{1, 9}); hit != 50.0 {
		t.Fatalf("keyframe_hit=%.2f%%, want 50", hit)
	}
}
