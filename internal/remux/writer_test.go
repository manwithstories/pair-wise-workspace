package remux

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"mediadeliver/internal/model"
)

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func mkPackets() []*model.Packet {
	out := make([]*model.Packet, 6)
	for i := range out {
		tid := uint32(1)
		if i >= 3 {
			tid = 2
		}
		out[i] = &model.Packet{
			SourceID: "cam1", TrackID: tid, Seq: int64(i % 3),
			DTS: int64(i * 100), PTS: int64(i * 100), Duration: 100,
			Timescale: 90000, Payload: []byte{byte(i), byte(i + 1), byte(i + 2)},
		}
	}
	return out
}

func TestLosslessWriteAndProvenance(t *testing.T) {
	var buf bytes.Buffer
	sw := NewSegmentWriter(0, "segA.mp4", nil, &buf, model.DefaultLimits())
	pkts := mkPackets()
	for _, p := range pkts {
		if err := sw.WritePacket(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	rec, err := sw.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Output must be the exact concatenation of payloads (lossless copy).
	var want bytes.Buffer
	for _, p := range pkts {
		want.Write(p.Payload)
	}
	if !bytes.Equal(buf.Bytes(), want.Bytes()) {
		t.Fatal("output is not a byte-for-byte copy of input payloads")
	}
	if len(rec.SHA256Hex) != 64 {
		t.Fatalf("digest must be 64 hex chars, got %d", len(rec.SHA256Hex))
	}
	if rec.PacketCount != 6 || rec.Bytes != int64(want.Len()) {
		t.Fatalf("record counts wrong: %+v", rec)
	}
	if len(rec.Ranges) != 2 {
		t.Fatalf("want 2 source ranges (2 tracks), got %d", len(rec.Ranges))
	}
	for _, r := range rec.Ranges {
		if r.FirstSeq != 0 || r.LastSeq != 2 || r.Packets != 3 {
			t.Fatalf("range bounds wrong: %+v", r)
		}
	}
}

// failWriter injects an output write error after N successful writes.
type failWriter struct {
	failAfter int
	n         int
}

func (f *failWriter) Write(p []byte) (int, error) {
	if f.n >= f.failAfter {
		return 0, errors.New("simulated disk full")
	}
	f.n++
	return len(p), nil
}

func TestWriteFailurePropagates(t *testing.T) {
	fw := &failWriter{failAfter: 2}
	sw := NewSegmentWriter(0, "segA.mp4", nil, fw, model.DefaultLimits())
	var got error
	for _, p := range mkPackets() {
		if err := sw.WritePacket(context.Background(), p); err != nil {
			got = err
			break
		}
	}
	if model.KindOf(got) != model.KindIO {
		t.Fatalf("want KindIO on write failure, got %v", got)
	}
	if model.PositionOf(got) < 0 {
		t.Fatalf("write failure must carry packet position: %v", got)
	}
}

func TestOversizedRejectedAtWrite(t *testing.T) {
	var buf bytes.Buffer
	lim := model.DefaultLimits()
	lim.MaxPacketBytes = 2
	sw := NewSegmentWriter(0, "seg.mp4", nil, &buf, lim)
	err := sw.WritePacket(context.Background(), &model.Packet{
		Seq: 0, Payload: []byte{1, 2, 3},
	})
	if model.KindOf(err) != model.KindLimitExceeded {
		t.Fatalf("want KindLimitExceeded, got %v", err)
	}
}

func TestCancelStopsWrite(t *testing.T) {
	var buf bytes.Buffer
	sw := NewSegmentWriter(0, "seg.mp4", nil, &buf, model.DefaultLimits())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sw.WritePacket(ctx, &model.Packet{Seq: 0, Payload: []byte{1}})
	if model.KindOf(err) != model.KindCancelled {
		t.Fatalf("want KindCancelled, got %v", err)
	}
}

func TestDigestStableAcrossRuns(t *testing.T) {
	hashes := make([]string, 2)
	for run := 0; run < 2; run++ {
		var buf bytes.Buffer
		sw := NewSegmentWriter(0, "seg.mp4", nil, &buf, model.DefaultLimits())
		for _, p := range mkPackets() {
			if err := sw.WritePacket(context.Background(), p); err != nil {
				t.Fatal(err)
			}
		}
		rec, err := sw.Close()
		if err != nil {
			t.Fatal(err)
		}
		hashes[run] = rec.SHA256Hex
	}
	if hashes[0] != hashes[1] || !isHex64(hashes[0]) {
		t.Fatalf("identical input must yield identical hex digest: %v", hashes)
	}
}
