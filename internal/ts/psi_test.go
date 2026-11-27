package ts

import (
	"testing"

	"tscheck/internal/model"
)

// mkSection appends a valid CRC to a section body (table_id .. last byte
// before CRC) and returns the full section.
func mkSection(body []byte) []byte {
	sec := append([]byte(nil), body...)
	crc := CRC32MPEG(sec)
	return append(sec, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// mkPAT builds a PAT section mapping program 1 to the given PMT PID.
func mkPAT(version int, pmtPID uint16) []byte {
	body := []byte{
		0x00, 0xB0, 0x0D, // table_id, section_length 13
		0x00, 0x01, // transport_stream_id
		0xC1 | byte(version<<1), // current_next + version
		0x00, 0x00, // section numbers
		0x00, 0x01, // program 1
		0xE0 | byte(pmtPID>>8), byte(pmtPID),
	}
	return mkSection(body)
}

// mkPMT builds a PMT section with one H.264 video stream.
func mkPMT(version int, videoPID uint16) []byte {
	body := []byte{
		0x02, 0xB0, 0x12, // table_id, section_length 18
		0x00, 0x01, // program_number
		0xC1 | byte(version<<1),
		0x00, 0x00,
		0xE0 | byte(videoPID>>8), byte(videoPID), // PCR PID
		0xF0, 0x00, // program_info_length 0
		0x1B, // stream_type H.264
		0xE0 | byte(videoPID>>8), byte(videoPID),
		0xF0, 0x00, // ES_info_length 0
	}
	return mkSection(body)
}

// feedSection delivers a whole section as one PUSI packet payload.
func feedSection(tr *PSITracker, pid uint16, sec []byte) []model.Fault {
	payload := append([]byte{0x00}, sec...) // pointer_field 0
	return tr.Feed(PacketInfo{PID: pid, PUSI: true, Payload: payload})
}

func TestPSIAcceptsValidSequence(t *testing.T) {
	tr := NewPSITracker()
	if f := feedSection(tr, 0, mkPAT(0, 0x1000)); len(f) != 0 {
		t.Fatalf("PAT v0 faults: %v", f)
	}
	if f := feedSection(tr, 0x1000, mkPMT(0, 0x100)); len(f) != 0 {
		t.Fatalf("PMT v0 faults: %v", f)
	}
	st := tr.Streams()
	if len(st) != 1 || st[0].PID != 0x100 || st[0].StreamType != 0x1B {
		t.Fatalf("streams = %+v", st)
	}
	// Legal update: version 0 -> 1.
	if f := feedSection(tr, 0, mkPAT(1, 0x1000)); len(f) != 0 {
		t.Fatalf("PAT v1 faults: %v", f)
	}
	if f := feedSection(tr, 0x1000, mkPMT(1, 0x101)); len(f) != 0 {
		t.Fatalf("PMT v1 faults: %v", f)
	}
	if st := tr.Streams(); len(st) != 1 || st[0].PID != 0x101 {
		t.Fatalf("streams after update = %+v", st)
	}
}

func TestPSIRejectsBadCRCAndKeepsMapping(t *testing.T) {
	tr := NewPSITracker()
	feedSection(tr, 0, mkPAT(0, 0x1000))
	feedSection(tr, 0x1000, mkPMT(0, 0x100))
	bad := mkPMT(1, 0x999)
	bad[len(bad)-1] ^= 0xFF // corrupt CRC
	f := feedSection(tr, 0x1000, bad)
	if len(f) != 1 || f[0].Kind != model.FaultPSICRC {
		t.Fatalf("faults = %+v", f)
	}
	if st := tr.Streams(); len(st) != 1 || st[0].PID != 0x100 {
		t.Fatalf("trusted mapping lost: %+v", st)
	}
}

func TestPSIRejectsIllegalVersionJump(t *testing.T) {
	tr := NewPSITracker()
	feedSection(tr, 0, mkPAT(0, 0x1000))
	f := feedSection(tr, 0, mkPAT(2, 0x1000)) // 0 -> 2 is illegal
	if len(f) != 1 || f[0].Kind != model.FaultPSIVersion {
		t.Fatalf("faults = %+v", f)
	}
}

func TestPSISectionSpanningPackets(t *testing.T) {
	tr := NewPSITracker()
	feedSection(tr, 0, mkPAT(0, 0x1000))
	sec := append([]byte{0x00}, mkPMT(0, 0x100)...)
	half := len(sec) / 2
	if f := tr.Feed(PacketInfo{PID: 0x1000, PUSI: true, Payload: sec[:half]}); len(f) != 0 {
		t.Fatalf("first half faults: %v", f)
	}
	if st := tr.Streams(); len(st) != 0 {
		t.Fatalf("mapping applied before section complete: %+v", st)
	}
	if f := tr.Feed(PacketInfo{PID: 0x1000, Payload: sec[half:]}); len(f) != 0 {
		t.Fatalf("second half faults: %v", f)
	}
	if st := tr.Streams(); len(st) != 1 || st[0].PID != 0x100 {
		t.Fatalf("streams = %+v", st)
	}
}

func TestCRC32MPEGKnownVector(t *testing.T) {
	// CRC-32/MPEG-2 of "123456789" is 0x0376E6E7.
	if got := CRC32MPEG([]byte("123456789")); got != 0x0376E6E7 {
		t.Fatalf("crc = %#x", got)
	}
}
