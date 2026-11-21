package ts

import (
	"bytes"
	"io"
	"testing"
)

// buildPacket makes a syntactically valid packet with the given header
// fields; payload is padded with 0xFF stuffing via the adaptation field.
func buildPacket(pid uint16, pusi bool, cc uint8, payload []byte) [PacketSize]byte {
	var p [PacketSize]byte
	p[0] = SyncByte
	p[1] = byte(pid>>8) & 0x1F
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	if len(payload) > 184 {
		panic("payload too large")
	}
	stuff := 184 - len(payload)
	if stuff == 0 {
		p[3] = 0x10 | cc // payload only
		copy(p[4:], payload)
		return p
	}
	p[3] = 0x30 | cc // adaptation + payload
	afl := stuff - 1
	p[4] = byte(afl)
	if afl >= 1 {
		p[5] = 0x00 // flags
		for i := 6; i < 5+afl; i++ {
			p[i] = 0xFF
		}
	}
	copy(p[5+afl:], payload)
	return p
}

func buildSection(tableID, version uint8, body []byte) []byte {
	// header(3) + ext(5) + body + CRC(4)
	secLen := 5 + len(body) + 4
	sec := make([]byte, 3+secLen)
	sec[0] = tableID
	sec[1] = 0xB0 | byte(secLen>>8)
	sec[2] = byte(secLen)
	sec[3] = 0x00
	sec[4] = 0x01
	sec[5] = 0xC1 | version<<1 // current_next=1
	sec[6] = 0x00
	sec[7] = 0x00
	copy(sec[8:], body)
	crc := CRC32MPEG(sec[:len(sec)-4])
	sec[len(sec)-4] = byte(crc >> 24)
	sec[len(sec)-3] = byte(crc >> 16)
	sec[len(sec)-2] = byte(crc >> 8)
	sec[len(sec)-1] = byte(crc)
	return sec
}

func TestParseHeaderAndPCR(t *testing.T) {
	var p [PacketSize]byte
	p[0] = SyncByte
	p[1] = 0x40 | 0x01 // PUSI, PID 0x101
	p[2] = 0x01
	p[3] = 0x20 // adaptation only
	p[4] = 7
	p[5] = 0x50 // random_access + PCR_flag
	pcr := uint64(123456789)
	p[6] = byte(pcr >> 25)
	p[7] = byte(pcr >> 17)
	p[8] = byte(pcr >> 9)
	p[9] = byte(pcr >> 1)
	p[10] = byte(pcr<<7) | 0x7E
	p[11] = 0x00
	h, err := ParseHeader(&p)
	if err != nil {
		t.Fatal(err)
	}
	if !h.PUSI || h.PID != 0x101 || !h.HasPCR || h.PCR != pcr || !h.RandomAccess {
		t.Fatalf("bad header: %+v", h)
	}
	if h.HasPayload {
		t.Fatal("adaptation-only packet must have no payload")
	}
	p[0] = 0x00
	if _, err := ParseHeader(&p); err != ErrSync {
		t.Fatalf("want ErrSync, got %v", err)
	}
}

func TestReaderShortReadsAndTruncation(t *testing.T) {
	var stream bytes.Buffer
	for i := 0; i < 3; i++ {
		pkt := buildPacket(0x100+uint16(i), false, uint8(i), []byte{1, 2, 3})
		stream.Write(pkt[:])
	}
	stream.Write(make([]byte, 100)) // dangling partial packet
	r, err := NewReader(&stream, PacketSize)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReader(&stream, 204); err != ErrBadPacketSize {
		t.Fatalf("want ErrBadPacketSize, got %v", err)
	}
	var pkt [PacketSize]byte
	for i := 0; i < 3; i++ {
		idx, err := r.Next(&pkt)
		if err != nil || idx != uint64(i) || pkt[2] != byte(0x100+i) {
			t.Fatalf("packet %d: idx=%d err=%v", i, idx, err)
		}
	}
	if _, err := r.Next(&pkt); err == nil {
		t.Fatal("expected truncation error")
	} else {
		te, ok := err.(*TruncationError)
		if !ok || te.Partial != 100 || te.Offset != 3*PacketSize {
			t.Fatalf("bad truncation: %#v", err)
		}
	}
}

