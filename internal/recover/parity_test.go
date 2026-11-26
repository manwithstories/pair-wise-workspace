package recover

import (
	"bytes"
	"testing"

	"audiorestore/internal/model"
	"audiorestore/internal/wav"
)

// generateParityFixture builds a parity carrier and re-reads it into
// blocks plus the manifest needed to construct recovery groups.
func generateParityFixture(t *testing.T, dataPerGroup int, drops, corrupt []int) (
	[]wav.FrameBlock, []Group, *model.ErrorLog,
) {
	t.Helper()
	cfg := wav.GenerateConfig{
		SampleRate: 48000, Channels: 2, BitDepth: 16,
		Frames: 64 * 16, BlockFrames: 64,
		Layout: "stereo_lr", Color: "bt709", Seed: 17,
		DropBlocks: drops, CorruptBlocks: corrupt,
	}
	raw, _, err := wav.GenerateParity(cfg, dataPerGroup)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	rd, err := wav.Open(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	log := model.NewErrorLog()
	blocks, err := rd.ReadBlocks(log)
	if err != nil {
		t.Fatalf("read blocks: %v", err)
	}
	groups := groupsFromManifest(rd.Manifest(), dataPerGroup)
	return blocks, groups, log
}

func groupsFromManifest(m *wav.Manifest, dpg int) []Group {
	parity := map[int]bool{}
	for _, p := range m.ParityAt {
		parity[p] = true
	}
	var out []Group
	for start, gid := 0, 0; start < m.DataBlocks; start, gid = start+dpg, gid+1 {
		end := start + dpg
		if end > m.DataBlocks {
			end = m.DataBlocks
		}
		pIdx := m.DataBlocks + gid
		if !parity[pIdx] {
			continue
		}
		members := make([]int, 0, end-start+1)
		for i := start; i < end; i++ {
			members = append(members, i)
		}
		members = append(members, pIdx)
		out = append(out, Group{ID: gid, Members: members})
	}
	return out
}

func TestSingleLossFullyRecovered(t *testing.T) {
	blocks, groups, log := generateParityFixture(t, 4, []int{3}, nil)
	rc := &Recoverer{Groups: groups, Log: log, Track: "t"}
	out := rc.Repair(blocks)

	if len(out.Unrecovered) != 0 {
		t.Fatalf("expected no unrecovered blocks, got %v", out.Unrecovered)
	}
	if len(out.Recovered) != 1 || out.Recovered[0] != 3 {
		t.Fatalf("expected block 3 recovered, got %v", out.Recovered)
	}
	b := blocks[3]
	if b.Missing || b.Corrupt || b.Truncated {
		t.Fatalf("repaired block still flagged bad: %+v", b)
	}
	if b.CRC != b.WantCRC {
		t.Fatalf("repaired block CRC not verified: %08x != %08x", b.CRC, b.WantCRC)
	}
	if log.Has(model.ErrUnrecovered) {
		t.Fatal("a fully recovered run must not record unrecovered defects")
	}
}

func TestExcessLossKeepsGap(t *testing.T) {
	// Two members of one group lost: parity cannot reconstruct either.
	blocks, groups, log := generateParityFixture(t, 4, []int{0, 1}, nil)
	rc := &Recoverer{Groups: groups, Log: log, Track: "t"}
	out := rc.Repair(blocks)

	if len(out.Recovered) != 0 {
		t.Fatalf("expected no recovery, got %v", out.Recovered)
	}
	got := map[int]bool{}
	for _, idx := range out.Unrecovered {
		got[idx] = true
	}
	if !got[0] || !got[1] {
		t.Fatalf("expected blocks 0 and 1 unrecovered, got %v", out.Unrecovered)
	}
	// Gaps must never be filled with fabricated audio.
	for _, idx := range []int{0, 1} {
		if blocks[idx].Data != nil {
			t.Fatalf("unrecovered block %d carries fabricated data", idx)
		}
	}
	if !log.Has(model.ErrUnrecovered) {
		t.Fatal("excess loss must be reported as an unrecovered defect")
	}
}

func TestCorruptBlockRecoveredAndChecked(t *testing.T) {
	blocks, groups, log := generateParityFixture(t, 4, nil, []int{7})
	if !blocks[7].Corrupt {
		t.Fatal("fixture should mark block 7 corrupt")
	}
	rc := &Recoverer{Groups: groups, Log: log, Track: "t"}
	out := rc.Repair(blocks)
	if len(out.Recovered) != 1 || out.Recovered[0] != 7 {
		t.Fatalf("expected corrupt block 7 recovered, got %v", out.Recovered)
	}
	if blocks[7].CRC != blocks[7].WantCRC {
		t.Fatal("recovered corrupt block failed CRC verification")
	}
}

func TestBuildGroupsLayout(t *testing.T) {
	g := BuildGroups(10, 2) // 2 data + 1 parity per group
	if len(g) != 3 {
		t.Fatalf("expected 3 full groups, got %d", len(g))
	}
	want := []int{0, 1, 2}
	for i, m := range g[0].Members {
		if m != want[i] {
			t.Fatalf("group 0 member %d = %d, want %d", i, m, want[i])
		}
	}
}
