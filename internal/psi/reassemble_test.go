package psi

import "testing"

func packetize(section []byte) [][]byte {
	var out [][]byte
	first := true
	for pos := 0; pos < len(section); {
		n := 184
		if first {
			n = 183
		}
		end := pos + n
		if end > len(section) {
			end = len(section)
		}
		chunk := append([]byte(nil), section[pos:end]...)
		if first {
			chunk = append([]byte{0}, chunk...)
		}
		out = append(out, chunk)
		first, pos = false, end
	}
	return out
}

func TestReassemblerMultipleSectionsAndStuffing(t *testing.T) {
	a := BuildPAT(1, 0, true, []PATBuildProgram{{ProgramNumber: 1, PID: 0x100}})
	b := BuildPAT(1, 1, true, []PATBuildProgram{{ProgramNumber: 1, PID: 0x100}})
	payload := append([]byte{0}, a...)
	payload = append(payload, b...)
	payload = append(payload, 0xff, 0xff, 0xff)
	var got []*Section
	r := NewReassembler(func(s *Section) { got = append(got, s) })
	r.Feed(0, true, payload, 5)
	if len(got) != 2 || got[0].Version != 0 || got[1].Version != 1 || got[0].StartPacket != 5 {
		t.Fatalf("unexpected sections: %+v", got)
	}
}

func TestReassemblerCrossPacketAndPointer(t *testing.T) {
	info := append([]byte{0x7f, 250}, make([]byte, 250)...)
	pmt := BuildPMT(1, 0x101, 0, true, info, []PMTBuildStream{{StreamType: 0x1b, PID: 0x101}})
	parts := packetize(pmt)
	if len(parts) < 2 {
		t.Fatal("expected a PMT spanning packets")
	}
	pat := BuildPAT(1, 1, true, []PATBuildProgram{{ProgramNumber: 1, PID: 0x100}})
	var got []*Section
	r := NewReassembler(func(s *Section) { got = append(got, s) })
	r.Feed(0x100, true, parts[0], 10)
	for i := 1; i < len(parts); i++ {
		r.Feed(0x100, false, parts[i], int64(10+i))
	}
	next := append([]byte{0}, pat...)
	r.Feed(0, true, next, 20)
	if len(got) != 2 || got[0].TableID != TableIDPMT || got[1].TableID != TableIDPAT {
		t.Fatalf("unexpected sections: %+v", got)
	}
}

func TestReassemblerDropsBadCRCOtherwiseContinues(t *testing.T) {
	bad := BuildPAT(1, 0, true, []PATBuildProgram{{ProgramNumber: 1, PID: 0x100}})
	bad = append([]byte(nil), bad...)
	bad[len(bad)-1] ^= 0xff
	good := BuildPAT(1, 1, true, []PATBuildProgram{{ProgramNumber: 1, PID: 0x100}})
	var got []*Section
	r := NewReassembler(func(s *Section) { got = append(got, s) })
	r.Feed(0, true, append([]byte{0}, bad...), 1)
	r.Feed(0, true, append([]byte{0}, good...), 2)
	if len(got) != 1 || got[0].Version != 1 || r.Stats().CRCErrors != 1 {
		t.Fatalf("got=%+v stats=%+v", got, r.Stats())
	}
}
