package source

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func mp4Box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func fullBox(version byte, fields []byte) []byte {
	p := append([]byte{version, 0, 0, 0}, fields...)
	return p
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func TestInspectEditListAndTrackOffset(t *testing.T) {
	mvhd := fullBox(0, make([]byte, 96))
	binary.BigEndian.PutUint32(mvhd[12:16], 1000) // movie timescale

	tkhd := fullBox(0, make([]byte, 80))
	binary.BigEndian.PutUint32(tkhd[12:16], 7) // track ID

	mdhd := fullBox(0, make([]byte, 20))
	binary.BigEndian.PutUint32(mdhd[12:16], 48_000)
	binary.BigEndian.PutUint32(mdhd[16:20], 48_000*30)

	elstFields := append(u32(1), u32(10_000)...)         // one 10s edit in movie timescale
	elstFields = append(elstFields, u32(48_000)...)      // start at 1s in media timescale
	elstFields = append(elstFields, u32(0x0001_0000)...) // rate 1.0
	elst := mp4Box("elst", fullBox(0, elstFields))
	edts := mp4Box("edts", elst)
	mdia := mp4Box("mdia", mp4Box("mdhd", mdhd))
	trak := mp4Box("trak", append(append(mp4Box("tkhd", tkhd), edts...), mdia...))
	moov := mp4Box("moov", append(mp4Box("mvhd", mvhd), trak...))
	ftyp := mp4Box("ftyp", []byte("isom"))
	mdat := mp4Box("mdat", make([]byte, 32))

	path := filepath.Join(t.TempDir(), "edit.mp4")
	if err := os.WriteFile(path, append(append(ftyp, mdat...), moov...), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := InspectBoxes(path)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.MoovPresent || !rep.MoovComplete || rep.MovieTimeScale != 1000 {
		t.Fatalf("bad moov report: %+v", rep)
	}
	if len(rep.Tracks) != 1 {
		t.Fatalf("tracks=%+v", rep.Tracks)
	}
	tr := rep.Tracks[0]
	if tr.TrackID != 7 || tr.TimeScale != 48_000 || tr.OffsetSec != 1 {
		t.Fatalf("bad track layout: %+v", tr)
	}
	if len(tr.Edits) != 1 || tr.Edits[0].SegmentDurationSec != 10 || tr.Edits[0].MediaTimeSec != 1 {
		t.Fatalf("bad edits: %+v", tr.Edits)
	}
}

func TestDetectTruncatedAndMissingMoov(t *testing.T) {
	moov := mp4Box("moov", make([]byte, 200))
	mdat := mp4Box("mdat", make([]byte, 64))
	ftyp := mp4Box("ftyp", []byte("isom"))
	good := append(append(ftyp, mdat...), moov...)

	truncPath := filepath.Join(t.TempDir(), "trunc.mp4")
	if err := os.WriteFile(truncPath, good[:len(good)-24], 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err := InspectBoxes(truncPath)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated || rep.TruncatedBox != "moov" || !rep.MoovPresent || rep.MoovComplete {
		t.Fatalf("expected truncated moov, got %+v", rep)
	}

	missingPath := filepath.Join(t.TempDir(), "missing.mp4")
	if err := os.WriteFile(missingPath, append(ftyp, mdat...), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, err = InspectBoxes(missingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated || rep.TruncatedBox != "moov-missing" {
		t.Fatalf("expected missing moov, got %+v", rep)
	}
}
