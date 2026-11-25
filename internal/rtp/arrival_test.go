package rtp

import (
	"bytes"
	"testing"

	"github.com/pion/rtp"

	"mediapipe/internal/media"
)

func packet(ssrc uint32, seq uint16, ts uint32) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    96,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           ssrc,
			Marker:         seq%10 == 0,
		},
		Payload: []byte{byte(seq), byte(seq >> 8), 0xAA, 0xBB},
	}
}

func buildCapture(t *testing.T, pkts []*rtp.Packet, arrivals []media.Timestamp) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteCapture(&buf, nil); err != nil {
		t.Fatal(err)
	}
	for i, p := range pkts {
		rec, err := BuildRecord(arrivals[i], 90000, p)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(rec)
	}
	return buf.Bytes()
}

func TestParseCapture(t *testing.T) {
	pkts := []*rtp.Packet{packet(1, 100, 9000), packet(1, 101, 12000), packet(1, 102, 15000)}
	arrivals := []media.Timestamp{media.Second, 1_033 * media.Millisecond, 1_066 * media.Millisecond}
	data := buildCapture(t, pkts, arrivals)
	res, err := Parse(bytes.NewReader(data), Limits{})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Arrivals) != 3 {
		t.Fatalf("arrivals=%d", len(res.Arrivals))
	}
	if res.Streams[1] != 3 {
		t.Fatalf("streams=%v", res.Streams)
	}
	if res.Discontinuity != 0 {
		t.Fatalf("unexpected discontinuity: %+v", res.Arrivals)
	}
	if got := res.Arrivals[2].ExtrapolatedTimestamp(); got != 15000*media.Second/90000 {
		t.Fatalf("extrapolated=%d", got)
	}
	if res.Arrivals[0].Offset != int64(len(Magic)) {
		t.Fatalf("offset=%d", res.Arrivals[0].Offset)
	}
}

func TestParseDetectsGap(t *testing.T) {
	pkts := []*rtp.Packet{packet(1, 10, 0), packet(1, 11, 1000), packet(1, 15, 5000)}
	arrivals := []media.Timestamp{0, media.Millisecond, 5 * media.Millisecond}
	data := buildCapture(t, pkts, arrivals)
	res, err := Parse(bytes.NewReader(data), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Discontinuity != 1 {
		t.Fatalf("discontinuity=%d", res.Discontinuity)
	}
	if res.Gaps != 1 || res.Lost != 3 {
		t.Fatalf("gaps=%d lost=%d", res.Gaps, res.Lost)
	}
}

func TestParseTruncatedPayloadReportsOffset(t *testing.T) {
	data := buildCapture(t, []*rtp.Packet{packet(1, 1, 0)}, []media.Timestamp{0})
	trunc := data[:len(data)-2]
	_, err := Parse(bytes.NewReader(trunc), Limits{})
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
	if off, ok := media.OffsetOf(err); !ok || off != int64(len(Magic)) {
		t.Fatalf("offset=%d ok=%v", off, ok)
	}
}

func TestParseBadMagic(t *testing.T) {
	_, err := Parse(bytes.NewReader([]byte("NOPE")), Limits{})
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
}

func TestParseRejectsOversizedPacket(t *testing.T) {
	data := buildCapture(t, []*rtp.Packet{packet(1, 1, 0)}, []media.Timestamp{0})
	_, err := Parse(bytes.NewReader(data), Limits{MaxPacketSize: 8})
	if err == nil || media.ClassOf(err) != media.ClassFatal {
		t.Fatalf("err=%v", err)
	}
}

func TestParseEmptyCapture(t *testing.T) {
	var buf bytes.Buffer
	WriteCapture(&buf, nil)
	res, err := Parse(&buf, Limits{})
	if err != nil || len(res.Arrivals) != 0 {
		t.Fatalf("err=%v arrivals=%d", err, len(res.Arrivals))
	}
}

func TestSequenceWrapNotADiscontinuity(t *testing.T) {
	pkts := []*rtp.Packet{packet(1, 65534, 0), packet(1, 65535, 1000), packet(1, 0, 2000)}
	arrivals := []media.Timestamp{0, media.Millisecond, 2 * media.Millisecond}
	data := buildCapture(t, pkts, arrivals)
	res, err := Parse(bytes.NewReader(data), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Discontinuity != 0 {
		t.Fatalf("wrap counted as discontinuity: %+v", res.Arrivals)
	}
}