func TestReaderCleanEOF(t *testing.T) {
	pkt := buildPacket(0x20, false, 0, nil)
	r, _ := NewReader(bytes.NewReader(pkt[:]), PacketSize)
	var out [PacketSize]byte
	if _, err := r.Next(&out); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(&out); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestCCTracker(t *testing.T) {
	tt := NewCCTracker()
	if v, _ := tt.Check(1, 5, true); v != CCFirst {
		t.Fatal("first packet must be CCFirst")
	}
	if v, _ := tt.Check(1, 6, true); v != CCOK {
		t.Fatal("in-order must be CCOK")
	}
	if v, _ := tt.Check(1, 6, true); v != CCDuplicate {
		t.Fatal("repeated counter must be CCDuplicate")
	}
	if v, g := tt.Check(1, 9, true); v != CCGap || g != 2 {
		t.Fatalf("want gap of 2, got %v gap=%d", v, g)
	}
	if v, _ := tt.Check(1, 9, false); v != CCOK {
		t.Fatal("payload-less packets are excluded")
	}
}

func TestPSIAssemblyAcrossPackets(t *testing.T) {
	sec := buildSection(0x00, 0, []byte{0x00, 0x01, 0xF0, 0x00})
	// Split the section across two packets behind a pointer_field.
	var a Assembler
	first := append([]byte{0x00}, sec[:5]...) // pointer_field=0 then 5 bytes
	got := a.Push(first, true, 10)
	if len(got) != 0 {
		t.Fatal("section must not complete yet")
	}
	got = a.Push(sec[5:], false, 11)
	if len(got) != 1 || got[0].EndPacket != 11 || got[0].Version != 0 {
		t.Fatalf("bad assembly: %+v", got)
	}
	if !ValidCRC(got[0].Raw) {
		t.Fatal("CRC must validate")
	}
}

func TestTableTrackerVersions(t *testing.T) {
	var tt TableTracker
	mk := func(ver uint8, corrupt bool) Section {
		s := buildSection(0x00, ver, []byte{0x00, 0x01, 0xF0, 0x00})
		if corrupt {
			s[len(s)-1] ^= 0xFF
		}
		sec, _ := parseSection(s, 0)
		return sec
	}
	if !tt.Update(mk(0, false), 0, 0) || len(tt.Updates) != 0 {
		t.Fatal("initial table must be accepted as baseline, not an update")
	}
	if !tt.Update(mk(1, false), 0, 0) || len(tt.Updates) != 1 {
		t.Fatal("forward version update must be accepted")
	}
	if tt.Update(mk(0, false), 0, 0) { // rollback
		t.Fatal("version rollback must be rejected")
	}
	if tt.Update(mk(1, true), 0, 0) { // CRC error
		t.Fatal("CRC failure must be rejected")
	}
	if tt.version != 1 {
		t.Fatalf("last valid table must survive, version=%d", tt.version)
	}
	if len(tt.Faults) != 2 {
		t.Fatalf("want 2 faults, got %d", len(tt.Faults))
	}
}

func TestParsePATandPMT(t *testing.T) {
	pat, _ := parseSection(buildSection(0x00, 0, []byte{0x00, 0x01, 0xF0, 0x00}), 0)
	if m := ParsePAT(pat); m[1] != 0x1000 {
		t.Fatalf("PAT map=%v", m)
	}
	// PMT: pcr_pid=0x101, one stream 0x1B on 0x101.
	body := []byte{0xE1, 0x01, 0xF0, 0x00, 0x1B, 0xE1, 0x01, 0xF0, 0x00}
	pmt, _ := parseSection(buildSection(0x02, 0, body), 0)
	pcrPID, entries := ParsePMT(pmt)
	if pcrPID != 0x101 || len(entries) != 1 || entries[0].PID != 0x101 || entries[0].StreamType != 0x1B {
		t.Fatalf("PMT pcr=0x%x entries=%+v", pcrPID, entries)
	}
}
