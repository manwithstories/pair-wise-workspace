package ingest

import (
	"bytes"
	"io"
	"testing"

	"mediameld/internal/fixture"
	"mediameld/internal/model"
)

func generateRTP(t *testing.T) []byte {
	t.Helper()
	b, err := fixture.Generate(t.TempDir(), fixture.Options{
		Seed: 17, DriftPPM: 85, WrapAt: 4_294_960_000, VFR: []int64{24, 25, 23},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := openFile(t, b.RTPStream)
	raw, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDemuxRTPCountsAndDuplicates(t *testing.T) {
	raw := generateRTP(t)
	plan := fixture.PlanDelivery(48, 12)
	res, err := DemuxRTP(bytes.NewReader(raw), "cam-video")
	if err != nil {
		t.Fatalf("demux: %v", err)
	}
	// 48 source frames, one duplicate -> 49 packets.
	want := 48
	if plan.Duplicate >= 0 {
		want++
	}
	if plan.Missing >= 0 {
		want--
	}
	if len(res.Packets) != want {
		t.Fatalf("packets=%d want %d", len(res.Packets), want)
	}
	seen := map[uint16]int{}
	for _, p := range res.Packets {
		seen[p.Seq]++
	}
	dups := 0
	for _, n := range seen {
		if n > 1 {
			dups++
		}
	}
	if dups != 1 {
		t.Fatalf("duplicate sequences=%d want 1", dups)
	}
	// At least one timestamp must have wrapped below the start value.
	wrapped := false
	for _, p := range res.Packets {
		if p.Timestamp < 1000 {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("no 32-bit wrap observed in fixture timestamps")
	}
}

func TestDemuxRTPBadFraming(t *testing.T) {
	bad := []byte{0x00, 0x00, 0x00, 0x40, 0x80}
	_, err := DemuxRTP(bytes.NewReader(bad), "x")
	if err == nil {
		t.Fatal("overrun framing accepted")
	}
	if code, _ := model.CodeOf(err); code != model.CodeRTPPacket {
		t.Fatalf("code=%v", code)
	}
	if model.ExitCode(err) != 2 {
		t.Fatalf("exit=%d want 2", model.ExitCode(err))
	}
}

func TestDemuxRTPEmpty(t *testing.T) {
	_, err := DemuxRTP(bytes.NewReader(nil), "x")
	if err == nil {
		t.Fatal("empty stream accepted")
	}
}
