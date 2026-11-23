package fixture

import (
	"path/filepath"
	"testing"

	"archive.local/archive/internal/rtp"
)

func TestGeneratedTraceRecoversInjectedDefects(t *testing.T) {
	dir := t.TempDir()
	tr, err := GenerateTrace(filepath.Join(dir, "trace.bin"), TracePlan{
		Packets: 400, Payload: 64,
		DupEvery: 17, ReorderEvery: 23,
		Bursts: []Burst{{At: 100, Count: 6}, {At: 260, Count: 9}},
	}, 41)
	if err != nil {
		t.Fatal(err)
	}
	// Count how many packets were actually recorded versus delivered.
	var track = rtp.NewTrack(tr.SSRC)
	for _, p := range tr.Packets {
		track.Push(p)
	}
	res := track.Close()
	if res.Duplicate == 0 {
		t.Fatalf("tracker saw no duplicates; injected %d", tr.InjectedDup)
	}
	if res.Reordered == 0 {
		t.Fatalf("tracker saw no reordering; injected %d", tr.InjectedReorder)
	}
	if res.Lost == 0 || len(res.Gaps) == 0 {
		t.Fatalf("tracker saw no loss; injected %d", tr.InjectedLoss)
	}
	if res.ConfirmedHigh >= res.MaxSeq {
		t.Fatalf("confirmed progress overruns a real gap: %d >= %d", res.ConfirmedHigh, res.MaxSeq)
	}
}

func TestTraceRoundTripsThroughDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "trace.gob")
	in, err := GenerateTrace(p, TracePlan{Packets: 50, Payload: 32, DupEvery: 5}, 7)
	if err != nil {
		t.Fatal(err)
	}
	out, err := ReadTrace(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Packets) != len(in.Packets) || out.InjectedDup != in.InjectedDup {
		t.Fatalf("round trip mismatch: %d vs %d", len(out.Packets), len(in.Packets))
	}
}

func TestGeneratedTraceDeterministic(t *testing.T) {
	dir := t.TempDir()
	a, err := GenerateTrace(filepath.Join(dir, "a.bin"), TracePlan{Packets: 120, DupEvery: 9, ReorderEvery: 11}, 41)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateTrace(filepath.Join(dir, "b.bin"), TracePlan{Packets: 120, DupEvery: 9, ReorderEvery: 11}, 41)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Packets) != len(b.Packets) {
		t.Fatalf("count differs: %d vs %d", len(a.Packets), len(b.Packets))
	}
	for i := range a.Packets {
		if a.Packets[i].Seq != b.Packets[i].Seq || a.Packets[i].Timestamp != b.Packets[i].Timestamp {
			t.Fatalf("packet %d differs", i)
		}
	}
}
