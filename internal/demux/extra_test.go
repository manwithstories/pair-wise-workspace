package demux

import (
	"bytes"
	"encoding/binary"
	"testing"

	"archive/internal/fault"
)

func mkBox(typ string, payload []byte) []byte {
	out := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(8+len(payload)))
	copy(out[4:], typ)
	return append(out, payload...)
}

func TestDemuxRejectsGarbage(t *testing.T) {
	cases := map[string][]byte{
		"empty":     nil,
		"short":     []byte{0, 0, 0, 8},
		"not a box": []byte("this is definitely not an mp4 file at all"),
	}
	for name, data := range cases {
		tracks, err := Demux(data, name)
		if err == nil {
			t.Errorf("%s: expected a fault", name)
			continue
		}
		if fault.LevelOf(err) != fault.Fatal {
			t.Errorf("%s: level = %v, want fatal", name, fault.LevelOf(err))
		}
		if len(tracks) != 0 {
			t.Errorf("%s: expected no tracks, got %d", name, len(tracks))
		}
	}
}

func TestDemuxTruncatedIsDegraded(t *testing.T) {
	// A structurally truncated box must be reported as degraded (fatal
	// in the pipeline sense: stop, but not a programming error).
	ftyp := mkBox("ftyp", append([]byte("iso6\x00\x00\x00\x00"), []byte("iso6")...))
	data := append(ftyp, 0, 0, 0, 100, 'm', 'o', 'o', 'v') // claims 100 bytes, only 8 present
	_, err := Demux(data, "trunc.m4s")
	if err == nil {
		t.Fatal("expected a fault")
	}
	if fault.LevelOf(err) != fault.Degraded {
		t.Fatalf("level = %v, want degraded", fault.LevelOf(err))
	}
}

func TestDemuxRejectsOrphanFragmentBox(t *testing.T) {
	data := mkBox("moof", mkBox("trun", make([]byte, 8)))
	_, err := Demux(data, "orphan.m4s")
	if err == nil || fault.LevelOf(err) != fault.Fatal {
		t.Fatalf("orphan trun must be fatal, got %v", err)
	}
}

func TestDemuxRejectsMultipleTrun(t *testing.T) {
	// Two trun boxes in one traf: the second data offset would be
	// silently wrong, so demux must refuse rather than mis-assemble.
	var traf bytes.Buffer
	traf.Write(mkBox("tfhd", []byte{0, 0, 0, 0, 0, 0, 0, 1}))
	trunBody := make([]byte, 8) // version/flags, sample count 0
	traf.Write(mkBox("trun", trunBody))
	traf.Write(mkBox("trun", trunBody))
	moof := mkBox("moof", mkBox("traf", traf.Bytes()))
	data := append(moof, mkBox("mdat", nil)...)
	_, err := Demux(data, "multi.m4s")
	if err == nil || fault.LevelOf(err) != fault.Fatal {
		t.Fatalf("multiple trun must be fatal, got %v", err)
	}
}
