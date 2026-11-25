package archivecheck_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"mediaarchive/internal/clock"
	"mediaarchive/internal/fault"
	"mediaarchive/internal/ingest"
	"mediaarchive/internal/samplegen"
)

func TestClockWraparoundAndVariableDurations(t *testing.T) {
	seq := clock.Unwrap16(0, 65534)
	seq = clock.Unwrap16(seq, 65535)
	seq = clock.Unwrap16(seq, 0)
	seq = clock.Unwrap16(seq, 1)
	if seq != 65537 {
		t.Fatalf("extended seq=%d want 65537", seq)
	}

	ts := clock.Unwrap32(0, 0xfffffffe)
	ts = clock.Unwrap32(ts, 0xffffffff)
	ts = clock.Unwrap32(ts, 0)
	ts = clock.Unwrap32(ts, 960)
	if ts != 1<<32+960 {
		t.Fatalf("extended ts=%d want %d", ts, 1<<32+960)
	}

	if d, err := clock.OpusDuration([]byte{0x08}); err != nil || d != 960 {
		t.Fatalf("opus 20ms duration=%d err=%v", d, err)
	}
	if d, err := clock.OpusDuration([]byte{0x10}); err != nil || d != 1920 {
		t.Fatalf("opus 40ms duration=%d err=%v", d, err)
	}
	if d, err := clock.OpusDuration([]byte{0x09}); err != nil || d != 1920 {
		t.Fatalf("opus code1 doubled duration=%d err=%v", d, err)
	}
	if d := clock.AACDuration(1024, 48000); d != 1024 {
		t.Fatalf("aac duration=%d want 1024", d)
	}
	if d := clock.AACDuration(480, 24000); d != 960 {
		t.Fatalf("scaled aac duration=%d want 960", d)
	}
}

func TestGeneratedMP4SampleTableAndOffsets(t *testing.T) {
	out := t.TempDir()
	params := samplegen.DefaultParams(41, 30, 1)
	corpus, err := samplegen.Generate(out, params, "clean")
	if err != nil {
		t.Fatal(err)
	}

	packets, findings, ferr := ingest.ReadMP4(corpus.AACPath, samplegen.TrackAAC, samplegen.AACGlobalNo)
	if ferr != nil {
		t.Fatalf("ReadMP4: %v", ferr)
	}
	if len(packets) != 10 {
		t.Fatalf("aac packets=%d want 10", len(packets))
	}
	if len(findings) != 0 {
		t.Fatalf("unexpected mp4 findings: %+v", findings)
	}
	first := packets[0]
	if first.No != 1 || first.Source.SampleIndex != 1 {
		t.Fatalf("first provenance no=%d sample=%d", first.No, first.Source.SampleIndex)
	}
	if first.Source.Offset != int64(corpus.AACDataOffset) {
		t.Fatalf("offset=%d want dataOffset=%d", first.Source.Offset, corpus.AACDataOffset)
	}
	if first.Duration <= 0 || first.Hash == "" {
		t.Fatalf("invalid aac normalization: %+v", first)
	}
}

func TestRTPRecordsAndShortPacketLocation(t *testing.T) {
	out := t.TempDir()
	corpus, err := samplegen.Generate(out, samplegen.DefaultParams(41, 30, 1), "clean")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{corpus.Opus2Path, corpus.Opus3Path} {
		packets, findings, ferr := ingest.ReadRTPRecords(path, filepath.Base(path), "opus")
		if ferr != nil {
			t.Fatalf("ReadRTPRecords %s: %v", path, ferr)
		}
		if len(packets) != 10 || len(findings) != 0 {
			t.Fatalf("%s packets=%d findings=%d", path, len(packets), len(findings))
		}
		for _, p := range packets {
			if p.ExtSeq == 0 || p.Duration <= 0 || p.Source.Offset <= 0 {
				t.Fatalf("unnormalized RTP packet: %+v", p)
			}
		}
	}

	packets, findings, ferr := ingest.ReadRTPRecords(corpus.ShortRTPPath, "edge", "opus")
	if ferr != nil {
		t.Fatalf("short stream should continue after bad packet: %v", ferr)
	}
	if len(packets) != 2 || len(findings) != 1 {
		t.Fatalf("packets=%d findings=%d, want 2 and 1", len(packets), len(findings))
	}
	if findings[0].Location == nil || findings[0].Location.Offset <= 0 {
		t.Fatalf("short RTP finding lacks source offset: %+v", findings[0])
	}
}

func TestTruncatedMP4BoxDamageLocation(t *testing.T) {
	out := t.TempDir()
	corpus, err := samplegen.Generate(out, samplegen.DefaultParams(41, 30, 1), "damage")
	if err != nil {
		t.Fatal(err)
	}
	fe, _ := ingest.LocateStructuralDamage(corpus.Damage.File)
	if fe == nil || fe.Location == nil {
		t.Fatalf("expected located damage, got %v", fe)
	}
	if fe.Location.Offset != corpus.Damage.ExpectedOffset {
		t.Fatalf("offset error: got=%d expected=%d", fe.Location.Offset, corpus.Damage.ExpectedOffset)
	}
}

func TestRejectOverlongBoxAndOutOfBoundsOffset(t *testing.T) {
	dir := t.TempDir()
	overlong := filepath.Join(dir, "overlong.mp4")
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[:4], uint32(ingest.MaxMP4BoxSize+1))
	copy(header[4:], "free")
	if err := os.WriteFile(overlong, header, 0o644); err != nil {
		t.Fatal(err)
	}
	fe, _ := ingest.LocateStructuralDamage(overlong)
	if fe == nil || fe.Kind != fault.KindBounds {
		t.Fatalf("expected bounds fault for overlong box, got %v", fe)
	}

	out := t.TempDir()
	corpus, err := samplegen.Generate(out, samplegen.DefaultParams(41, 30, 1), "clean")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(corpus.AACPath)
	if err != nil {
		t.Fatal(err)
	}
	offsetBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(offsetBytes, corpus.AACDataOffset)
	idx := bytes.Index(b, offsetBytes)
	if idx < 0 {
		t.Fatal("stco offset bytes not found")
	}
	binary.BigEndian.PutUint32(b[idx:idx+4], 0xffffffff)
	badMP4 := filepath.Join(dir, "out-of-bounds.mp4")
	if err := os.WriteFile(badMP4, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, ferr := ingest.ReadMP4(badMP4, samplegen.TrackAAC, samplegen.AACGlobalNo)
	if ferr == nil || ferr.Kind != fault.KindBounds {
		t.Fatalf("expected bounds fault for out-of-bounds stco, got %v", ferr)
	}
}

func TestRejectUnknownTrackCodec(t *testing.T) {
	out := t.TempDir()
	corpus, err := samplegen.Generate(out, samplegen.DefaultParams(41, 30, 1), "clean")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(corpus.AACPath)
	if err != nil {
		t.Fatal(err)
	}
	idx := bytes.Index(b, []byte("mp4a"))
	if idx < 0 {
		t.Fatal("mp4a sample entry not found")
	}
	copy(b[idx:idx+4], "zzzz")
	bad := filepath.Join(t.TempDir(), "unknown-codec.mp4")
	if err := os.WriteFile(bad, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, ferr := ingest.ReadMP4(bad, "unknown", nil)
	if ferr == nil || ferr.Kind != fault.KindUnsupported {
		t.Fatalf("expected unsupported codec fault, got %v", ferr)
	}
}
