package rtpnet

import (
	"testing"

	"mediacore/internal/clock"
	"mediacore/internal/sample"
)

func TestPacketizeSingleAndFragment(t *testing.T) {
	pz := NewPacketizer(TrackConfig{SSRC: 7, PayloadType: 96, ClockRate: clock.Rate}, DefaultMTU)
	small := &sample.Sample{TrackID: 1, PTS: 45000, Data: []byte("hello")}
	pkts, err := pz.Packetize(small)
	if err != nil || len(pkts) != 1 || !pkts[0].Marker {
		t.Fatalf("small sample: %v %d", err, len(pkts))
	}
	if pkts[0].Timestamp != 45000 || pkts[0].SSRC != 7 {
		t.Fatalf("header fields wrong: %+v", pkts[0].Header)
	}

	big := &sample.Sample{TrackID: 1, PTS: 0, Data: make([]byte, 4000)}
	frags, err := pz.Packetize(big)
	if err != nil || len(frags) < 2 {
		t.Fatalf("fragmentation failed: %v %d", err, len(frags))
	}
	for i, p := range frags {
		if p.Marker != (i == len(frags)-1) {
			t.Fatalf("marker only on last fragment at %d", i)
		}
		if i > 0 && p.SequenceNumber != frags[i-1].SequenceNumber+1 {
			t.Fatal("sequence numbers must be contiguous")
		}
	}
}

func TestDeterministicSequence(t *testing.T) {
	mk := func() []byte {
		pz := NewPacketizer(TrackConfig{SSRC: 1, PayloadType: 96, ClockRate: clock.Rate}, DefaultMTU)
		var raw []byte
		for i := 0; i < 5; i++ {
			pkts, _ := pz.Packetize(&sample.Sample{TrackID: 1, PTS: clock.Tick(i) * 3000,
				Data: []byte{byte(i)}})
			for _, p := range pkts {
				b, _ := Encode(p)
				raw = append(raw, b...)
			}
		}
		return raw
	}
	a, b := mk(), mk()
	if string(a) != string(b) {
		t.Fatal("packetizer output must be deterministic across runs")
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	pz := NewPacketizer(TrackConfig{SSRC: 9, PayloadType: 98, ClockRate: 48000}, DefaultMTU)
	pkts, _ := pz.Packetize(&sample.Sample{TrackID: 2, PTS: 0, Data: []byte{1, 2, 3}})
	raw, err := Encode(pkts[0])
	if err != nil {
		t.Fatal(err)
	}
	dec, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Version != 2 || dec.PayloadType != 98 || len(dec.Payload) != 3 {
		t.Fatalf("round trip mismatch: %+v", dec.Header)
	}
}
