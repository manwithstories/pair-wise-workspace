package rtp

import (
	"testing"

	"github.com/pion/rtp"

	"mediad/internal/clock"
	"mediad/internal/model"
)

func TestDecodeUnwrapsTimestampAndWraparound(t *testing.T) {
	pkts := make([][]byte, 4)
	ts := []uint32{0xFFFFFFF0, 0x00000000, 0x00000010, 0x00000020}
	for i, v := range ts {
		raw, err := Encode(uint16(i), v, i == 0, []byte{byte(i), 2, 3})
		if err != nil {
			t.Fatal(err)
		}
		pkts[i] = raw
	}
	tr := Track{
		Source:    model.Source{ID: "rtp-1", Kind: model.KindRTP},
		ClockRate: clock.MustNew(90000, 1),
		Packets:   pkts,
	}
	samples, errs := Decode(tr)
	if len(errs) != 0 || len(samples) != 4 {
		t.Fatalf("decode: %v %v", samples, errs)
	}
	// 回绕后时间戳必须保持单调，且增量为 16。
	if samples[1].Timestamp != samples[0].Timestamp+16 {
		t.Fatalf("wrap delta = %d", samples[1].Timestamp-samples[0].Timestamp)
	}
	for i := 1; i < len(samples); i++ {
		if samples[i].Timestamp <= samples[i-1].Timestamp {
			t.Fatal("unwrapped timestamps must be monotonic")
		}
	}
	if !samples[0].Marker || samples[1].Marker {
		t.Fatal("marker flag not carried through")
	}
}

func TestDecodeSkipsCorruptPacket(t *testing.T) {
	good, _ := Encode(0, 100, true, []byte("a"))
	tr := Track{Packets: [][]byte{{0x80}, good}} // 首包字节不足
	samples, errs := Decode(tr)
	if len(errs) != 1 {
		t.Fatalf("want 1 decode error, got %d", len(errs))
	}
	if len(samples) != 1 || samples[0].Timestamp != 100 {
		t.Fatalf("good packet must survive: %+v", samples)
	}
}

func TestToManifestSamples(t *testing.T) {
	raw1, _ := Encode(0, 500, true, []byte("k"))
	raw2, _ := Encode(1, 560, false, []byte("p"))
	ss, errs := Decode(Track{Packets: [][]byte{raw1, raw2}})
	if len(errs) != 0 {
		t.Fatalf("decode errors: %v", errs)
	}
	got := ToManifestSamples("trk-rtp", ss)
	if len(got) != 2 {
		t.Fatalf("manifest samples = %d", len(got))
	}
	if got[0].PTS != 500 || !got[0].Keyframe || got[0].TrackID != "trk-rtp" {
		t.Fatalf("first sample wrong: %+v", got[0])
	}
	if got[1].Keyframe || got[1].Index != 2 {
		t.Fatalf("second sample wrong: %+v", got[1])
	}
}

func TestEncodeIsValidRTP(t *testing.T) {
	raw, err := Encode(7, 9000, true, []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	var p rtp.Packet
	if err := p.Unmarshal(raw); err != nil {
		t.Fatalf("roundtrip packet: %v", err)
	}
	if p.SequenceNumber != 7 || p.Timestamp != 9000 || !p.Marker {
		t.Fatalf("packet fields lost: %+v", p)
	}
}
