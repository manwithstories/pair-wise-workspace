package tspacket

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/asticode/go-astits"
)

func testPacket(pid uint16, cc uint8, pusi, tei bool, payload []byte) []byte {
	packet := make([]byte, PacketSize)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = SyncByte
	packet[1] = byte(pid>>8) & 0x1f
	if pusi {
		packet[1] |= 0x40
	}
	if tei {
		packet[1] |= 0x80
	}
	packet[2] = byte(pid)
	packet[3] = 0x10 | cc&0x0f
	copy(packet[4:], payload)
	return packet
}

func adaptationPacket(pid uint16, cc uint8, discontinuity bool, payload []byte) []byte {
	packet := make([]byte, PacketSize)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = SyncByte
	packet[1] = byte(pid >> 8)
	packet[2] = byte(pid)
	packet[3] = 0x30 | cc&0x0f
	packet[4] = 1
	if discontinuity {
		packet[5] = 0x80
	}
	copy(packet[6:], payload)
	return packet
}

func adaptationOnlyPacket(pid uint16, tei bool) []byte {
	packet := make([]byte, PacketSize)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = SyncByte
	packet[1] = byte(pid >> 8)
	if tei {
		packet[1] |= 0x80
	}
	packet[2] = byte(pid)
	packet[3] = 0x20
	packet[4] = 183
	return packet
}

func TestReaderResyncsPastGarbage(t *testing.T) {
	first := testPacket(0x100, 0, true, false, []byte("abc"))
	second := testPacket(0x100, 1, true, false, []byte("def"))
	third := testPacket(0x101, 0, false, false, []byte("ghi"))
	input := append(append(append(first, []byte{0xde, 0xad, 0xbe}...), second...), third...)
	input = append(input, 0x99)
	r := NewReader(bytes.NewReader(input), 8)
	var pids []uint16
	for {
		p, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, p.PID)
	}
	if len(pids) != 3 {
		t.Fatalf("packets=%d, want 3", len(pids))
	}
	if pids[0] != 0x100 || pids[1] != 0x100 || pids[2] != 0x101 {
		t.Fatalf("unexpected PIDs: %v", pids)
	}
	stats := r.Stats()
	if stats.GarbageBytes < 3 || stats.SyncResyncs == 0 {
		t.Fatalf("garbage/resync stats not recorded: %+v", stats)
	}
}

func TestContinuityCounters(t *testing.T) {
	data := bytes.Join([][]byte{
		testPacket(0x100, 0, false, false, []byte{1}),
		testPacket(0x100, 2, false, false, []byte{2}),
		testPacket(0x100, 2, false, false, []byte{3}),
		adaptationPacket(0x100, 9, true, []byte{4}),
		testPacket(0x100, 10, false, false, []byte{5}),
		testPacket(0x100, 1, false, false, []byte{6}),
		testPacket(0x100, 2, false, false, []byte{7}),
	}, nil)
	r := NewReader(bytes.NewReader(data), 4)
	var duplicates int
	for {
		p, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.Duplicate {
			duplicates++
		}
	}
	stats := r.Stats()
	if stats.LostPackets != 1 {
		t.Fatalf("lost=%d, want 1", stats.LostPackets)
	}
	if stats.DuplicatePackets != 1 || duplicates != 1 {
		t.Fatalf("duplicate stats=%d packets=%d, want 1", stats.DuplicatePackets, duplicates)
	}
	if stats.Discontinuities != 1 || stats.CCJumps != 1 {
		t.Fatalf("discontinuities=%d jumps=%d, want 1/1", stats.Discontinuities, stats.CCJumps)
	}
}

func TestSpecialPacketStats(t *testing.T) {
	data := bytes.Join([][]byte{
		adaptationOnlyPacket(0x200, true),
		testPacket(NullPID, 0, false, false, []byte{0xff}),
		testPacket(0x201, 0, true, true, []byte{1}),
	}, nil)
	r := NewReader(bytes.NewReader(data), 8)
	for {
		_, err := r.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	stats := r.Stats()
	if stats.TEIPackets != 2 || stats.AdaptationOnly != 1 || stats.NullPackets != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestAdaptationPCRAndRandomAccess(t *testing.T) {
	packet := make([]byte, PacketSize)
	for i := range packet {
		packet[i] = 0xff
	}
	packet[0] = SyncByte
	packet[1] = 0x01
	packet[2] = 0x00
	packet[3] = 0x30
	packet[4] = 7
	packet[5] = 0x50
	base := uint64(123456)
	packet[6] = byte(base >> 25)
	packet[7] = byte(base >> 17)
	packet[8] = byte(base >> 9)
	packet[9] = byte(base >> 1)
	packet[10] = byte(base&1)<<7 | 0x7e
	packet[11] = 0
	r := NewReader(bytes.NewReader(packet), 8)
	p, err := r.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasPCR || !p.RandomAccess || p.PCRBase != base {
		t.Fatalf("adaptation parse mismatch: %+v", p)
	}
}

func TestReaderErrorsAndAstitsHeaderCrossCheck(t *testing.T) {
	if _, err := NewReader(bytes.NewReader(nil), 8).ReadPacket(); !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("empty input error=%v", err)
	}
	if _, err := NewReader(bytes.NewReader(bytes.Repeat([]byte{1}, 400)), 8).ReadPacket(); !errors.Is(err, ErrNoSync) {
		t.Fatalf("no sync error=%v", err)
	}

	first := testPacket(0x123, 7, true, false, []byte("payload"))
	second := testPacket(0x123, 8, false, false, []byte("next"))
	r := NewReader(bytes.NewReader(append(first, second...)), 8)
	p, err := r.ReadPacket()
	if err != nil {
		t.Fatal(err)
	}
	if p.PID != 0x123 || p.CC != 7 || !p.PUSI || len(p.Payload) != 184 {
		t.Fatalf("decoded packet mismatch: %+v", p)
	}

	dmx := astits.NewDemuxer(context.Background(), bytes.NewReader(append(first, second...)))
	astPacket, err := dmx.NextPacket()
	if err != nil {
		t.Fatal(err)
	}
	if astPacket.Header.PID != p.PID || astPacket.Header.ContinuityCounter != p.CC || astPacket.Header.PayloadUnitStartIndicator != p.PUSI {
		t.Fatalf("astits cross-check mismatch: ast=%+v ours=%+v", astPacket.Header, p)
	}
}
