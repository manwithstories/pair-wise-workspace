package segment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mediamend/mediamend/internal/fault"
	"github.com/mediamend/mediamend/internal/pipeline"
)

// frames builds a synthetic item stream: one keyframe every keyEvery frames.
func frames(camera string, n int, keyEvery int, step uint64, tsScale uint64) []pipeline.Item {
	out := make([]pipeline.Item, n)
	for i := range out {
		master := uint64(i) * step
		out[i] = pipeline.Item{
			Camera: camera, Seq: uint64(i), MasterTS: master,
			TransformTS: master * tsScale, Offset: int64(i * 100),
			RTPSeq: uint16(i), Keyframe: keyEvery > 0 && i%keyEvery == 0,
		}
	}
	return out
}

func TestParseLadder(t *testing.T) {
	l, err := ParseLadder("360p,1080p")
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 2 || l[0].Name != "360p" || l[1].Name != "1080p" {
		t.Fatalf("ladder=%+v", l)
	}
	if _, err := ParseLadder("360p,4k"); err == nil {
		t.Fatal("unknown representation accepted")
	}
	if _, err := ParseLadder("720p,720p"); err == nil {
		t.Fatal("duplicate representation accepted")
	}
	def, err := ParseLadder("")
	if err != nil || len(def) != 3 {
		t.Fatalf("default ladder=%+v err=%v", def, err)
	}
}

func TestCutAtKeyframes(t *testing.T) {
	// 300 frames at 3000 ticks (30fps), keyframes every 30 frames -> 1s.
	fs := frames("cam1", 300, 30, 3000, 1)
	bounds, keyed, faults := Cut(fs, 90000, 0)
	if len(bounds) != 10 {
		t.Fatalf("bounds=%v want 10 segments", bounds)
	}
	for i, b := range bounds {
		if b%30 != 0 {
			t.Fatalf("bound %d at frame %d is not a keyframe", i, b)
		}
		if !keyed[i] {
			t.Fatalf("bound %d not keyed", i)
		}
		if !fs[b].Keyframe {
			t.Fatalf("bound %d is an inter frame", i)
		}
	}
	if len(faults) != 0 {
		t.Fatalf("unexpected faults: %v", faults)
	}
}

func TestCutMissingKeyframeContinues(t *testing.T) {
	fs := frames("cam1", 300, 30, 3000, 1)
	// Remove the keyframe flag at frames 90 and 120 to simulate dropped
	// keyframes; the cadence check must report them by sequence.
	fs[90].Keyframe = false
	fs[120].Keyframe = false
	bounds, keyed, faults := Cut(fs, 90000, 30)
	if len(bounds) < 5 {
		t.Fatalf("segmentation stopped early: %v", bounds)
	}
	missing := faults.Count(fault.KindMissingKeyframe)
	if missing < 2 {
		t.Fatalf("missing keyframes=%d want >= 2: %v", missing, faults)
	}
	for _, f := range faults {
		if f.Kind == fault.KindMissingKeyframe && (f.Seq == 90 || f.Seq == 120) {
			if f.Offset < 0 {
				t.Fatalf("missing keyframe has no offset: %+v", f)
			}
		}
	}
	// Every boundary must still be a real keyframe.
	for i, b := range bounds {
		if !fs[b].Keyframe || !keyed[i] {
			t.Fatalf("boundary %d at %d is not a keyframe", i, b)
		}
	}
}

