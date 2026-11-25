package ts_test

import (
	"encoding/binary"
	"testing"

	"mediqc/internal/media"
	"mediqc/internal/remux"
	"mediqc/internal/ts"
)

func testTracks() []media.Track {
	return []media.Track{
		{ID: 1, PID: 0x100, Kind: media.TrackVideo, Codec: media.CodecH264, Timescale: 90000},
		{ID: 2, PID: 0x101, Kind: media.TrackAudio, Codec: media.CodecAAC, Timescale: 90000},
	}
}

func pmtPacketForVersion(t *testing.T, version byte) []byte {
	t.Helper()
	b, err := remux.BuildTS(testTracks(), nil, remux.TSOptions{PMTVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 2*188 {
		t.Fatalf("short TS: %d", len(b))
	}
	return b[188 : 2*188]
}

func TestPSICRCAndVersionSwitch(t *testing.T) {
	limits := media.DefaultLimits()
	r := &ts.Registry{}
	pmt0 := pmtPacketForVersion(t, 0)[5:]
	if _, err := r.ApplyPMT(pmt0, limits); err != nil {
		t.Fatalf("PMT v0 rejected: %v", err)
	}
	if r.TrustedPMT == nil || r.TrustedPMT.Version != 0 || len(r.TrustedPMT.Streams) != 2 {
		t.Fatalf("bad trusted PMT: %+v", r.TrustedPMT)
	}

	badPacket := pmtPacketForVersion(t, 1)
	sectionLen := int(binary.BigEndian.Uint16(badPacket[6:8])&0x0fff) + 3
	crcLast := 5 + sectionLen - 1
	badPacket[crcLast] ^= 0xff
	if _, err := r.ApplyPMT(badPacket[5:], limits); err == nil || err.Code != media.CodeCRCInvalid {
		t.Fatalf("expected CRC invalid, got %v", err)
	}
	if r.TrustedPMT.Version != 0 || r.PMTSwitches != 0 {
		t.Fatalf("corrupt update changed trusted PMT: %+v", r.TrustedPMT)
	}

	good1 := pmtPacketForVersion(t, 1)[5:]
	if _, err := r.ApplyPMT(good1, limits); err != nil {
		t.Fatalf("valid PMT v1 rejected: %v", err)
	}
	if r.TrustedPMT.Version != 1 || r.PMTSwitches != 1 {
		t.Fatalf("switch metrics wrong: version=%d switches=%d", r.TrustedPMT.Version, r.PMTSwitches)
	}

	future := pmtPacketForVersion(t, 5)[5:]
	if _, err := r.ApplyPMT(future, limits); err == nil || err.Code != media.CodePSIVersion {
		t.Fatalf("expected version consistency error, got %v", err)
	}
}

func TestSectionAssemblerAcrossPackets(t *testing.T) {
	b, err := remux.BuildTS(testTracks(), nil, remux.TSOptions{})
	if err != nil {
		t.Fatal(err)
	}
	patPayload := b[4:188] // includes pointer field and one padded PAT section
	a := &ts.SectionAssembler{}
	sections, merr := a.Push(true, patPayload[:14], media.DefaultLimits())
	if merr != nil || len(sections) != 0 {
		t.Fatalf("first fragment should be incomplete: sections=%d err=%v", len(sections), merr)
	}
	sections, merr = a.Push(false, patPayload[14:], media.DefaultLimits())
	if merr != nil {
		t.Fatal(merr)
	}
	if len(sections) != 1 {
		t.Fatalf("sections = %d, want 1", len(sections))
	}
	sec, perr := ts.ParseSection(sections[0], media.DefaultLimits())
	if perr != nil {
		t.Fatal(perr)
	}
	if !sec.CRCOK || sec.TableID != 0 {
		t.Fatalf("section not valid PAT: %+v", sec)
	}
}
