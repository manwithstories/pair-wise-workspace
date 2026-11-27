package probe

import (
	"os"
	"path/filepath"
	"testing"

	"bcast/internal/fault"
	"bcast/internal/model"
)

func shortPlan(t *testing.T, faultsAll bool) model.Plan {
	t.Helper()
	tracks := DefaultTracks()
	return model.Plan{
		Seed:        41,
		DurationMS:  3000,
		Tracks:      tracks,
		WrapModulus: DefaultWrapModulus(tracks),
		Faults:      DefaultFaultPlan(faultsAll),
	}
}

func readAll(t *testing.T, path string) (*Reader, []model.Packet) {
	t.Helper()
	fc := &fault.Collector{}
	r, err := Open(path, fc)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var pkts []model.Packet
	for {
		p, ok, err := r.ReadPacket()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !ok {
			break
		}
		pkts = append(pkts, p)
	}
	t.Cleanup(func() { r.Close() })
	return r, pkts
}

func TestCleanFixtureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, shortPlan(t, false)); err != nil {
		t.Fatal(err)
	}
	r, pkts := readAll(t, filepath.Join(dir, "master.bin"))
	if len(pkts) == 0 {
		t.Fatal("no packets delivered")
	}
	if r.faults.Count() != 0 {
		t.Fatalf("clean fixture has faults: %+v", r.faults.Items())
	}
	if len(r.Gaps()) != 0 {
		t.Fatalf("clean fixture has gaps: %+v", r.Gaps())
	}
	// 每条轨道序号必须从 0 连续。
	next := map[uint8]uint64{}
	for _, p := range pkts {
		if want := next[p.TrackID]; p.Seq != want {
			t.Fatalf("track %d seq %d, want %d", p.TrackID, p.Seq, want)
		}
		next[p.TrackID] = p.Seq + 1
		if p.ByteEnd <= p.ByteStart {
			t.Fatal("byte offsets must advance")
		}
	}
	if len(next) != 5 {
		t.Fatalf("tracks = %d, want 5", len(next))
	}
}

func TestFaultyFixtureReportsAndRecovers(t *testing.T) {
	dir := t.TempDir()
	p := shortPlan(t, true)
	if _, err := Generate(dir, p); err != nil {
		t.Fatal(err)
	}
	r, pkts := readAll(t, filepath.Join(dir, "master.bin"))
	fc := r.faults
	for _, kind := range []string{fault.KindChecksum, fault.KindTruncated, fault.KindDrop} {
		if fc.CountByKind(kind) == 0 {
			t.Fatalf("expected at least one %q fault, items=%+v", kind, fc.Items())
		}
	}
	if len(r.Gaps()) == 0 {
		t.Fatal("drops must produce gaps")
	}
	// 重复包：同一轨道出现相同序号。
	seen := map[[2]uint64]bool{}
	dupes := 0
	for _, p := range pkts {
		key := [2]uint64{uint64(p.TrackID), p.Seq}
		if seen[key] {
			dupes++
		}
		seen[key] = true
	}
	if dupes == 0 {
		t.Fatal("expected duplicate packets in fixture")
	}
	// 截断后仍有后续包：可恢复片段继续交付。
	if r.ReadCount() < uint64(len(p.Tracks))*5 {
		t.Fatalf("recovered packets too few: %d", r.ReadCount())
	}
	// 故障记录必须带字节偏移（截断/校验类）。
	for _, f := range fc.Items() {
		if (f.Kind == fault.KindTruncated || f.Kind == fault.KindChecksum) && f.ByteStart < 0 {
			t.Fatalf("fault missing byte offset: %+v", f)
		}
	}
}

func TestExternalTruncationResyncs(t *testing.T) {
	dir := t.TempDir()
	if _, err := Generate(dir, shortPlan(t, false)); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "master.bin")
	fi, _ := os.Stat(src)
	// 在文件中段砍断，制造头部完整但记录被截断的素材。
	broken := filepath.Join(dir, "broken.bin")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	cut := fi.Size() * 2 / 3
	if err := os.WriteFile(broken, data[:cut], 0o644); err != nil {
		t.Fatal(err)
	}
	fc := &fault.Collector{}
	r, err := Open(broken, fc)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n := 0
	for {
		_, ok, err := r.ReadPacket()
		if err != nil {
			t.Fatalf("reader must not hard-fail on truncation: %v", err)
		}
		if !ok {
			break
		}
		n++
	}
	if n == 0 {
		t.Fatal("truncated file yielded no recoverable packets")
	}
	if fc.CountByKind(fault.KindTruncated) == 0 {
		t.Fatalf("truncation must be reported, items=%+v", fc.Items())
	}
}

func TestBadMagicRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.bin")
	if err := os.WriteFile(path, []byte("XXXXXXXX............"), 0o644); err != nil {
		t.Fatal(err)
	}
	fc := &fault.Collector{}
	if _, err := Open(path, fc); err == nil {
		t.Fatal("bad magic must be rejected")
	}
}
