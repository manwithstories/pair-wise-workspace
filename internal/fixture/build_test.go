package fixture

import (
	"testing"

	"archivecheck/internal/ebml"
	"archivecheck/internal/fault"
)

func specs() []TrackSpec {
	return []TrackSpec{
		{Number: 1, Kind: ebml.TrackVideo, Codec: "V_VP8", Timescale: 1000, FrameDur: 40, PayloadSize: 64},
		{Number: 2, Kind: ebml.TrackAudio, Codec: "A_PCM/INT/LIT", Timescale: 48000, FrameDur: 1920, PayloadSize: 128},
		{Number: 3, Kind: ebml.TrackSubtitle, Codec: "S_TEXT/UTF8", Timescale: 1000, FrameDur: 1000},
	}
}

func TestBuildParsesCleanly(t *testing.T) {
	plan, err := Build(41, specs(), 40, Defect{TimestampRegressAt: -1, DuplicateAt: -1})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fault.Set{}
	r := ebml.NewReader(byteReaderAt(plan.Data), int64(len(plan.Data)), ebml.Options{Faults: fs})
	br := ebml.NewBlockReaderWithFaults(r, fs)
	if err := br.Scan(); err != nil {
		t.Fatal(err)
	}
	if fs.Len() != 0 {
		t.Fatalf("clean fixture has faults: %v", fs.Faults())
	}
	if len(plan.Frames) != 40+40+10 {
		t.Fatalf("frames=%d", len(plan.Frames))
	}
	if plan.TargetBps <= 0 {
		t.Fatalf("target bps=%d", plan.TargetBps)
	}
}

func TestBuildDeterministic(t *testing.T) {
	a, _ := Build(41, specs(), 20, Defect{TimestampRegressAt: -1, DuplicateAt: -1})
	b, _ := Build(41, specs(), 20, Defect{TimestampRegressAt: -1, DuplicateAt: -1})
	if len(a.Data) != len(b.Data) {
		t.Fatalf("sizes differ %d/%d", len(a.Data), len(b.Data))
	}
	for i := range a.Data {
		if a.Data[i] != b.Data[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
	c, _ := Build(42, specs(), 20, Defect{TimestampRegressAt: -1, DuplicateAt: -1})
	if len(c.Data) == len(a.Data) && string(c.Data) == string(a.Data) {
		t.Fatal("different seeds produced identical data")
	}
}

func TestTruncatedFixtureReportsDamage(t *testing.T) {
	clean, _ := Build(41, specs(), 20, Defect{TimestampRegressAt: -1, DuplicateAt: -1})
	plan, err := Build(41, specs(), 20, Defect{TimestampRegressAt: -1, DuplicateAt: -1, TruncateAt: len(clean.Data) - 40})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Truncated {
		t.Fatal("plan not marked truncated")
	}
	fs := &fault.Set{}
	r := ebml.NewReader(byteReaderAt(plan.Data), int64(len(plan.Data)), ebml.Options{Faults: fs})
	br := ebml.NewBlockReaderWithFaults(r, fs)
	if err := br.Scan(); err != nil {
		t.Fatal(err)
	}
	if fs.Count(fault.KindTruncated) == 0 {
		t.Fatalf("no truncation fault: %v", fs.Kinds())
	}
}

func TestUnknownSizeFixture(t *testing.T) {
	plan, err := Build(41, specs(), 20, Defect{TimestampRegressAt: -1, DuplicateAt: -1, UnknownSizeSegment: true, UnknownSizeCluster: true})
	if err != nil {
		t.Fatal(err)
	}
	fs := &fault.Set{}
	r := ebml.NewReader(byteReaderAt(plan.Data), int64(len(plan.Data)), ebml.Options{Faults: fs})
	br := ebml.NewBlockReaderWithFaults(r, fs)
	if err := br.Scan(); err != nil {
		t.Fatal(err)
	}
	if fs.Len() != 0 {
		t.Fatalf("unknown-size faults: %v", fs.Faults())
	}
	if len(br.Blocks()) != len(plan.Frames) {
		t.Fatalf("blocks=%d frames=%d", len(br.Blocks()), len(plan.Frames))
	}
}

func TestTimestampDefectsPresent(t *testing.T) {
	clean, _ := Build(41, specs(), 16, Defect{TimestampRegressAt: -1, DuplicateAt: -1})
	dirty, _ := Build(41, specs(), 16, Defect{HasRegress: true, TimestampRegressAt: 5, HasDuplicate: true, DuplicateAt: 9})
	if len(clean.Data) == len(dirty.Data) && string(clean.Data) == string(dirty.Data) {
		t.Fatal("defect injection had no effect")
	}
}
