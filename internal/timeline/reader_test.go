package timeline

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/relayteam/relay/internal/media"
)

func validTestRecord(seq int64) Record {
	r := Record{
		Track: "main", Rendition: "720p", Seq: seq,
		PTS90k: seq * 3000, Dur90k: 3000, Clip: "clip-a",
		Payload: "frame-" + strings.Repeat("x", 16),
	}
	r.ComputeCRC()
	return r
}

func encodeLine(t *testing.T, r Record) string {
	t.Helper()
	// Marshal through the same JSON tags used by production fixtures.
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func TestReaderStreamsValidRecordsAndPreservesLocation(t *testing.T) {
	var sb strings.Builder
	for i := int64(0); i < 5; i++ {
		sb.WriteString(encodeLine(t, validTestRecord(i)))
	}
	rs, err := ReadAll(strings.NewReader(sb.String()))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rs) != 5 {
		t.Fatalf("records=%d, want 5", len(rs))
	}
	if rs[4].RecordNo != 5 || rs[4].LineNo != 5 {
		t.Fatalf("location=(%d,%d), want (5,5)", rs[4].RecordNo, rs[4].LineNo)
	}
}

func TestReaderDetectsCRCMismatchAtExactRecord(t *testing.T) {
	good := validTestRecord(1)
	bad := validTestRecord(2)
	bad.Payload += "corrupt" // CRC intentionally left stale.

	input := encodeLine(t, good) + encodeLine(t, bad)
	r := NewReader(strings.NewReader(input))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	returned, err := r.Next()
	if err == nil {
		t.Fatal("expected CRC error")
	}
	if returned == nil {
		t.Fatal("CRC error should retain the partially decoded record")
	}
	var me *media.Error
	if !errors.As(err, &me) || !errors.Is(err, media.ErrCRC32) {
		t.Fatalf("error=%v, want unified CRC error", err)
	}
	if me.Meta.RecordNo != 2 || me.Meta.LineNo != 2 {
		t.Fatalf("location=%+v, want record/line 2", me.Meta)
	}
	if me.Meta.Track != "main" || me.Meta.PTS90k != media.Tick(6000) {
		t.Fatalf("meta=%+v missing track/timestamp", me.Meta)
	}
}

func TestReaderClassifiesTruncatedFinalRecord(t *testing.T) {
	input := encodeLine(t, validTestRecord(0)) + `{"track":"main","seq":1,`
	_, err := ReadAll(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !errors.Is(err, media.ErrTruncatedRecord) {
		t.Fatalf("err=%v, want ErrTruncatedRecord", err)
	}
	if got := FirstBadRecord(err); got != 2 {
		t.Fatalf("first bad record=%d, want 2", got)
	}
}

func TestReaderReturnsEOFAfterDrain(t *testing.T) {
	r := NewReader(strings.NewReader(encodeLine(t, validTestRecord(0))))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("final err=%v, want EOF", err)
	}
}
