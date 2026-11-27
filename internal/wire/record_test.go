package wire

import (
	"errors"
	"hash/crc32"
	"io"
	"testing"

	"relayaudit/internal/clock"
	"relayaudit/internal/fault"
)

func mkRecord(track uint8, seq uint32, pts clock.Stamp, body []byte) *Record {
	return &Record{Track: track, Seq: seq, PTS: pts, Payload: body, CRC: PayloadCRC(body)}
}

func encode(t *testing.T, recs ...*Record) []byte {
	t.Helper()
	var out []byte
	for _, r := range recs {
		out = r.Marshal(out)
	}
	return out
}

func readAll(t *testing.T, buf []byte) ([]*Record, error) {
	t.Helper()
	rd := NewRecordReader(buf, 0, int64(len(buf)))
	var got []*Record
	for {
		r, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		got = append(got, r)
	}
}

func TestRecordRoundTripPreservesFieldsAndOffsets(t *testing.T) {
	a := mkRecord(1, 7, clock.Stamp(4294967200), []byte("alpha payload"))
	b := mkRecord(3, 8, clock.Stamp(12), []byte("bravo payload longer"))
	buf := encode(t, a, b)

	got, err := readAll(t, buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2", len(got))
	}
	if got[0].Track != 1 || got[0].Seq != 7 || got[0].PTS != a.PTS {
		t.Fatalf("first record fields wrong: %+v", got[0])
	}
	if string(got[0].Payload) != "alpha payload" || string(got[1].Payload) != "bravo payload longer" {
		t.Fatalf("payload mismatch")
	}
	if got[0].Offset != 0 {
		t.Fatalf("first offset = %d, want 0", got[0].Offset)
	}
	if want := int64(len(buf) - len(b.Marshal(nil))); got[1].Offset != want {
		t.Fatalf("second offset = %d, want %d", got[1].Offset, want)
	}
	if !got[0].ValidCRC() || !got[1].ValidCRC() {
		t.Fatalf("round-tripped CRCs not valid")
	}
}

func TestRecordCRCMismatchReportsRecordOffset(t *testing.T) {
	buf := encode(t, mkRecord(2, 1, 100, []byte("hello world")))
	buf[RecordHeaderSize+3] ^= 0xff

	_, err := readAll(t, buf)
	f, ok := fault.As(err)
	if !ok {
		t.Fatalf("error %v is not a fault", err)
	}
	if f.Kind() != fault.CRC {
		t.Fatalf("kind = %v, want crc", f.Kind())
	}
	if f.Offset() != 0 {
		t.Fatalf("offset = %d, want 0", f.Offset())
	}
	if f.Track() != 2 {
		t.Fatalf("track = %d, want 2", f.Track())
	}
}

func TestRecordTruncatedHeaderKeepsOffset(t *testing.T) {
	buf := encode(t, mkRecord(1, 0, 0, []byte("abcdef")))
	cut := buf[:len(buf)-3]
	_, err := readAll(t, cut)
	f, ok := fault.As(err)
	if !ok || f.Kind() != fault.Truncated {
		t.Fatalf("err = %v, want truncated fault", err)
	}
	if f.Offset() != 0 {
		t.Fatalf("offset = %d, want 0", f.Offset())
	}
}

func TestRecordTruncatedPayloadIsFaultNotPanic(t *testing.T) {
	buf := encode(t, mkRecord(1, 0, 0, make([]byte, 512)))
	buf = buf[:RecordHeaderSize+100]
	_, err := readAll(t, buf)
	f, _ := fault.As(err)
	if f == nil || f.Kind() != fault.Truncated {
		t.Fatalf("err = %v, want truncated", err)
	}
}

func TestRecordIllegalPayloadLengthIsLimitFault(t *testing.T) {
	buf := make([]byte, RecordOverhead)
	buf[0] = 1
	// declared length just over the 4 MiB ceiling
	buf[9], buf[10], buf[11], buf[12] = 0x00, 0x40, 0x00, 0x01
	_, err := readAll(t, buf)
	f, _ := fault.As(err)
	if f == nil || f.Kind() != fault.Limit {
		t.Fatalf("err = %v, want limit", err)
	}
}

func TestRecordZeroTrackRejected(t *testing.T) {
	buf := encode(t, mkRecord(0, 0, 0, []byte("x")))
	_, err := readAll(t, buf)
	f, _ := fault.As(err)
	if f == nil || f.Kind() != fault.Length {
		t.Fatalf("err = %v, want length", err)
	}
}

func TestRecordOverTrackCeilingRejected(t *testing.T) {
	buf := encode(t, mkRecord(MaxTracks+1, 0, 0, []byte("x")))
	_, err := readAll(t, buf)
	if !IsFaultKind(err, fault.Length) {
		t.Fatalf("err = %v, want length fault", err)
	}
}

func TestResyncFindsNextGoodBoundary(t *testing.T) {
	good := mkRecord(1, 3, 555, []byte("recoverable payload bytes"))
	buf := append([]byte{0xde, 0xad, 0xbe, 0xef, 0x00}, good.Marshal(nil)...)
	rd := NewRecordReader(buf, 0, int64(len(buf)))
	if _, err := rd.Next(); err == nil {
		t.Fatalf("expected damage at offset 0")
	}
	next, err := rd.Resync(0)
	if err != nil {
		t.Fatalf("resync: %v", err)
	}
	if next != 5 {
		t.Fatalf("resync landed at %d, want 5", next)
	}
	rd.off = next
	rec, err := rd.Next()
	if err != nil {
		t.Fatalf("read after resync: %v", err)
	}
	if string(rec.Payload) != "recoverable payload bytes" {
		t.Fatalf("wrong payload after resync")
	}
}

func TestResyncGivesUpAtEndWithoutValidRecord(t *testing.T) {
	buf := []byte("no records here at all, just text")
	rd := NewRecordReader(buf, 0, int64(len(buf)))
	if _, err := rd.Resync(0); err == nil {
		t.Fatalf("resync should fail on text input")
	}
}

func TestResyncBudgetIsBounded(t *testing.T) {
	buf := make([]byte, MaxScanBytes+64)
	rd := NewRecordReader(buf, 0, int64(len(buf)))
	_, err := rd.Resync(0)
	f, _ := fault.As(err)
	if f == nil || f.Kind() != fault.Limit {
		t.Fatalf("err = %v, want limit fault", err)
	}
}

func TestPayloadCRCUsesCastagnoli(t *testing.T) {
	body := []byte("checksum me")
	if got, want := PayloadCRC(body), crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)); got != want {
		t.Fatalf("crc = %08x, want %08x", got, want)
	}
}
