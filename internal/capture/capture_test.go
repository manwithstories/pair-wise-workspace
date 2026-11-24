package capture

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/pion/rtp"

	"mediarig/internal/media"
)

func TestFramingRoundTripAndTruncatedHeader(t *testing.T) {
	var buf bytes.Buffer
	payload := make([]byte, 12)
	payload[0] = 0x80
	if err := WriteRecord(&buf, RecordRTP, payload); err != nil {
		t.Fatal(err)
	}
	rec, err := ReadAll(&buf)
	if err != nil || len(rec) != 1 {
		t.Fatalf("ReadAll records=%d err=%v", len(rec), err)
	}

	trunc := []byte{RecordRTP, 0, 0}
	r := NewReader(bytes.NewReader(trunc))
	_, err = r.Next()
	var re *media.Error
	if !errors.As(err, &re) || re.Code != media.ExitCorrupt || re.Stage != media.StageFraming {
		t.Fatalf("expected framing corrupt, got %#v", err)
	}
	if re.RecordIndex == nil || *re.RecordIndex != 0 {
		t.Fatalf("expected record index 0, got %#v", re.RecordIndex)
	}
}

func TestFramingRejectsOversizedAndIllegalType(t *testing.T) {
	var hdr [5]byte
	hdr[0] = RecordRTP
	binary.BigEndian.PutUint32(hdr[1:], MaxRecordPayload+1)
	_, err := ReadAll(bytes.NewReader(hdr[:]))
	var re *media.Error
	if !errors.As(err, &re) || re.Code != 2 {
		t.Fatalf("expected policy error for oversized record, got %v", err)
	}

	var buf bytes.Buffer
	_ = WriteRecord(&buf, 99, nil)
	// WriteRecord rejects before writing; craft an illegal type directly.
	buf.Reset()
	buf.Write([]byte{99, 0, 0, 0, 0})
	_, err = ReadAll(&buf)
	if !errors.As(err, &re) || re.Stage != media.StageFraming {
		t.Fatalf("expected illegal record type, got %v", err)
	}
}

func TestExtendWraparound(t *testing.T) {
	seq := ExtendUint16(0, 65534, false)
	seq = ExtendUint16(seq, 65535, true)
	seq = ExtendUint16(seq, 0, true)
	seq = ExtendUint16(seq, 1, true)
	if seq != 65537 {
		t.Fatalf("sequence wrap = %d, want 65537", seq)
	}
	ts := ExtendUint32(0, 0xfffffffe, false)
	ts = ExtendUint32(ts, 1, true)
	if ts != 0x100000001 {
		t.Fatalf("timestamp wrap = %d", ts)
	}
}

func rtpRecord(t *testing.T, pt uint8, seq uint16, ts uint32, ssrc uint32, payload []byte) Record {
	t.Helper()
	b, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc},
		Payload: payload,
	}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return Record{Type: RecordRTP, Payload: b}
}

func TestAssemblerReorderDuplicateLossAndWrap(t *testing.T) {
	asm := NewAssembler(DefaultPayloads())
	const ssrc = uint32(42)
	// Logical extended sequences 65534..65539 with seq 65538 missing.
	// Feed 65537 before 65536 to create one reorder event.
	seqs := []uint16{65534, 65535, 1, 0, 3, 4, 5}
	for i, seq := range seqs {
		rec := rtpRecord(t, 96, seq, 1000+uint32(i)*960, ssrc, make([]byte, 960*4))
		if err := asm.IngestRecords([]Record{rec}); err != nil {
			t.Fatal(err)
		}
	}
	// Duplicate the last packet.
	dup := rtpRecord(t, 96, 5, 1000+6*960, ssrc, make([]byte, 960*4))
	if err := asm.IngestRecords([]Record{dup}); err != nil {
		t.Fatal(err)
	}
	if err := asm.Finish(); err != nil {
		t.Fatal(err)
	}
	tr := asm.Tracks[ssrc]
	if tr.Stats.ReorderedPackets != 1 {
		t.Fatalf("reordered=%d want1", tr.Stats.ReorderedPackets)
	}
	if tr.Stats.DuplicatePackets != 1 {
		t.Fatalf("duplicates=%d want1", tr.Stats.DuplicatePackets)
	}
	if tr.Stats.MissingPackets != 1 || len(tr.Gaps) != 1 {
		t.Fatalf("missing=%d gaps=%d", tr.Stats.MissingPackets, len(tr.Gaps))
	}
	if tr.Packets[0].ExtSequence != 65534 || tr.Packets[len(tr.Packets)-1].ExtSequence != 65541 {
		t.Fatalf("unexpected ext range %d..%d", tr.Packets[0].ExtSequence, tr.Packets[len(tr.Packets)-1].ExtSequence)
	}
	if n := tr.GapSamples(tr.Gaps[0]); n != 960 {
		t.Fatalf("gap samples=%d want960", n)
	}
}

func TestRTCPAndClockAlignment(t *testing.T) {
	asm := NewAssembler(DefaultPayloads())
	const ssrc = uint32(77)
	for i, seq := range []uint16{10, 11} {
		rec := rtpRecord(t, 96, seq, 5000+uint32(i)*960, ssrc, make([]byte, 3840))
		if err := asm.IngestRecords([]Record{rec}); err != nil {
			t.Fatal(err)
		}
	}
	sr := make([]byte, 28)
	sr[0] = 0x80
	sr[1] = 200
	binary.BigEndian.PutUint16(sr[2:4], 6)
	binary.BigEndian.PutUint32(sr[4:8], ssrc)
	binary.BigEndian.PutUint32(sr[8:12], 100)
	binary.BigEndian.PutUint32(sr[16:20], 5000)
	recs, err := ParseSenderReports(Record{Type: RecordRTCP, Payload: sr})
	if err != nil || len(recs) != 1 {
		t.Fatalf("SR parse reports=%d err=%v", len(recs), err)
	}
	asm.reports[ssrc] = recs
	asm.Tracks[ssrc].reports = recs
	if err := asm.Finish(); err != nil {
		t.Fatal(err)
	}
	clock, err := AlignClocks(asm)
	if err != nil {
		t.Fatal(err)
	}
	rel, err := clock.Relative(ssrc, 5960)
	if err != nil {
		t.Fatal(err)
	}
	if got := rel.Float64(); got < 0.019 || got > 0.021 {
		t.Fatalf("relative clock=%v want ~0.02s", got)
	}
}

func TestReaderEOFAtCleanBoundary(t *testing.T) {
	r := NewReader(bytes.NewReader(nil))
	_, err := r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("empty input err=%v want EOF", err)
	}
}
