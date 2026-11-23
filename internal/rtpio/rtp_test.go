package rtpio

import (
	"testing"

	"github.com/pion/rtp"

	"medpipe/internal/model"
)

func trackAndFrames(n int) (model.Track, []model.Frame) {
	tr := model.Track{ID: "remote1", Kind: model.KindVideo, Timescale: 90000, ClockBits: 32, NominalTickDelta: 3000}
	frames := make([]model.Frame, n)
	for i := 0; i < n; i++ {
		frames[i] = model.Frame{
			TrackID: tr.ID, Seq: uint64(i), RawTS: uint64(5000 + i*3000),
			Payload: model.EncodeVideoPayload(320, 180, uint32(i), make([]byte, 200+i*13)),
		}
	}
	return tr, frames
}

func TestRTPRoundTripMultiFragment(t *testing.T) {
	tr, frames := trackAndFrames(6)
	packets, err := RoundTrip([]model.Track{tr}, map[string][]model.Frame{tr.ID: frames})
	if err != nil {
		t.Fatal(err)
	}
	if packets < len(frames)*2 {
		t.Fatalf("expected multi-fragment frames, got %d packets for %d frames", packets, len(frames))
	}
}

func TestReassembleDetectsSequenceGap(t *testing.T) {
	tr, frames := trackAndFrames(3)
	var all []*rtp.Packet
	for i := range frames {
		pkts, err := Packetize(tr.ID, &frames[i])
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range pkts {
			p.SequenceNumber = uint16(len(all))
			all = append(all, p)
		}
	}
	// Drop a middle fragment of the first (multi-fragment) frame, leaving a
	// sequence-number hole inside that frame.
	all = append(all[:1], all[2:]...)
	_, err := Reassemble(tr.ID, all)
	if err == nil || !model.IsKind(err, model.KindCorrupt) {
		t.Fatalf("got %v, want corruption", err)
	}
}

func TestReassembleDetectsMarkerAndSSRC(t *testing.T) {
	tr, frames := trackAndFrames(1)
	pkts, err := Packetize(tr.ID, &frames[0])
	if err != nil {
		t.Fatal(err)
	}
	pkts[len(pkts)-1].Marker = false
	if _, err := Reassemble(tr.ID, pkts); err == nil {
		t.Fatal("missing marker accepted")
	}
	pkts2, _ := Packetize(tr.ID, &frames[0])
	pkts2[0].SSRC = 1234
	if _, err := Reassemble(tr.ID, pkts2); err == nil {
		t.Fatal("foreign SSRC accepted")
	}
}

func TestEmptyPayloadRejected(t *testing.T) {
	fr := model.Frame{TrackID: "x", Seq: 0}
	if _, err := Packetize("x", &fr); err == nil {
		t.Fatal("empty payload packetized")
	}
}
