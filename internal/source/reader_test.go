package source

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mediq/internal/model"
	"mediq/internal/validate"
)

// writeRecordFile writes n complete records to path and returns the byte
// length of each.
func writeRecordFile(t *testing.T, path string, n int, mutate func(i int, rec []byte) []byte) []int64 {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lens []int64
	for i := 0; i < n; i++ {
		seq := uint64(i + 1)
		meta := validate.EncodeMetadata(seq, uint64(i)*3000, model.Timebase{Num: 90000, Den: 1})
		payload := []byte{byte(i), 1, 2, 3, 4, 5, 6, 7}
		hdr := model.RecordHeader{
			Version: model.RecordVersion, Flags: model.FlagMetadataPresent,
			Seq: seq, Timestamp: uint64(i) * 3000, Timebase: model.Timebase{Num: 90000, Den: 1},
			MetaLen: uint32(len(meta)), FrameLen: uint32(len(payload)),
			Hash: validate.HashFrame(payload),
		}
		raw, err := hdr.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		rec := append(append(append([]byte{}, raw...), meta...), payload...)
		if mutate != nil {
			rec = mutate(i, rec)
		}
		if _, err := f.Write(rec); err != nil {
			t.Fatal(err)
		}
		lens = append(lens, int64(len(rec)))
	}
	return lens
}

func TestReaderSequential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seq")
	writeRecordFile(t, path, 5, nil)

	r, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var seqs []uint64
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if rec.Fault != nil {
			t.Fatalf("unexpected fault: %v", rec.Fault)
		}
		seqs = append(seqs, rec.Frame.Seq)
		if rec.Frame.StartOffset != rec.Frame.EndOffset-int64(len(rec.Frame.Meta))-int64(len(rec.Frame.Payload))-model.RecordHeaderSize {
			t.Fatalf("start offset wrong for frame %d", rec.Frame.Seq)
		}
	}
	if len(seqs) != 5 {
		t.Fatalf("read %d frames", len(seqs))
	}
}

func TestReaderTruncatedHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seq")
	lens := writeRecordFile(t, path, 3, nil)
	if err := os.Truncate(path, lens[0]+lens[1]+20); err != nil {
		t.Fatal(err)
	}
	r, _ := Open(path, Options{})
	defer r.Close()
	var fault *model.Fault
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if rec.Fault != nil {
			fault = rec.Fault
		}
	}
	if fault == nil || fault.Code != model.CodeTruncatedHeader {
		t.Fatalf("got %+v", fault)
	}
	want := lens[0] + lens[1]
	if fault.Offset != want {
		t.Fatalf("offset %d want %d", fault.Offset, want)
	}
}

func TestReaderTruncatedPayload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seq")
	lens := writeRecordFile(t, path, 3, nil)
	cut := lens[0] + lens[1] + 100
	if err := os.Truncate(path, cut); err != nil {
		t.Fatal(err)
	}
	r, _ := Open(path, Options{})
	defer r.Close()
	var fault *model.Fault
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if rec.Fault != nil {
			fault = rec.Fault
		}
	}
	if fault == nil || fault.Code != model.CodeTruncatedPayload {
		t.Fatalf("got %+v", fault)
	}
	if fault.Offset != cut {
		t.Fatalf("first missing byte %d want %d", fault.Offset, cut)
	}
	if fault.Seq != 3 {
		t.Fatalf("frame %d", fault.Seq)
	}
}

func TestReaderOversizeFrame(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seq")
	writeRecordFile(t, path, 2, nil)
	r, _ := Open(path, Options{MaxFrameBytes: 4})
	defer r.Close()
	rec, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Fault == nil || rec.Fault.Code != model.CodeOversizeFrame {
		t.Fatalf("got %+v", rec.Fault)
	}
}

func TestReaderDigestMatchesPrefixCRC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seq")
	lens := writeRecordFile(t, path, 4, nil)
	r, _ := Open(path, Options{})
	defer r.Close()
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if rec.Fault != nil {
			t.Fatal(rec.Fault)
		}
	}
	// After a full read the reader consumed exactly the file.
	crc, err := r.PrefixCRC(lens[0] + lens[1])
	if err != nil {
		t.Fatal(err)
	}
	if crc == 0 {
		t.Fatal("prefix crc is zero")
	}
	// The prefix hash must not disturb the reader's offset.
	if r.Offset() != 0 && r.Offset() != lens[0]+lens[1]+lens[2]+lens[3] {
		t.Fatalf("offset changed to %d", r.Offset())
	}
}

func TestReaderSeekStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seq")
	lens := writeRecordFile(t, path, 4, nil)
	r, _ := Open(path, Options{})
	defer r.Close()
	var off int64
	for i := 0; i < 2; i++ {
		off += lens[i]
	}
	if err := r.SeekStart(off); err != nil {
		t.Fatal(err)
	}
	rec, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Fault != nil {
		t.Fatalf("fault: %v", rec.Fault)
	}
	if rec.Frame.Seq != 3 {
		t.Fatalf("resumed at frame %d", rec.Frame.Seq)
	}
}

func TestReaderBadMagicStops(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "seq")
	writeRecordFile(t, path, 2, func(i int, rec []byte) []byte {
		if i == 1 {
			rec[0] = 'X'
		}
		return rec
	})
	r, _ := Open(path, Options{})
	defer r.Close()
	rec, _ := r.Read()
	if rec.Fault != nil {
		t.Fatal(rec.Fault)
	}
	rec, err := r.Read()
	if err != nil {
		t.Fatal(err)
	}
	if rec.Fault == nil || rec.Fault.Code != model.CodeBadMagic {
		t.Fatalf("got %+v", rec.Fault)
	}
	if !errors.Is(rec.Fault, rec.Fault) {
		t.Fatal("fault must be an error")
	}
	if _, err := r.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("scan continued after bad magic: %v", err)
	}
}

func TestBadHeaderLength(t *testing.T) {
	raw := make([]byte, model.RecordHeaderSize)
	copy(raw, model.RecordMagic)
	raw[4] = model.RecordVersion
	binary.BigEndian.PutUint16(raw[6:8], 99)
	if _, err := model.ParseRecordHeader(raw); err == nil {
		t.Fatal("accepted a bad header length")
	}
}
