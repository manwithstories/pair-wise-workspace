package segment

import (
	"encoding/binary"
	"testing"
)

func buildTS(pid uint16, pusi bool, payload []byte) []byte {
	p := make([]byte, TSPacketLen)
	p[0] = 0x47
	p[1] = byte(pid>>8) & 0x1F
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	p[3] = 0x10 // payload only
	copy(p[4:], payload)
	return p
}

func buildPES(streamID byte, pts int64, body []byte) []byte {
	enc := EncodePTS(pts)
	out := []byte{0x00, 0x00, 0x01, streamID, 0x00, 0x00, 0x80, 0x80, 5,
		enc[0], enc[1], enc[2], enc[3], enc[4]}
	return append(out, body...)
}

func TestPTSRoundTrip(t *testing.T) {
	for _, v := range []int64{0, 1, 90000, 1 << 32, (1 << 33) - 1} {
		enc := EncodePTS(v)
		got := decodePTS(enc[:])
		if got != v&((1<<33)-1) {
			t.Errorf("pts %d round-trip %d", v, got)
		}
	}
}

func TestParseTSVideoAndAudio(t *testing.T) {
	video := buildTS(0x100, true, buildPES(0xE0, 90000, []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88}))
	audio := buildTS(0x101, true, buildPES(0xC0, 90090, nil))
	pad := buildTS(0x100, false, make([]byte, 100))
	samples, err := ParseTS([][]byte{video, pad, audio})
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 {
		t.Fatalf("want 2 samples, got %d", len(samples))
	}
	if samples[0].Raw != 90000 || !samples[0].Sync {
		t.Fatalf("video sample: %+v", samples[0])
	}
	if samples[1].Raw != 90090 || samples[1].Scale != TSScale {
		t.Fatalf("audio sample: %+v", samples[1])
	}
}

func TestCaptureRoundTripAndSplit(t *testing.T) {
	ts := buildTS(0x100, true, buildPES(0xE0, 100, nil))
	pkts := []Packet{
		{Seq: 0, Payload: ts},
		{Seq: 1, Payload: ts},
		{Seq: 8, Parity: true, Base: 0, Payload: BuildParityFixture(ts)},
	}
	data := EncodePackets(pkts)
	got, err := DecodePackets(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || !got[2].Parity || got[2].Base != 0 {
		t.Fatalf("decoded: %+v", got)
	}
	if _, err := DecodePackets(data[:len(data)-1]); err == nil {
		t.Fatal("truncated capture accepted")
	}
}

// BuildParityFixture is a local XOR helper mirroring recovery.BuildParity
// without importing that package from the segment tests.
func BuildParityFixture(p []byte) []byte {
	out := make([]byte, TSPacketLen)
	copy(out, p)
	return out
}

func TestParseMP4TfdtAndTruncation(t *testing.T) {
	data := buildMoof(1000)
	base, err := ParseTfdt(data)
	if err != nil || base != 1000 {
		t.Fatalf("tfdt: %d %v", base, err)
	}
	info, err := ParseMP4(append(buildMoov(90000), buildMoof(0)...))
	if err != nil {
		t.Fatal(err)
	}
	if info.Timescale != 90000 || info.Truncated {
		t.Fatalf("info: %+v", info)
	}
	if _, err := ParseMP4(data[:12]); err == nil {
		t.Fatal("truncated mp4 accepted")
	}
}

func TestParseMP4InvalidTimescale(t *testing.T) {
	_, err := ParseMP4(append(buildMoov(0), buildMoof(0)...))
	if err == nil {
		t.Fatal("zero timescale accepted")
	}
}

func boxBytes(typ string, body []byte) []byte {
	out := make([]byte, 8+len(body))
	binary.BigEndian.PutUint32(out[:4], uint32(8+len(body)))
	copy(out[4:8], typ)
	copy(out[8:], body)
	return out
}

func buildMoov(timescale uint32) []byte {
	trak := boxBytes("tkhd", tkhdBody(1))
	trak = append(trak, boxBytes("mdia", boxBytes("mdhd", mdhdBody(timescale)))...)
	return boxBytes("moov", boxBytes("trak", trak))
}

func mdhdBody(timescale uint32) []byte {
	b := make([]byte, 20)
	binary.BigEndian.PutUint32(b[12:16], timescale)
	return b
}

func tkhdBody(id uint32) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint32(b[12:16], id)
	return b
}

func buildMoof(tfdt uint64) []byte {
	tf := make([]byte, 12)
	tf[0] = 1 // version 1
	binary.BigEndian.PutUint64(tf[4:12], tfdt)
	tfhd := make([]byte, 8)
	binary.BigEndian.PutUint32(tfhd[:4], 0x000001)
	binary.BigEndian.PutUint32(tfhd[4:8], 1)
	traf := boxBytes("tfhd", tfhd)
	traf = append(traf, boxBytes("tfdt", tf)...)
	return boxBytes("moof", boxBytes("traf", traf))
}
