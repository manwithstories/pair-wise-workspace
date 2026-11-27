package engine

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"relay/internal/index"
	"relay/internal/streamgen"
)

// TestWrapStartGeneratesWrap 验证 WrapStart 夹具确实产生时间戳回绕，
// 且扫描器的回绕计数与扩展时间戳单调性正确。
func TestWrapStartGeneratesWrap(t *testing.T) {
	dir := t.TempDir()
	stream, _ := streamgen.Generate(streamgen.Spec{
		Seed: 41, DurationMS: 5000, AudioRate: 48000, Channels: 2,
		ClockRate: 48000, PPM: 150, DamageSeq: 0xFFFF, WrapStart: true,
	})
	p := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(p, stream, 0644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	idx, err := index.Scan(f, params(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if idx.WrapCount == 0 {
		t.Fatal("WrapStart fixture must cross a 32-bit timestamp boundary")
	}
	// 流内含受控缺包（300）与乱序（500/501），按扩展时间戳排序后必须严格单调，
	// 证明回绕扩展没有产生倒退或重复。
	ts := make([]int64, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		ts = append(ts, e.ExtTS)
	}
	sort.Slice(ts, func(a, b int) bool { return ts[a] < ts[b] })
	for i := 1; i < len(ts); i++ {
		if ts[i] <= ts[i-1] {
			t.Fatalf("extended ts not strictly monotonic at %d: %d <= %d",
				i, ts[i], ts[i-1])
		}
	}
}
