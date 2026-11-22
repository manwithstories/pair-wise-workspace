package fixture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mediaverify/internal/manifest"
)

func TestGenerateDeterministic(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	p1, err := Generate(d1, DefaultOptions(42))
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Generate(d2, DefaultOptions(42))
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Packets) != len(p2.Packets) {
		t.Fatal("packet count differs")
	}
	for i := range p1.Packets {
		a, b := p1.Packets[i], p2.Packets[i]
		if a.Seq != b.Seq || a.TS != b.TS || string(a.Payload) != string(b.Payload) {
			t.Fatalf("packet %d differs", i)
		}
	}
	// 同种子文件内容一致
	f1, _ := os.ReadFile(p1.MPDPath)
	f2, _ := os.ReadFile(p2.MPDPath)
	if string(f1) != string(f2) {
		t.Fatal("MPD differs")
	}
}

func TestGeneratedManifestsParse(t *testing.T) {
	dir := t.TempDir()
	p, err := Generate(dir, DefaultOptions(42))
	if err != nil {
		t.Fatal(err)
	}
	mpd, _ := os.ReadFile(p.MPDPath)
	prog, err := manifest.ParseDASH(p.MPDPath, strings.NewReader(string(mpd)))
	if err != nil {
		t.Fatalf("DASH: %v", err)
	}
	if len(prog.Representations[0].Segments) != 4 {
		t.Fatalf("dash segs=%d", len(prog.Representations[0].Segments))
	}
	hls, _ := os.ReadFile(p.HLSPath)
	hprog, err := manifest.ParseHLS(p.HLSPath, strings.NewReader(string(hls)))
	if err != nil {
		t.Fatalf("HLS: %v", err)
	}
	if len(hprog.Representations[0].Segments) != 4 {
		t.Fatalf("hls segs=%d", len(hprog.Representations[0].Segments))
	}
	// 损伤存在：重复/乱序/丢失
	var dup, reorder, gap bool
	for i := 1; i < len(p.Packets); i++ {
		prev, cur := p.Packets[i-1], p.Packets[i]
		if cur.Seq == prev.Seq {
			dup = true
		}
		if cur.Seq < prev.Seq && cur.Seq != prev.Seq {
			reorder = true
		}
		if cur.Seq > prev.Seq+1 {
			gap = true
		}
	}
	if !dup || !reorder || !gap {
		t.Fatalf("impairments missing: dup=%v reorder=%v gap=%v", dup, reorder, gap)
	}
}

func TestPCMSegmentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.m4s")
	// 正确长度：1 帧 = 48000/50=960 采样 * 1ch * 2B
	full := make([]byte, 960*2)
	if err := WritePCMSegment(path, 48000, 1, 1, full); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	rate, ch, frames, pl, err := ReadPCMSegment(b)
	if err != nil {
		t.Fatal(err)
	}
	if rate != 48000 || ch != 1 || frames != 1 || len(pl) != 1920 {
		t.Fatalf("rate=%d ch=%d frames=%d len=%d", rate, ch, frames, len(pl))
	}
	// 截断检测
	if _, _, _, _, err := ReadPCMSegment(b[:len(b)/2]); err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestCorruptSet(t *testing.T) {
	dir := t.TempDir()
	cs, err := GenerateCorrupt(dir, DefaultOptions(7))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cs.NonMonotonicMPD, cs.MissingInitM3U8} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s", p)
		}
	}
	if len(cs.RollbackPackets) != 10 {
		t.Fatalf("rollback pkts=%d", len(cs.RollbackPackets))
	}
	if cs.RollbackPackets[5].TS != 0 {
		t.Fatal("rollback not injected")
	}
}
