package journal

import (
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediamerge/internal/fault"
)

func hash(s string) [32]byte { return sha256.Sum256([]byte(s)) }

func tempWAL(t *testing.T) (*WAL, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := OpenWAL(filepath.Join(dir, "run.wal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, filepath.Join(dir, "output.bin")
}

func TestRecordRoundtrip(t *testing.T) {
	r := Record{Seq: 7, Kind: KindOutput, Stage: uint16(StageAudio), TrackID: 3, Offset: 4096, Digest: hash("payload"), Summary: "len=1024"}
	buf := Encode(r)
	got, n, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(buf) {
		t.Fatalf("consumed %d of %d", n, len(buf))
	}
	if got.Seq != 7 || got.Kind != KindOutput || got.Stage != uint16(StageAudio) || got.TrackID != 3 || got.Offset != 4096 || got.Summary != "len=1024" || got.Digest != r.Digest {
		t.Fatalf("roundtrip %+v", got)
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	buf := Encode(Record{Seq: 1, Kind: KindBegin, Stage: 1})
	for i := 0; i < len(buf); i++ {
		bad := append([]byte(nil), buf...)
		bad[i] ^= 0xff
		if _, _, err := Decode(bad); err == nil && i >= 4 {
			t.Fatalf("flipping byte %d was not detected", i)
		}
	}
	if _, _, err := Decode(buf[:10]); err == nil {
		t.Fatal("short record must fail")
	}
}

func TestWALAppendAndReopen(t *testing.T) {
	w, _ := tempWAL(t)
	path := w.Path()
	for i := 0; i < 3; i++ {
		if _, err := w.Append(Record{Kind: KindBegin, Stage: uint16(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if w.Size() == 0 {
		t.Fatal("size")
	}
	w.Close()
	w2, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	recs, tail, err := w2.Read()
	if err != nil || len(recs) != 3 || tail.TruncatedBytes != 0 {
		t.Fatalf("%d %+v %v", len(recs), tail, err)
	}
	r, err := w2.Append(Record{Kind: KindCommit, Stage: 1})
	if err != nil || r.Seq != 4 {
		t.Fatalf("seq %d err %v", r.Seq, err)
	}
}

func TestWALTornTailIsTruncated(t *testing.T) {
	w, _ := tempWAL(t)
	path := w.Path()
	for i := 0; i < 4; i++ {
		w.Append(Record{Kind: KindBegin, Stage: uint16(i + 1)})
	}
	w.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the last 5 bytes of the final record, which live in its summary
	// body, so the header arrives but the payload does not.
	torn := data[:len(data)-5]
	if err := os.WriteFile(path, torn, 0o644); err != nil {
		t.Fatal(err)
	}
	// Reading the damaged bytes directly must report the torn tail.
	raw, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recs, tail, err := ReadAll(raw)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("valid records %d", len(recs))
	}
	if tail.TruncatedBytes == 0 || tail.TruncatedReason == "" {
		t.Fatalf("tail not reported: %+v", tail)
	}
	validBytes := tail.ValidBytes

	w2, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	if w2.Size() != validBytes {
		t.Fatalf("recovered size %d want %d", w2.Size(), validBytes)
	}
	// The writer must continue after the last valid record, not append
	// behind the torn bytes.
	r, err := w2.Append(Record{Kind: KindBegin, Stage: 9})
	if err != nil || r.Seq != 4 {
		t.Fatalf("append after recovery seq %d err %v", r.Seq, err)
	}
}

func writeOutput(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func frag(off int64, data []byte) OutputFragment {
	return OutputFragment{Offset: off, Length: int64(len(data)), Digest: sha256.Sum256(data)}
}

func TestCommitManagerHappyPath(t *testing.T) {
	wal, out := tempWAL(t)
	writeOutput(t, out, []byte("abcdefghij"))
	cm := NewCommitManager(wal, out)
	if err := cm.Begin(StageParse, hash("input"), 10); err != nil {
		t.Fatal(err)
	}
	if err := cm.AppendOutput(StageParse, frag(0, []byte("abcde"))); err != nil {
		t.Fatal(err)
	}
	if err := cm.AppendOutput(StageParse, frag(5, []byte("fghij"))); err != nil {
		t.Fatal(err)
	}
	if err := cm.Commit(StageParse, ""); err != nil {
		t.Fatal(err)
	}
	if len(cm.CommittedDigests()) != 2 {
		t.Fatalf("digests %d", len(cm.CommittedDigests()))
	}
	rec, err := Recover(wal.Path(), out)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.CommittedStages) != 1 || rec.CommittedStages[0] != StageParse {
		t.Fatalf("committed %+v", rec.CommittedStages)
	}
	if rec.ResumeFrom != StageAudio {
		t.Fatalf("resume from %v", rec.ResumeFrom)
	}
	if rec.RolledBackBytes != 0 {
		t.Fatalf("rolled back %d", rec.RolledBackBytes)
	}
}

func TestCommitRejectsChecksumMismatch(t *testing.T) {
	wal, out := tempWAL(t)
	writeOutput(t, out, []byte("abcdefghij"))
	cm := NewCommitManager(wal, out)
	cm.Begin(StageParse, hash("in"), 10)
	good := frag(0, []byte("abcde"))
	bad := good
	bad.Digest = hash("something else")
	cm.AppendOutput(StageParse, bad)
	err := cm.Commit(StageParse, "")
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeChecksumMismatch {
		t.Fatalf("bad error %v", err)
	}
	rec, err := Recover(wal.Path(), out)
	if err != nil {
		t.Fatal(err)
	}
	if rec.States[StageParse] != StatePending {
		t.Fatalf("uncommitted stage state %v", rec.States[StageParse])
	}
}

func TestCommitRejectsShortOutput(t *testing.T) {
	wal, out := tempWAL(t)
	writeOutput(t, out, []byte("abc"))
	cm := NewCommitManager(wal, out)
	cm.Begin(StageParse, hash("in"), 3)
	cm.AppendOutput(StageParse, frag(0, []byte("abcdef")))
	if err := cm.Commit(StageParse, ""); err == nil {
		t.Fatal("expected short-output rejection")
	}
}

func TestRollbackTruncatesUncommittedTail(t *testing.T) {
	wal, out := tempWAL(t)
	writeOutput(t, out, []byte("0123"))
	cm := NewCommitManager(wal, out)
	cm.Begin(StageParse, hash("in1"), 4)
	cm.AppendOutput(StageParse, frag(0, []byte("0123")))
	if err := cm.Commit(StageParse, ""); err != nil {
		t.Fatal(err)
	}
	// Second stage writes six more bytes but never commits.
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("456789"))
	f.Close()
	cm.Begin(StageAudio, hash("in2"), 6)
	cm.AppendOutput(StageAudio, frag(4, []byte("456789")))

	discarded, err := cm.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if discarded != 6 {
		t.Fatalf("discarded %d want 6", discarded)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "0123" {
		t.Fatalf("output %q", data)
	}
}

func TestRecoverRollsBackAndReports(t *testing.T) {
	wal, out := tempWAL(t)
	writeOutput(t, out, []byte("0123456789"))
	cm := NewCommitManager(wal, out)
	cm.Begin(StageParse, hash("in1"), 4)
	cm.AppendOutput(StageParse, frag(0, []byte("0123")))
	if err := cm.Commit(StageParse, "parse ok"); err != nil {
		t.Fatal(err)
	}
	cm.Begin(StageAudio, hash("in2"), 6)
	cm.AppendOutput(StageAudio, frag(4, []byte("456789")))
	wal.Close()

	rec, err := Recover(wal.Path(), out)
	if err != nil {
		t.Fatal(err)
	}
	if rec.RolledBackBytes != 6 {
		t.Fatalf("rolled back %d", rec.RolledBackBytes)
	}
	if rec.OutputBytes != 4 {
		t.Fatalf("output bytes %d", rec.OutputBytes)
	}
	if rec.ResumeFrom != StageAudio {
		t.Fatalf("resume from %v", rec.ResumeFrom)
	}
	if rec.States[StageParse] != StateCommitted || rec.States[StageAudio] != StatePending {
		t.Fatalf("states %+v", rec.States)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "0123" {
		t.Fatalf("output %q", data)
	}
	// A second recovery must see no open stage and no further rollback.
	rec2, err := Recover(wal.Path(), out)
	if err != nil {
		t.Fatal(err)
	}
	if rec2.RolledBackBytes != 0 {
		t.Fatalf("second rollback %d", rec2.RolledBackBytes)
	}
	if rec2.ResumeFrom != StageAudio {
		t.Fatalf("second resume %v", rec2.ResumeFrom)
	}
}

func TestRecoverDetectsSequenceGap(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "gap.wal")
	var buf []byte
	buf = append(buf, Encode(Record{Seq: 1, Kind: KindBegin, Stage: 1})...)
	buf = append(buf, Encode(Record{Seq: 3, Kind: KindCommit, Stage: 1})...)
	if err := os.WriteFile(walPath, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.bin")
	os.WriteFile(outPath, nil, 0o644)
	_, err := Recover(walPath, outPath)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeWALGap {
		t.Fatalf("bad error %v", err)
	}
}

func TestRecoverDetectsOutputShortfall(t *testing.T) {
	wal, out := tempWAL(t)
	writeOutput(t, out, []byte("0123456789"))
	cm := NewCommitManager(wal, out)
	cm.Begin(StageParse, hash("in"), 10)
	cm.AppendOutput(StageParse, frag(0, []byte("0123456789")))
	if err := cm.Commit(StageParse, ""); err != nil {
		t.Fatal(err)
	}
	wal.Close()
	// Damage the committed prefix.
	if err := os.Truncate(out, 4); err != nil {
		t.Fatal(err)
	}
	_, err := Recover(wal.Path(), out)
	if err == nil {
		t.Fatal("expected output shortfall")
	}
}

func TestRecoverMissingWALStartsFresh(t *testing.T) {
	dir := t.TempDir()
	rec, err := Recover(filepath.Join(dir, "nope.wal"), filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if rec.ResumeFrom != StageParse || len(rec.CommittedStages) != 0 {
		t.Fatalf("%+v", rec)
	}
}

func TestBeginRejectsDoubleOpen(t *testing.T) {
	wal, out := tempWAL(t)
	cm := NewCommitManager(wal, out)
	if err := cm.Begin(StageParse, hash("a"), 1); err != nil {
		t.Fatal(err)
	}
	var fe *fault.Error
	if err := cm.Begin(StageAudio, hash("b"), 1); !errors.As(err, &fe) || fe.Code != fault.CodeWALGap {
		t.Fatalf("bad error %v", err)
	}
	if err := cm.AppendOutput(StageAudio, frag(0, []byte("x"))); err == nil {
		t.Fatal("output before begin must fail")
	}
}

func TestReadAllPartialRecordHeader(t *testing.T) {
	recs, tail, err := ReadAll(io.MultiReader(
		&sliceReader{data: Encode(Record{Seq: 1, Kind: KindBegin, Stage: 1})},
		&sliceReader{data: []byte{1, 2, 3}},
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || tail.TruncatedBytes != 3 {
		t.Fatalf("%d %+v", len(recs), tail)
	}
}

type sliceReader struct {
	data []byte
	off  int
}

func (s *sliceReader) Read(p []byte) (int, error) {
	if s.off >= len(s.data) {
		return 0, io.EOF
	}
	n := copy(p, s.data[s.off:])
	s.off += n
	return n, nil
}

func TestRecoverRejectsTamperedCommittedFragment(t *testing.T) {
	wal, out := tempWAL(t)
	writeOutput(t, out, []byte("0123456789"))
	cm := NewCommitManager(wal, out)
	cm.Begin(StageParse, hash("in"), 10)
	cm.AppendOutput(StageParse, frag(0, []byte("0123456789")))
	if err := cm.Commit(StageParse, ""); err != nil {
		t.Fatal(err)
	}
	wal.Close()
	// Flip one byte inside the committed range.
	data, _ := os.ReadFile(out)
	data[3] ^= 0xff
	os.WriteFile(out, data, 0o644)
	_, err := Recover(wal.Path(), out)
	var fe *fault.Error
	if !errors.As(err, &fe) || fe.Code != fault.CodeChecksumMismatch {
		t.Fatalf("bad error %v", err)
	}
	if !strings.Contains(fe.Detail, "committed fragment 1") {
		t.Fatalf("detail %q lacks fragment location", fe.Detail)
	}
}

func TestRecoverIgnoresUncommittedFragments(t *testing.T) {
	wal, out := tempWAL(t)
	writeOutput(t, out, []byte("0123"))
	cm := NewCommitManager(wal, out)
	cm.Begin(StageParse, hash("in1"), 4)
	cm.AppendOutput(StageParse, frag(0, []byte("0123")))
	if err := cm.Commit(StageParse, ""); err != nil {
		t.Fatal(err)
	}
	// An uncommitted stage records a fragment that does not match the
	// bytes on disk; recovery must still succeed and roll it back.
	cm.Begin(StageAudio, hash("in2"), 6)
	cm.AppendOutput(StageAudio, frag(0, []byte("xxxxxx")))
	wal.Close()
	os.Truncate(out, 4)
	rec, err := Recover(wal.Path(), out)
	if err != nil {
		t.Fatalf("uncommitted fragment must not fail recovery: %v", err)
	}
	if rec.ResumeFrom != StageAudio {
		t.Fatalf("resume %v", rec.ResumeFrom)
	}
}