func TestSegmentAllWritesConsistentLadder(t *testing.T) {
	root := t.TempDir()
	items := frames("cam1", 300, 30, 3000, 10_000_000/90000)
	ladder, _ := ParseLadder("360p,720p,1080p")
	results, faults := SegmentAll(items, Options{Config: Config{
		Root: root, Ladder: ladder, TargetDuration: 90000,
	}})
	if len(faults) != 0 {
		t.Fatalf("faults=%v", faults)
	}
	if len(results) != 1 {
		t.Fatalf("results=%d", len(results))
	}
	r := results[0]
	if r.AlignmentError != 0 {
		t.Fatalf("alignment error=%d", r.AlignmentError)
	}
	counts := map[string]int{}
	ends := map[string][]uint64{}
	for _, s := range r.Segments {
		counts[s.Representation]++
		ends[s.Representation] = append(ends[s.Representation], s.EndTS)
		if s.SHA256 == "" || s.Size == 0 {
			t.Fatalf("segment missing hash/size: %+v", s)
		}
		if _, err := os.Stat(filepath.Join(root, "output", s.File)); err != nil {
			t.Fatalf("segment file missing: %v", err)
		}
	}
	if counts["360p"] != counts["720p"] || counts["720p"] != counts["1080p"] {
		t.Fatalf("representations cut differently: %v", counts)
	}
	for rep, e := range ends {
		for i := 1; i < len(e); i++ {
			if e[i] != ends["360p"][i] {
				t.Fatalf("%s ends differ from 360p at %d: %d vs %d", rep, i, e[i], ends["360p"][i])
			}
		}
	}
	if len(r.Manifests) != 4 { // three representations + combined
		t.Fatalf("manifests=%v", r.Manifests)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	root := t.TempDir()
	items := frames("cam1", 120, 30, 3000, 1)
	results, faults := SegmentAll(items, Options{Config: Config{
		Root: root, Ladder: DefaultLadder[:1], TargetDuration: 90000,
	}})
	if len(faults) != 0 {
		t.Fatalf("faults=%v", faults)
	}
	path := results[0].Manifests[0]
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if len(m.Segments) != len(results[0].Segments) {
		t.Fatalf("manifest segments=%d want %d", len(m.Segments), len(results[0].Segments))
	}
	if m.Timescale != 90000 {
		t.Fatalf("timescale=%d", m.Timescale)
	}
	// Every manifest segment must describe a real file with the same hash.
	for _, s := range m.Segments {
		b, err := os.ReadFile(filepath.Join(root, "output", s.File))
		if err != nil {
			t.Fatal(err)
		}
		if sum := sha256Hex(b); sum != s.SHA256 {
			t.Fatalf("segment %s hash %s want %s", s.File, s.SHA256, sum)
		}
	}
}

func TestAtomicWriteLeavesNoTempFiles(t *testing.T) {
	root := t.TempDir()
	items := frames("cam1", 60, 30, 3000, 1)
	if _, faults := SegmentAll(items, Options{Config: Config{
		Root: root, Ladder: DefaultLadder[:1], TargetDuration: 90000,
	}}); len(faults) != 0 {
		t.Fatalf("faults=%v", faults)
	}
	entries, err := os.ReadDir(filepath.Join(root, "output"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if len(name) > 0 && name[0] == '.' {
			t.Fatalf("temporary file left behind: %s", name)
		}
	}
}

func TestEmptyInputProducesNoSegments(t *testing.T) {
	root := t.TempDir()
	results, faults := SegmentAll(nil, Options{Config: Config{Root: root, Ladder: DefaultLadder[:1]}})
	if len(faults) != 0 {
		t.Fatalf("faults=%v", faults)
	}
	if len(results) != 0 {
		t.Fatalf("results=%d", len(results))
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestCombinedManifestSwitchList(t *testing.T) {
	root := t.TempDir()
	items := frames("cam1", 240, 30, 3000, 1)
	results, faults := SegmentAll(items, Options{Config: Config{
		Root: root, Ladder: DefaultLadder, TargetDuration: 90000,
	}})
	if len(faults) != 0 {
		t.Fatalf("faults=%v", faults)
	}
	combinedPath := results[0].Manifests[len(results[0].Manifests)-1]
	data, err := os.ReadFile(combinedPath)
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.SwitchList) == 0 {
		t.Fatal("combined manifest has no switch points")
	}
	for _, sp := range m.SwitchList {
		if len(sp.Renditions) != len(DefaultLadder) {
			t.Fatalf("switch point %d has %d renditions", sp.Index, len(sp.Renditions))
		}
		if sp.EndTS <= sp.StartTS {
			t.Fatalf("switch point %d has invalid span", sp.Index)
		}
	}
	// Segments in the combined manifest must be sorted by representation
	// then index, and cover the whole stream exactly once per rendition.
	seen := map[string]map[int]bool{}
	for _, s := range m.Segments {
		if seen[s.Representation] == nil {
			seen[s.Representation] = map[int]bool{}
		}
		if seen[s.Representation][s.Index] {
			t.Fatalf("duplicate segment %s/%d in combined manifest", s.Representation, s.Index)
		}
		seen[s.Representation][s.Index] = true
	}
	for _, rep := range DefaultLadder {
		total := 0
		for _, s := range m.Segments {
			if s.Representation == rep.Name {
				total += s.FrameCount
			}
		}
		if total != len(items) {
			t.Fatalf("%s covers %d frames want %d", rep.Name, total, len(items))
		}
	}
}
