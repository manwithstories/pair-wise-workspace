package psi

import (
	"encoding/binary"
	"testing"
)

func validSection(pid uint16, data []byte) Section {
	return Section{PID: pid, Data: data, CRCValid: CRC32(data) == 0}
}

func setCurrent(data []byte, current bool) []byte {
	out := append([]byte(nil), data...)
	body := out[:len(out)-4]
	if current {
		body[5] |= 1
	} else {
		body[5] &^= 1
	}
	var crc [4]byte
	binary.BigEndian.PutUint32(crc[:], CRC32(body))
	copy(out[len(out)-4:], crc[:])
	return out
}

func TestParsePATPMTSDTWithDescriptors(t *testing.T) {
	patSection, err := BuildPAT(7, 1, []PATProgram{{Program: 1, PID: 0x100}, {Program: 2, PID: 0x101}})
	if err != nil {
		t.Fatal(err)
	}
	pat, err := ParsePAT(patSection)
	if err != nil {
		t.Fatal(err)
	}
	if pat.Version != 1 || !pat.CurrentNext || len(pat.Programs) != 2 || pat.Programs[0].PID != 0x100 {
		t.Fatalf("unexpected PAT: %+v", pat)
	}

	pmtSection, err := BuildPMT(1, 0x200, 2, []ESInfo{{
		StreamType: 0x1b, ElementaryPID: 0x200,
		Descriptors: []Descriptor{{Tag: 0x05, Data: []byte("h264")}},
	}}, []Descriptor{{Tag: 0x0f, Data: []byte{1, 2, 3}}})
	if err != nil {
		t.Fatal(err)
	}
	pmt, err := ParsePMT(pmtSection)
	if err != nil {
		t.Fatal(err)
	}
	if pmt.Version != 2 || pmt.PCRPID != 0x200 || len(pmt.Streams) != 1 || len(pmt.Streams[0].Descriptors) != 1 {
		t.Fatalf("unexpected PMT: %+v", pmt)
	}
	if StreamTypeName(pmt.Streams[0].StreamType) != "H264" || !IsVideoStreamType(0x24) {
		t.Fatalf("stream classification mismatch")
	}

	sdtSection, err := BuildSDT(7, 9, 3, []Service{{ID: 1, ServiceType: 1}, {ID: 2, ServiceType: 1}})
	if err != nil {
		t.Fatal(err)
	}
	sdt, err := ParseSDT(sdtSection)
	if err != nil {
		t.Fatal(err)
	}
	if sdt.Version != 3 || sdt.OriginalNetworkID != 9 || len(sdt.Services) != 2 {
		t.Fatalf("unexpected SDT: %+v", sdt)
	}
}

