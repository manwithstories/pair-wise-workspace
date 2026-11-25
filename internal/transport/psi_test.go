package transport

import (
	"errors"
	"testing"
)

func TestMPEGCRCVector(t *testing.T) {
	if got := CRC32MPEG([]byte("123456789")); got != 0x0376e6e7 {
		t.Fatalf("CRC=%08x, want 0376e6e7", got)
	}
}

func TestPSIRegistryRejectsBadCRCAndSwitchesConsistentVersion(t *testing.T) {
	reg := NewPSIRegistry()

	patPayload := BuildPSIPayload(0x00, 1, 1, BuildPATEntries(1, 0x1000))
	patPkt, err := BuildPacket(PacketOptions{PID: 0, PUSI: true, CC: 0, Payload: patPayload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.HandlePacket(mustParse(t, patPkt)); !errors.Is(err, ErrPSIWaitingConfirm) {
		t.Fatalf("first PAT err=%v, want confirmation", err)
	}
	if _, err := reg.HandlePacket(mustParse(t, patPkt)); err != nil {
		t.Fatalf("second PAT: %v", err)
	}
	if reg.PAT == nil || !reg.KnownPMTPIDs[0x1000] {
		t.Fatalf("PAT not applied: %+v", reg)
	}

	pmt1 := BuildPSIPayload(0x02, 1, 1, BuildPMTEntries(0x101, []PMTStream{
		{StreamType: StreamTypeH264, PID: 0x101},
	}))
	pmtPkt1, _ := BuildPacket(PacketOptions{PID: 0x1000, PUSI: true, CC: 0, Payload: pmt1})
	if _, err := reg.HandlePacket(mustParse(t, pmtPkt1)); !errors.Is(err, ErrPSIWaitingConfirm) {
		t.Fatalf("first PMT v1 err=%v", err)
	}
	if _, err := reg.HandlePacket(mustParse(t, pmtPkt1)); err != nil {
		t.Fatal(err)
	}
	if v := reg.PMTVersion(0x1000); v != 1 {
		t.Fatalf("PMT version=%d, want 1", v)
	}

	bad := append([]byte(nil), BuildPSIPayload(0x02, 1, 2, BuildPMTEntries(0x101, []PMTStream{
		{StreamType: StreamTypeH264, PID: 0x101},
		{StreamType: StreamTypePCMS16LE, PID: 0x102},
	}))...)
	bad[len(bad)-1] ^= 0xff
	badPkt, _ := BuildPacket(PacketOptions{PID: 0x1000, PUSI: true, CC: 2, Payload: bad})
	if _, err := reg.HandlePacket(mustParse(t, badPkt)); !errors.Is(err, ErrBadCRC) {
		t.Fatalf("bad PMT err=%v, want ErrBadCRC", err)
	}
	if reg.BadCRC != 1 || reg.PMTVersion(0x1000) != 1 {
		t.Fatalf("bad CRC did not retain v1: count=%d version=%d", reg.BadCRC, reg.PMTVersion(0x1000))
	}

	good := append([]byte(nil), bad...)
	// Rebuild a valid v2 because bad's CRC is intentionally corrupt.
	good = BuildPSIPayload(0x02, 1, 2, BuildPMTEntries(0x101, []PMTStream{
		{StreamType: StreamTypeH264, PID: 0x101},
		{StreamType: StreamTypePCMS16LE, PID: 0x102},
	}))
	goodPkt, _ := BuildPacket(PacketOptions{PID: 0x1000, PUSI: true, CC: 3, Payload: good})
	if _, err := reg.HandlePacket(mustParse(t, goodPkt)); !errors.Is(err, ErrPSIWaitingConfirm) {
		t.Fatalf("first v2 err=%v", err)
	}
	goodPkt2, _ := BuildPacket(PacketOptions{PID: 0x1000, PUSI: true, CC: 4, Payload: good})
	if _, err := reg.HandlePacket(mustParse(t, goodPkt2)); err != nil {
		t.Fatalf("consistent v2 should apply: %v", err)
	}
	if v := reg.PMTVersion(0x1000); v != 2 {
		t.Fatalf("PMT version=%d, want 2", v)
	}
	_, streams, err := ParsePMTEntries(reg.PMTs[0x1000])
	if err != nil || len(streams) != 2 || streams[1].Codec != "pcm_s16le" {
		t.Fatalf("streams=%+v err=%v", streams, err)
	}
}

func mustParse(t *testing.T, raw []byte) Packet {
	t.Helper()
	p, err := ParsePacket(raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
