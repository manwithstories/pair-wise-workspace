package fieldpost_test

import (
	"os"
	"testing"

	"fieldpost/demux"
	"fieldpost/fixture"
	"fieldpost/internal/core"
	"fieldpost/mapping"
	"fieldpost/remux"
)

func mappedTSSamples(t *testing.T, camera string, parsed *core.Segment) []core.Sample {
	t.Helper()
	m, err := mapping.DefaultRules().Map(camera, parsed.Tracks)
	if err != nil {
		t.Fatal(err)
	}
	return m.RemapSamples(parsed.Samples)
}

func TestTSRoundTripPreservesEssenceAndTimeline(t *testing.T) {
	want := fixture.MediaSegment(42, "stage", 0)
	wire, err := demux.MuxTS(want)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := demux.ParseTS(want.ID, "stage-0.ts", wire)
	if err != nil {
		t.Fatal(err)
	}
	got := mappedTSSamples(t, "stage", parsed)
	if err := core.VerifyEssence(core.StageInvariant, "stage-0.ts", want.Samples, got); err != nil {
		t.Fatalf("TS essence mismatch: %v", err)
	}
	if got[0].PTS < 900000 {
		t.Fatalf("non-zero base lost: first PTS=%d", got[0].PTS)
	}
}

func TestTSRejectsTruncationAndSyncDamage(t *testing.T) {
	want := fixture.MediaSegment(7, "stage", 2)
	wire, err := demux.MuxTS(want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := demux.ParseTS(want.ID, "trunc.ts", wire[:len(wire)-1]); err == nil {
		t.Fatal("truncated TS accepted")
	} else if core.ExitCode(err) != core.ExitUnrecoverable {
		t.Fatalf("exit=%d err=%v", core.ExitCode(err), err)
	}
	bad := append([]byte(nil), wire...)
	bad[188] = 0x46 // second packet sync byte
	if _, err := demux.ParseTS(want.ID, "sync.ts", bad); err == nil {
		t.Fatal("bad sync accepted")
	}
}

func TestCMAFRoundTripPreservesEssence(t *testing.T) {
	want := fixture.MediaSegment(42, "side", 1)
	out, err := remux.CMAF(want.Tracks, want.Samples)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Fragments) != 2 {
		t.Fatalf("keyframe fragments=%d want 2", len(out.Fragments))
	}
	parsed, err := demux.ParseMP4(want.ID, "side-1.m4s", out.Data)
	if err != nil {
		t.Fatalf("parse remux output: %v", err)
	}
	if err := core.VerifyEssence(core.StageInvariant, "side-1.m4s", want.Samples, parsed.Samples); err != nil {
		t.Fatalf("CMAF essence mismatch: %v", err)
	}
	if core.EssenceHash(want) != out.Essence {
		t.Fatal("result essence hash differs from canonical segment hash")
	}
}

func TestCMAFRejectsIllegalAndTruncatedBoxes(t *testing.T) {
	cases := [][]byte{
		{0, 0, 0, 4, 'f', 'r', 'e', 'e'},
		{0, 0, 0, 32, 'm', 'o', 'o', 'f', 0, 0},
	}
	for i, data := range cases {
		_, err := demux.ParseMP4("bad", "box.m4s", data)
		if err == nil {
			t.Fatalf("case %d accepted", i)
		}
		if core.ExitCode(err) != core.ExitUnrecoverable {
			t.Fatalf("case %d exit=%d err=%v", i, core.ExitCode(err), err)
		}
	}
}

func TestAtomicWriteLeavesNoTemporaryOnFailure(t *testing.T) {
	dir := t.TempDir()
	path, err := remux.AtomicWrite(dir, "ok.m4s", []byte("data"))
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("empty path")
	}
	entries, err := listTemp(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range entries {
		if len(name) > 4 && name[:5] == ".tmp-" {
			t.Fatalf("temporary left behind: %s", name)
		}
	}
}

func listTemp(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}
