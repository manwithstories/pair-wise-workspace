package ts

import (
	"testing"

	"tscheck/internal/model"
)

// mkPacket builds a minimal 188-byte packet for pid with the given cc.
func mkPacket(pid uint16, cc uint8, pusi bool) []byte {
	b := make([]byte, PacketSize)
	b[0] = SyncByte
	b[1] = byte(pid >> 8)
	if pusi {
		b[1] |= 0x40
	}
	b[2] = byte(pid)
	b[3] = 0x10 | (cc & 0x0F) // payload only
	for i := 4; i < PacketSize; i++ {
		b[i] = byte(i)
	}
	return b
}

func concat(pkts ...[]byte) []byte {
	var out []byte
	for _, p := range pkts {
		out = append(out, p...)
	}
	return out
}

func TestScanClean(t *testing.T) {
	data := concat(mkPacket(0x100, 0, false), mkPacket(0x100, 1, false), mkPacket(0x100, 2, false))
	pkts, rep := NewScanner().Scan(data)
	if rep.Packets != 3 || len(pkts) != 3 {
		t.Fatalf("packets = %d", rep.Packets)
	}
	if rep.FirstFaultOffset != -1 || len(rep.Faults) != 0 {
		t.Fatalf("unexpected faults: %+v", rep.Faults)
	}
	if pkts[1].Offset != 188 || pkts[1].CC != 1 {
		t.Fatalf("bad packet info: %+v", pkts[1])
	}
}

func TestScanContinuityGapRecordedOnce(t *testing.T) {
	// cc jumps 1 -> 4 (gap), then continues 5, 6: exactly one fault.
	data := concat(mkPacket(0x100, 0, false), mkPacket(0x100, 1, false),
		mkPacket(0x100, 4, false), mkPacket(0x100, 5, false), mkPacket(0x100, 6, false))
	_, rep := NewScanner().Scan(data)
	if len(rep.Faults) != 1 || rep.Faults[0].Kind != model.FaultContinuityGap {
		t.Fatalf("faults = %+v", rep.Faults)
	}
	if rep.Faults[0].Offset != 2*188 {
		t.Fatalf("gap offset = %d", rep.Faults[0].Offset)
	}
}

func TestScanDuplicateNotAnError(t *testing.T) {
	data := concat(mkPacket(0x100, 0, false), mkPacket(0x100, 1, false),
		mkPacket(0x100, 1, false), mkPacket(0x100, 2, false))
	_, rep := NewScanner().Scan(data)
	if rep.Duplicates != 1 {
		t.Fatalf("duplicates = %d", rep.Duplicates)
	}
	if len(rep.Faults) != 0 {
		t.Fatalf("duplicate raised faults: %+v", rep.Faults)
	}
}

func TestScanSyncLossAndResync(t *testing.T) {
	good := concat(mkPacket(0x100, 0, false), mkPacket(0x100, 1, false))
	bad := []byte{0x00, 0x11, 0x22} // garbage between packets
	data := concat(good, bad, mkPacket(0x100, 2, false))
	_, rep := NewScanner().Scan(data)
	if rep.FirstFaultOffset != 2*188 {
		t.Fatalf("first fault offset = %d", rep.FirstFaultOffset)
	}
	if rep.Packets != 3 {
		t.Fatalf("resync failed, packets = %d", rep.Packets)
	}
}

func TestScanTruncatedTail(t *testing.T) {
	data := concat(mkPacket(0x100, 0, false), []byte{SyncByte, 0x00})
	_, rep := NewScanner().Scan(data)
	if len(rep.Faults) != 1 || rep.Faults[0].Kind != model.FaultTruncated {
		t.Fatalf("faults = %+v", rep.Faults)
	}
	if rep.Faults[0].Offset != 188 {
		t.Fatalf("truncated offset = %d", rep.Faults[0].Offset)
	}
}

func TestParsePacketRejectsBadInput(t *testing.T) {
	if _, err := ParsePacket(make([]byte, 100)); err == nil {
		t.Fatal("short packet accepted")
	}
	if _, err := ParsePacket(make([]byte, PacketSize)); err == nil {
		t.Fatal("missing sync byte accepted")
	}
}