func TestStorePATVersionsAndCurrentNext(t *testing.T) {
	store := NewStore()
	v0, _ := BuildPAT(1, 0, []PATProgram{{Program: 1, PID: 0x100}})
	v1, _ := BuildPAT(1, 1, []PATProgram{{Program: 1, PID: 0x100}})
	if err := store.Consume(validSection(0, v0)); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume(validSection(0, v0)); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume(validSection(0, setCurrent(v1, false))); err != nil {
		t.Fatal(err)
	}
	if store.PATVersions() != 1 {
		t.Fatalf("non-current PAT changed version count: %d", store.PATVersions())
	}
	if err := store.Consume(validSection(0, v1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Consume(validSection(0, v0)); err != nil {
		t.Fatal(err)
	}
	if store.PATVersions() != 2 || store.PATRollbacks() != 1 {
		t.Fatalf("pat versions=%d rollbacks=%d, want 2/1", store.PATVersions(), store.PATRollbacks())
	}
	if store.Snapshot().PATVersion != 0 {
		t.Fatalf("rollback was not applied")
	}
}

func TestStoreMappingIntervalsAndDangling(t *testing.T) {
	store := NewStore()
	pat0, _ := BuildPAT(1, 0, []PATProgram{{Program: 1, PID: 0x100}, {Program: 2, PID: 0x101}})
	if err := store.Consume(validSection(0, pat0)); err != nil {
		t.Fatal(err)
	}
	if got := store.DanglingPrograms(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("dangling after PAT=%v", got)
	}

	pmt0, _ := BuildPMT(1, 0x200, 0, []ESInfo{{StreamType: 0x1b, ElementaryPID: 0x200}}, nil)
	if err := store.Consume(validSection(0x100, pmt0)); err != nil {
		t.Fatal(err)
	}
	if got := store.DanglingPrograms(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("dangling after PMT=%v, want [2]", got)
	}
	for i := 0; i < 3; i++ {
		store.RecordPacket(0x200)
	}
	pmt1, _ := BuildPMT(1, 0x200, 1, []ESInfo{{StreamType: 0x1b, ElementaryPID: 0x200}}, nil)
	if err := store.Consume(validSection(0x100, pmt1)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		store.RecordPacket(0x200)
	}
	pmt2, _ := BuildPMT(1, 0x200, 2, []ESInfo{{StreamType: 0x0f, ElementaryPID: 0x200}}, nil)
	if err := store.Consume(validSection(0x100, pmt2)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		store.RecordPacket(0x200)
	}
	pmt3, _ := BuildPMT(1, 0x200, 3, nil, nil)
	if err := store.Consume(validSection(0x100, pmt3)); err != nil {
		t.Fatal(err)
	}
	pat1, _ := BuildPAT(1, 1, []PATProgram{{Program: 2, PID: 0x101}, {Program: 3, PID: 0x102}})
	if err := store.Consume(validSection(0, pat1)); err != nil {
		t.Fatal(err)
	}

	pidMap := store.PIDMap()
	if len(pidMap) != 2 {
		t.Fatalf("pid intervals=%d, want 2: %+v", len(pidMap), pidMap)
	}
	if pidMap[0].PID != 0x200 || pidMap[0].StreamType != "H264" || pidMap[0].PacketCount != 5 || pidMap[0].EndVersion != 1 {
		t.Fatalf("unexpected H264 interval: %+v", pidMap[0])
	}
	if pidMap[1].StreamType != "AAC" || pidMap[1].PacketCount != 4 || pidMap[1].StartVersion != 2 || pidMap[1].EndVersion != 2 {
		t.Fatalf("unexpected AAC interval: %+v", pidMap[1])
	}
	if got := store.DanglingPrograms(); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Fatalf("dangling after PAT removal=%v", got)
	}
	if counts := store.PacketCounts(); counts[0x200] != 9 {
		t.Fatalf("packet counts=%v", counts)
	}
	if versions := store.PMTVersions(); len(versions) != 1 || versions[0].Program != 1 || versions[0].Versions != 4 {
		t.Fatalf("pmt versions=%+v", versions)
	}
}

func TestStoreRejectsBadCRCAndRepeatedPMTVersion(t *testing.T) {
	store := NewStore()
	pat, _ := BuildPAT(1, 0, []PATProgram{{Program: 1, PID: 0x100}})
	bad := validSection(0, pat)
	bad.CRCValid = false
	if err := store.Consume(bad); err == nil || store.HasPAT() {
		t.Fatalf("bad CRC polluted PAT state")
	}
	if err := store.Consume(validSection(0, pat)); err != nil {
		t.Fatal(err)
	}
	pmt, _ := BuildPMT(1, 0x200, 0, []ESInfo{{StreamType: 0x1b, ElementaryPID: 0x200}}, nil)
	for i := 0; i < 3; i++ {
		if err := store.Consume(validSection(0x100, pmt)); err != nil {
			t.Fatal(err)
		}
	}
	if versions := store.PMTVersions(); len(versions) != 1 || versions[0].Versions != 1 {
		t.Fatalf("repeated PMT version counted more than once: %+v", versions)
	}
}

func TestStorePMTRollback(t *testing.T) {
	store := NewStore()
	pat, _ := BuildPAT(1, 0, []PATProgram{{Program: 1, PID: 0x100}})
	_ = store.Consume(validSection(0, pat))
	for _, version := range []uint8{0, 1, 2, 1} {
		pmt, _ := BuildPMT(1, 0x200, version, []ESInfo{{StreamType: 0x1b, ElementaryPID: 0x200}}, nil)
		_ = store.Consume(validSection(0x100, pmt))
	}
	if store.PMTVersions()[0].Versions != 3 || store.PMTRollbacks()[1] != 1 {
		t.Fatalf("pmt rollback not tracked: versions=%+v rollbacks=%v",
			store.PMTVersions(), store.PMTRollbacks())
	}
}
