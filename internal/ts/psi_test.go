package ts

import (
	"errors"
	"testing"

	"streamcut/internal/media"
)

// buildSection assembles a syntactically valid PAT/PMT section and appends a
// correct CRC32-MPEG2, mirroring what the fixture generator emits.
func buildSection(tableID byte, payload []byte) []byte {
	body := []byte{tableID, 0xB0, 0x00} // section_syntax_indicator set
	secLen := len(payload) + 4
	body[1] |= byte(secLen>>8) & 0x0F
	body[2] = byte(secLen)
	body = append(body, payload...)
	crc := CRC32MPEG2(body) // FCS: hashing body||FCS then yields zero
	body = append(body, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	return body
}

func patPayload(version byte, pmtPID uint16) []byte {
	p := make([]byte, 5+4) // header bytes 3..7 + one program entry
	p[0], p[1] = 0x00, 0x01 // transport_stream_id
	p[2] = 0xC1 | (version << 1)
	p[3], p[4] = 0x00, 0x00 // section_number, last_section_number
	p[5], p[6] = 0x00, 0x01 // program_number 1
	p[7], p[8] = 0xE0|byte(pmtPID>>8), byte(pmtPID)
	return p
}

func pmtPayload(version byte, videoPID, audioPID uint16, audioType StreamType) []byte {
	p := make([]byte, 0, 5+5+5)
	p = append(p, 0x00, 0x01)                         // program number
	p = append(p, 0xC1|(version<<1), 0x00, 0x00)     // version + section nums
	p = append(p, 0xE1, 0x00)                        // PCR PID 0x100
	p = append(p, 0xF0, 0x00)                        // no program descriptors
	p = append(p, byte(STH264), 0xE0|byte(videoPID>>8), byte(videoPID), 0xF0, 0x00)
	p = append(p, byte(audioType), 0xE0|byte(audioPID>>8), byte(audioPID), 0xF0, 0x00)
	return p
}

func feedSection(t *testing.T, asm *SectionAssembler, trk *PSITracker, pid uint16, section []byte, corrupt bool) error {
	t.Helper()
	packets := sectionToPackets(pid, section)
	if corrupt {
		packets[0][10] ^= 0xFF // flip payload bits, CRC now invalid
	}
	var firstErr error
	for _, raw := range packets {
		p, err := Parse(raw)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		secs, err := asm.Push(p)
		if err != nil {
			t.Fatalf("section push: %v", err)
		}
		for _, s := range secs {
			if _, err := trk.Observe(pid, s); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// sectionToPackets wraps a PSI section in TS packets with a pointer field,
// matching the generator's framing.
func sectionToPackets(pid uint16, section []byte) [][]byte {
	var out [][]byte
	rest := section
	first := true
	for len(rest) > 0 || first {
		pkt := make([]byte, PacketSize)
		pkt[0] = 0x47
		pkt[1] = byte(pid>>8) | 0x10 // payload-only
		pkt[2] = byte(pid)
		off := 4
		if first {
			pkt[1] |= 0x40 // PUSI
			pkt[3] = 0x10
			pkt[4] = 0x00 // pointer field
			off = 5
		}
		n := copy(pkt[off:], rest)
		rest = rest[n:]
		for i := off + n; i < PacketSize; i++ {
			pkt[i] = 0xFF
		}
		out = append(out, pkt)
		first = false
	}
	return out
}

func TestCRCGateRejectsBadVersionAndKeepsPrevious(t *testing.T) {
	asm, trk := NewSectionAssembler(), NewPSITracker()
	const pmtPID uint16 = 0x1000
	if err := feedSection(t, asm, trk, PIDPAT,
		buildSection(0x00, patPayload(0, pmtPID)), false); err != nil {
		t.Fatal(err)
	}
	if trk.PMTPID() != pmtPID {
		t.Fatalf("PMT PID = %#x, want %#x", trk.PMTPID(), pmtPID)
	}
	// Good PMT v0 commits.
	if err := feedSection(t, asm, trk, pmtPID,
		buildSection(0x02, pmtPayload(0, 0x0100, 0x0101, STPCMPrivate)), false); err != nil {
		t.Fatal(err)
	}
	st := trk.Current()
	if st == nil || st.Version != 0 || st.VideoPID != 0x0100 ||
		st.AudioPID != 0x0101 || st.AudioKind != media.KindAudioPCM16LE {
		t.Fatalf("bad committed PMT: %+v", st)
	}
	// Corrupt PMT v1 must be rejected with ErrCRCReject and v0 retained.
	err := feedSection(t, asm, trk, pmtPID,
		buildSection(0x02, pmtPayload(1, 0x0200, 0x0201, STPCMPrivate)), true)
	if !errors.Is(err, media.ErrCRCReject) {
		t.Fatalf("want ErrCRCReject, got %v", err)
	}
	if trk.Rejects != 1 {
		t.Fatalf("rejects = %d, want 1", trk.Rejects)
	}
	if trk.Current().Version != 0 || trk.Current().VideoPID != 0x0100 {
		t.Fatalf("corrupt version must not commit: %+v", trk.Current())
	}
	// A later valid v1 commits normally.
	if err := feedSection(t, asm, trk, pmtPID,
		buildSection(0x02, pmtPayload(1, 0x0200, 0x0201, STPCMPrivate)), false); err != nil {
		t.Fatal(err)
	}
	if trk.Current().Version != 1 || trk.Current().VideoPID != 0x0200 {
		t.Fatalf("valid v1 should commit: %+v", trk.Current())
	}
}

func TestUnsupportedAudioIsMarkedNotSilenced(t *testing.T) {
	asm, trk := NewSectionAssembler(), NewPSITracker()
	const pmtPID uint16 = 0x1000
	_ = feedSection(t, asm, trk, PIDPAT,
		buildSection(0x00, patPayload(0, pmtPID)), false)
	_ = feedSection(t, asm, trk, pmtPID,
		buildSection(0x02, pmtPayload(0, 0x0100, 0x0102, STAACADTS)), false)
	st := trk.Current()
	if st.AudioKind != media.KindAudioUnsupported {
		t.Fatalf("AAC must be KindAudioUnsupported, got %v", st.AudioKind)
	}
}
