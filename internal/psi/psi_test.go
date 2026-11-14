package psi

import "testing"

func TestCRC32MPEGKnownVector(t *testing.T) {
	// MPEG-2 CRC32 check vector for "123456789".
	if got := CRC32MPEG([]byte("123456789")); got != 0x0376E6E7 {
		t.Fatalf("CRC32MPEG = 0x%08X, want 0x0376E6E7", got)
	}
}

func buildSection(t *testing.T, tableID byte, bodyLen int) []byte {
	t.Helper()
	sl := bodyLen + 4 // body + CRC
	sec := make([]byte, 3, 3+sl)
	sec[0] = tableID
	sec[1] = 0xB0 | byte(sl>>8)
	sec[2] = byte(sl)
	for i := 0; i < bodyLen; i++ {
		sec = append(sec, byte(i))
	}
	crc := CRC32MPEG(sec)
	return append(sec, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

func TestAssemblerCrossPacket(t *testing.T) {
	var c Counters
	a := NewAssembler(0)
	sec := buildSection(t, 0x00, 200)
	// First packet: pointer 0 + first 100 bytes.
	p1 := append([]byte{0x00}, sec[:100]...)
	if out := a.Feed(p1, true, 0, &c); len(out) != 0 {
		t.Fatalf("unexpected section after part 1")
	}
	// Second packet: continuation, no PUSI.
	out := a.Feed(sec[100:], false, 188, &c)
	if len(out) != 1 || len(out[0].Raw) != len(sec) {
		t.Fatalf("cross-packet assembly failed: %d sections", len(out))
	}
}

func TestAssemblerPointerResidual(t *testing.T) {
	var c Counters
	a := NewAssembler(0)
	sec1 := buildSection(t, 0x00, 10)
	sec2 := buildSection(t, 0x02, 20)
	// One packet carries the tail of sec1 and all of sec2.
	payload := []byte{byte(len(sec1) - 3)}
	payload = append(payload, sec1[3:]...)
	payload = append(payload, sec2...)
	a.buf = append(a.buf, sec1[:3]...)
	a.need = 0
	out := a.Feed(payload, true, 0, &c)
	if len(out) != 2 {
		t.Fatalf("got %d sections, want 2", len(out))
	}
}

func TestAssemblerStuffingAndBadCRC(t *testing.T) {
	var c Counters
	a := NewAssembler(0)
	sec := buildSection(t, 0x00, 10)
	payload := []byte{0x00}
	payload = append(payload, sec...)
	payload = append(payload, 0xFF, 0xFF, 0xFF) // stuffing
	if out := a.Feed(payload, true, 0, &c); len(out) != 1 {
		t.Fatalf("stuffing: got %d sections", len(out))
	}
	bad := buildSection(t, 0x00, 10)
	bad[5] ^= 0xFF
	if out := a.Feed(append([]byte{0x00}, bad...), true, 0, &c); len(out) != 0 {
		t.Fatalf("bad CRC section not dropped")
	}
	if c.CRCErrors != 1 {
		t.Fatalf("crc_errors=%d, want 1", c.CRCErrors)
	}
}

func TestAssemblerOversizedSection(t *testing.T) {
	var c Counters
	a := NewAssembler(0)
	// section_length 4094 exceeds the 4093 limit.
	payload := []byte{0x00, 0x00, 0xFF, 0xFE}
	if out := a.Feed(payload, true, 0, &c); len(out) != 0 {
		t.Fatalf("oversized section accepted")
	}
	if c.LengthErrors != 1 {
		t.Fatalf("length_errors=%d, want 1", c.LengthErrors)
	}
}

func TestRegistryVersionMachine(t *testing.T) {
	r := NewRegistry()
	mk := func(version byte, cn bool) []byte {
		sec := []byte{0x00, 0xB0, 0x0D, 0x00, 0x01,
			0xC0 | version<<1, 0x00, 0x00,
			0x00, 0x01, 0xE1, 0x00}
		if cn {
			sec[5] |= 0x01
		}
		crc := CRC32MPEG(sec)
		return append(sec, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	}
	r.ApplyPATSection(mk(0, true))  // initial
	r.ApplyPATSection(mk(0, true))  // repeat
	r.ApplyPATSection(mk(1, true))  // change
	r.ApplyPATSection(mk(5, true))  // change + skip
	r.ApplyPATSection(mk(2, true))  // change + rollback
	r.ApplyPATSection(mk(9, false)) // not current: ignored
	st := r.PATStats
	if st.Changes != 3 || st.Repeats != 1 || st.Skips != 1 || st.Rollbacks != 1 || st.NotCurrent != 1 {
		t.Fatalf("stats=%+v", st)
	}
	if r.PAT == nil || r.PAT.Version != 2 {
		t.Fatalf("current version=%v", r.PAT)
	}
}

func TestRegistryVersionReset31(t *testing.T) {
	r := NewRegistry()
	mk := func(version byte) []byte {
		sec := []byte{0x00, 0xB0, 0x0D, 0x00, 0x01,
			0xC1 | version<<1, 0x00, 0x00,
			0x00, 0x01, 0xE1, 0x00}
		crc := CRC32MPEG(sec)
		return append(sec, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	}
	r.ApplyPATSection(mk(31))
	r.ApplyPATSection(mk(0)) // 31 -> 0 reset is a normal change
	if st := r.PATStats; st.Changes != 1 || st.Rollbacks != 0 {
		t.Fatalf("stats=%+v", st)
	}
}
