package psi

import (
	"encoding/binary"
	"testing"
)

func TestPATMultiSectionMerge(t *testing.T) {
	body0 := []byte{0, 1, 0xe1, 0x00, 0, 2, 0xe1, 0x01}
	body1 := []byte{0, 3, 0xe1, 0x02}
	sec0 := testSection(TablePAT, 77, 5, 0, 1, true, body0)
	sec1 := testSection(TablePAT, 77, 5, 1, 1, true, body1)
	m := &PATMerger{}
	if _, complete, err := m.Add(sec0); err != nil || complete {
		t.Fatalf("first section: complete=%t err=%v", complete, err)
	}
	pat, complete, err := m.Add(sec1)
	if err != nil || !complete {
		t.Fatalf("second section: complete=%t err=%v", complete, err)
	}
	if pat.TransportStreamID != 77 || pat.Version != 5 || len(pat.Programs) != 3 || pat.Programs[3] != 0x102 {
		t.Fatalf("unexpected PAT: %+v", pat)
	}
}

func TestPMTDescriptorsAndPCR(t *testing.T) {
	programInfo := []byte{0x05, 0x04, 'T', 'S', 'A', 'U'}
	esDesc := []byte{0x0a, 0x04, 'e', 'n', 'g', 0x00}
	body := make([]byte, 4)
	binary.BigEndian.PutUint16(body[0:2], 0xe100)
	binary.BigEndian.PutUint16(body[2:4], 0xf000|uint16(len(programInfo)))
	body = append(body, programInfo...)
	body = append(body, 0x1b)
	var head [4]byte
	binary.BigEndian.PutUint16(head[0:2], 0xe101)
	binary.BigEndian.PutUint16(head[2:4], 0xf000|uint16(len(esDesc)))
	body = append(body, head[:]...)
	body = append(body, esDesc...)
	sec := testSection(TablePMT, 1, 2, 0, 0, true, body)
	pmt, err := ParsePMT(sec)
	if err != nil {
		t.Fatal(err)
	}
	if pmt.PCRPID != 0x100 || pmt.Streams[0].PID != 0x101 || pmt.Streams[0].StreamType != 0x1b {
		t.Fatalf("unexpected PMT: %+v", pmt)
	}
	if len(pmt.ProgramInfo) != 1 || pmt.ProgramInfo[0].Tag != 0x05 || len(pmt.Streams[0].Descriptors) != 1 {
		t.Fatalf("descriptors not traversed: %+v", pmt)
	}
}
