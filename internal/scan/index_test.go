package scan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaverify/internal/fault"
	"mediaverify/internal/manifest"
)

const mpd = `<?xml version="1.0"?>
<MPD><Period><AdaptationSet>
  <Representation id="a1" bandwidth="128000">
    <SegmentTemplate media="seg_$Number$.m4s" initialization="init.mp4" timescale="1000">
      <SegmentTimeline><S t="0" d="1000" r="1"/></SegmentTimeline>
    </SegmentTemplate>
  </Representation>
</AdaptationSet></Period></MPD>`

func setup(t *testing.T) (string, *Index) {
	t.Helper()
	dir := t.TempDir()
	mp := filepath.Join(dir, "prog.mpd")
	if err := os.WriteFile(mp, []byte(mpd), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"seg_1.m4s", "seg_2.m4s"} {
		if err := os.WriteFile(filepath.Join(dir, s), []byte("payload-"+s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := manifest.ParseDASH(mp, strings.NewReader(mpd))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Build(prog, dir, mp)
	if err != nil {
		t.Fatal(err)
	}
	return dir, idx
}

func TestBuildAndVerify(t *testing.T) {
	dir, idx := setup(t)
	if len(idx.Segments) != 2 {
		t.Fatalf("segments=%d", len(idx.Segments))
	}
	if err := idx.Verify(dir); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// 重复构建结果一致（确定性）
	prog, _ := manifest.ParseDASH(idx.ManifestPath, strings.NewReader(mpd))
	idx2, err := Build(prog, dir, idx.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if idx.InputDigest != idx2.InputDigest || idx.Checksum != idx2.Checksum {
		t.Fatal("rebuild not deterministic")
	}
}

func TestVerifyRejectsChangedInput(t *testing.T) {
	dir, idx := setup(t)
	if err := os.WriteFile(filepath.Join(dir, "seg_1.m4s"), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := idx.Verify(dir)
	fe, ok := fault.As(err)
	if !ok || fe.Recovery != fault.Rescan {
		t.Fatalf("err=%v", err)
	}
}

func TestSaveLoadAndTruncation(t *testing.T) {
	dir, idx := setup(t)
	ip := filepath.Join(dir, "index.json")
	if err := idx.Save(ip); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ip)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputDigest != idx.InputDigest {
		t.Fatal("roundtrip mismatch")
	}
	// 截断索引 → Rescan 策略错误
	b, _ := os.ReadFile(ip)
	if err := os.WriteFile(ip, b[:len(b)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(ip)
	if fe, ok := fault.As(err); !ok || fe.Recovery != fault.Rescan {
		t.Fatalf("err=%v", err)
	}
}

func TestBuildMissingSegment(t *testing.T) {
	dir, _ := setup(t)
	if err := os.Remove(filepath.Join(dir, "seg_2.m4s")); err != nil {
		t.Fatal(err)
	}
	prog, _ := manifest.ParseDASH("prog.mpd", strings.NewReader(mpd))
	_, err := Build(prog, dir, filepath.Join(dir, "prog.mpd"))
	fe, ok := fault.As(err)
	if !ok || fe.Segment != 1 {
		t.Fatalf("err=%v", err)
	}
}
