package rtpx

import (
	"bytes"
	"testing"

	"bcastqc/internal/media"
)

func TestPacketizeDeterministicAndDecodable(t *testing.T) {
	mk := func() [][]byte {
		pz := NewPacketizer(media.TrackVideo, media.VideoClockRate, 111, PayloadTypeVideo)
		frames := []Frame{
			{Track: 2, TimeNS: 0, Key: true, Payload: bytes.Repeat([]byte{0x10}, 3000)},
			{Track: 2, TimeNS: media.NSPerSecond / 30, Key: false, Payload: []byte{1, 2, 3}},
		}
		var raws [][]byte
		for i, f := range frames {
			pkts, err := pz.Packetize(f, int64(i)*1_000_000)
			if err != nil {
				t.Fatalf("packetize: %v", err)
			}
			for _, p := range pkts {
				raws = append(raws, p.Raw)
			}
		}
		return raws
	}
	first := mk()
	second := mk()
	if len(first) != len(second) {
		t.Fatalf("run lengths differ: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if !bytes.Equal(first[i], second[i]) {
			t.Fatalf("packet %d nondeterministic", i)
		}
	}
	if len(first) < 4 { // 3000 字节至少 3 个分片 + 第二帧 1 个
		t.Fatalf("expected fragmentation, got %d packets", len(first))
	}

	dec := NewDecoder(media.TrackVideo)
	var decoded []*Decoded
	for _, raw := range first {
		d, err := dec.Decode(raw)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		decoded = append(decoded, d)
	}
	if decoded[0].Seq != 1 {
		t.Fatalf("first seq = %d, want 1", decoded[0].Seq)
	}
	// 序列号连续。
	for i := 1; i < len(decoded); i++ {
		if decoded[i].Seq != decoded[i-1].Seq+1 {
			t.Fatalf("seq gap at %d: %d -> %d", i, decoded[i-1].Seq, decoded[i].Seq)
		}
	}
	// 同帧分片共享时间戳，末包 Marker。
	if decoded[0].RTPTS != decoded[1].RTPTS {
		t.Fatal("fragmented frame timestamps diverged")
	}
	if decoded[2].Marker != true {
		t.Fatal("last fragment should set marker")
	}
}

func TestReassembleRoundTrip(t *testing.T) {
	pz := NewPacketizer(media.TrackAudio, media.AudioClockRate, 222, PayloadTypeAudio)
	payload := bytes.Repeat([]byte{0xAB}, 2500)
	pkts, err := pz.Packetize(Frame{Track: 1, TimeNS: 0, Payload: payload}, 0)
	if err != nil {
		t.Fatalf("packetize: %v", err)
	}
	dec := NewDecoder(media.TrackAudio)
	var decoded []*Decoded
	for _, p := range pkts {
		d, err := dec.Decode(p.Raw)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		decoded = append(decoded, d)
	}
	frames := Reassemble(decoded)
	if len(frames) != 1 || !bytes.Equal(frames[0], payload) {
		t.Fatalf("reassembly mismatch, frames=%d", len(frames))
	}
}
