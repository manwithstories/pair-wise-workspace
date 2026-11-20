package index

import (
	"math/rand"
	"testing"

	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
	"github.com/mediamend/mediamend/internal/timeline"
)

func buildReport(t *testing.T, n int) *timeline.Report {
	t.Helper()
	clock := model.Clock{TrackID: "v1", Rate: 90000, AnchorSeq: 0, FrameStep: 3000}
	var frames []*ingest.Frame
	for i := 0; i < n; i++ {
		payload := make([]byte, 16+i%4)
		for j := range payload {
			payload[j] = byte(i + j)
		}
		frames = append(frames, &ingest.Frame{
			TrackID: "v1", Seq: uint64(i), DeviceTS: uint64(i * 3000),
			Rate: 90000, Payload: payload, ChecksumOK: true, Offset: int64(i * 64),
		})
	}
	return timeline.Repair(frames, clock, "v1.jsonl")
}

func TestBuildAndVerify(t *testing.T) {
	dir := t.TempDir()
	rep := buildReport(t, 120)
	idx, faults := Build(rep, dir, 10)
	if model.HasFatal(faults) {
		t.Fatalf("build faults: %v", faults)
	}
	if idx.FirstSeq != 0 || idx.LastSeq != 119 {
		t.Fatalf("bounds=%d..%d", idx.FirstSeq, idx.LastSeq)
	}
	if vf := Verify(idx.File, idx); model.HasFatal(vf) {
		t.Fatalf("verify faults: %v", vf)
	}
	checked, sf := RandomSeeks(idx, 50, rand.New(rand.NewSource(42)))
	if checked != 50 || model.HasFatal(sf) {
		t.Fatalf("seeks checked=%d faults=%v", checked, sf)
	}
}

func TestNonMonotonicRejected(t *testing.T) {
	dir := t.TempDir()
	rep := buildReport(t, 10)
	// Force a backwards master timestamp after repair.
	rep.Frames[5].MasterTS = rep.Frames[3].MasterTS
	_, faults := Build(rep, dir, 5)
	var mono bool
	for _, f := range faults {
		if f.Class == model.FaultIndexMonotonic {
			mono = true
			if f.Location.Seq != rep.Frames[5].Frame.Seq {
				t.Fatalf("fault not traced to source frame: %+v", f.Location)
			}
		}
	}
	if !mono {
		t.Fatalf("non-monotonic input accepted: %v", faults)
	}
}

func TestSeekDeterministicAcrossRNGSeeds(t *testing.T) {
	dir := t.TempDir()
	rep := buildReport(t, 60)
	idx, faults := Build(rep, dir, 15)
	if model.HasFatal(faults) {
		t.Fatal(faults)
	}
	c1, f1 := RandomSeeks(idx, 30, rand.New(rand.NewSource(7)))
	c2, f2 := RandomSeeks(idx, 30, rand.New(rand.NewSource(7)))
	if c1 != c2 || model.HasFatal(f1) || model.HasFatal(f2) {
		t.Fatalf("reproducibility broken: %d/%d %v %v", c1, c2, f1, f2)
	}
}
