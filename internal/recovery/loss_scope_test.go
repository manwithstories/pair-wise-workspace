package recovery

import (
	"testing"

	"avrepair/internal/segment"
)

// TestUnrecoverableGroupDoesNotRepairNeighbours verifies that a group with
// two losses yields an explicit loss record and no invented packet.
func TestUnrecoverableGroupDoesNotRepairNeighbours(t *testing.T) {
	g := group8()
	par := BuildParity(0, g)
	var pkts []segment.Packet
	for i := 0; i < GroupSize; i++ {
		if i == 2 || i == 7 {
			continue
		}
		pkts = append(pkts, segment.Packet{Seq: uint16(i), Payload: g[i]})
	}
	pkts = append(pkts, segment.Packet{Seq: 8, Parity: true, Base: 0, Payload: par})
	res := Recover(pkts)
	if res.Recovered != 0 {
		t.Fatalf("recovered=%d, want 0", res.Recovered)
	}
	if len(res.Unrecoverable) != 1 || res.Unrecoverable[0].Base != 0 {
		t.Fatalf("loss record: %+v", res.Unrecoverable)
	}
	for _, p := range res.Packets {
		if p.Seq == 2 || p.Seq == 7 {
			t.Fatalf("invented packet %d", p.Seq)
		}
	}
}

// TestParityWrongSizeIsRejected guards against truncated parity records.
func TestParityWrongSizeIsRejected(t *testing.T) {
	g := group8()
	pkts := []segment.Packet{{Seq: 8, Parity: true, Base: 0, Payload: []byte{1, 2, 3}}}
	for i := 1; i < GroupSize; i++ {
		pkts = append(pkts, segment.Packet{Seq: uint16(i), Payload: g[i]})
	}
	res := Recover(pkts)
	if res.Recovered != 0 || len(res.Unrecoverable) != 1 {
		t.Fatalf("bad parity accepted: %+v", res)
	}
}

// TestEmptyCaptureIsReported covers a capture with no data records.
func TestEmptyCaptureIsReported(t *testing.T) {
	res := Recover(nil)
	if len(res.Packets) != 0 || len(res.Warnings) == 0 {
		t.Fatalf("empty capture: %+v", res)
	}
}
