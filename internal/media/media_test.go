package media

import (
	"bytes"
	"testing"

	"github.com/pion/rtp"

	"mediacore/internal/clock"
)

func TestSegmentRoundTrip(t *testing.T) {
	samples := []Sample{
		{Duration: 3600, Size: 100, Keyframe: true},
		{Duration: 3600, Size: 50, Keyframe: false},
		{Duration: 3600, Size: 70, Keyframe: false},
	}
	payload := bytes.Repeat([]byte{0xAB}, 220)
	var buf bytes.Buffer
	if err := WriteSegment(&buf, 180000, samples, payload); err != nil {
		t.Fatal(err)
	}
	seg, err := ReadSegment(bytes.NewReader(buf.Bytes()), "v1", 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if seg.DecodeTime != 180000 {
		t.Fatalf("decode time = %d", seg.DecodeTime)
	}
	if len(seg.Samples) != 3 || !seg.Samples[0].Keyframe || seg.Samples[1].Keyframe {
		t.Fatalf("bad sample index: %+v", seg.Samples)
	}
	if !bytes.Equal(seg.Payload, payload) {
		t.Fatal("payload mismatch")
	}
}

func TestReadSegmentTruncated(t *testing.T) {
	samples := []Sample{{Duration: 3600, Size: 100, Keyframe: true}}
	var buf bytes.Buffer
	if err := WriteSegment(&buf, 0, samples, bytes.Repeat([]byte{1}, 100)); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()[:buf.Len()-10] // cut the mdat short
	_, err := ReadSegment(bytes.NewReader(raw), "v1", 3, false)
	if clock.CodeOf(err) != clock.CodeTruncatedSegment {
		t.Fatalf("want TRUNCATED_SEGMENT, got %v", err)
	}
	if e := err.(*clock.Error); e.Segment != 3 || e.Track != "v1" {
		t.Fatalf("error should carry track/segment position: %v", e)
	}
}

func TestRTPUnwrapOverflow(t *testing.T) {
	// Timestamps straddling the 32-bit wrap point.
	stamps := []uint32{0xFFFFFFF8, 0xFFFFFFFC, 0x00000002, 0x00000006}
	var packets []*rtp.Packet
	for i, ts := range stamps {
		p := &rtp.Packet{
			Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: ts, SSRC: 42},
			Payload: []byte{byte(i)},
		}
		raw, err := p.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		back := &rtp.Packet{}
		if err := back.Unmarshal(raw); err != nil {
			t.Fatal(err)
		}
		packets = append(packets, back)
	}
	ticks, err := TicksFromRTP(packets, 90000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(ticks); i++ {
		if ticks[i].Cmp(ticks[i-1]) <= 0 {
			t.Fatalf("ticks not monotonic after unwrap: %v", ticks)
		}
	}
	// (2^32 + 6) / 90000 s must be representable exactly.
	last := ticks[len(ticks)-1]
	want, _ := clock.New(1<<32+6, 90000)
	if last != want {
		t.Fatalf("last tick = %v, want %v", last, want)
	}
}

func TestTicksFromRTPBadTimescale(t *testing.T) {
	_, err := TicksFromRTP([]*rtp.Packet{{}}, 0)
	if clock.CodeOf(err) != clock.CodeTimescaleInvalid {
		t.Fatalf("want TIMESCALE_INVALID, got %v", err)
	}
}
