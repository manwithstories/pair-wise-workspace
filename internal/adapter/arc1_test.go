package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"archivemedia/internal/model"
)

func videoTrack() ARCTrackSpec {
	return ARCTrackSpec{Type: MediaVideo, Codec: "raw-test", TimeBase: model.MustRational(1, 25),
		Width: 64, Height: 48, WrapBits: 32}
}

func writeTemp(t *testing.T, name string, tracks []ARCTrackSpec, pkts []ARCPacket, trunc bool) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := WriteARCFile(p, tracks, pkts, trunc); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestARC1RoundTrip(t *testing.T) {
	pkts := []ARCPacket{
		{0, 0, 1, true, []byte("frame-0")},
		{0, 1, 1, false, []byte("frame-1")},
		{0, 2, 1, false, []byte("frame-2")},
	}
	p := writeTemp(t, "ok.arc", []ARCTrackSpec{videoTrack()}, pkts, false)
	pr := &Arc1Parser{}
	ok, err := pr.Probe(p)
	if err != nil || !ok {
		t.Fatalf("probe ok=%v err=%v", ok, err)
	}
	pm, err := pr.Parse(p, DefaultLimits())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(pm.Packets) != 3 || pm.Tracks[0].WrapBits != 32 {
		t.Fatalf("parsed=%s", pm)
	}
	if pm.VerifiedPrefix != pm.FileSize {
		t.Fatalf("完整文件 verified=%d size=%d", pm.VerifiedPrefix, pm.FileSize)
	}
}

func TestARC1WrapExpansion(t *testing.T) {
	mod := int64(1) << 32
	// 刻度 ... 2^32-2, 2^32-1, 0, 1 ... 解析后必须连续递增
	pkts := []ARCPacket{
		{0, mod - 2, 1, true, []byte("a")},
		{0, mod - 1, 1, false, []byte("b")},
		{0, 0, 1, false, []byte("c")},
		{0, 1, 1, false, []byte("d")},
	}
	p := writeTemp(t, "wrap.arc", []ARCTrackSpec{videoTrack()}, pkts, false)
	opts := DefaultLimits()
	// 32 位回绕在 25fps 下跨度约 48 小时，测试只验证展开逻辑，放宽时长上限。
	opts.MaxDurationUs = 0
	pm, err := (&Arc1Parser{}).Parse(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(pm.Wraps) != 1 {
		t.Fatalf("应记录 1 次回绕, got %d", len(pm.Wraps))
	}
	want := []int64{mod - 2, mod - 1, mod, mod + 1}
	for i, pkt := range pm.Packets {
		if pkt.StartTicks != want[i] {
			t.Fatalf("pkt %d expanded=%d want %d", i, pkt.StartTicks, want[i])
		}
	}
}

func TestARC1TruncatedTail(t *testing.T) {
	pkts := make([]ARCPacket, 6)
	for i := range pkts {
		pkts[i] = ARCPacket{0, int64(i), 1, i == 0, []byte{byte(i), byte(i + 1), byte(i + 2), byte(i + 3)}}
	}
	p := writeTemp(t, "trunc.arc", []ARCTrackSpec{videoTrack()}, pkts, true)
	pm, err := (&Arc1Parser{}).Parse(p, DefaultLimits())
	if err == nil || model.KindOf(err) != model.KindCorruptMedia {
		t.Fatalf("期望 corrupt 错误, got %v", err)
	}
	if pm == nil || !pm.Truncated || len(pm.Packets) != 5 {
		t.Fatalf("应保留 5 个已验证前缀包, got %d", len(pm.Packets))
	}
	// 已验证前缀：最后一个完整包的时间必须早于首个坏时间
	lastGood := model.TicksToTime(pm.Packets[4].StartTicks, pm.Tracks[0].TimeBase)
	me, _ := model.AsMediaError(err)
	if me.FirstBadAt.IsZero() || !me.FirstBadAt.After(lastGood) {
		t.Fatalf("首个坏时间 %v 必须晚于最后完好包 %v", me.FirstBadAt, lastGood)
	}
	// 残缺尾包的字节不得计入已验证前缀。
	if pm.VerifiedPrefix >= pm.FileSize {
		t.Fatalf("verified=%d 必须小于文件大小=%d", pm.VerifiedPrefix, pm.FileSize)
	}
}

func TestARC1QuotasAndBadFiles(t *testing.T) {
	pkts := []ARCPacket{{0, 0, 1, true, []byte("x")}}
	p := writeTemp(t, "q.arc", []ARCTrackSpec{videoTrack()}, pkts, false)
	opts := DefaultLimits()
	opts.MaxFileBytes = 1
	if _, err := (&Arc1Parser{}).Parse(p, opts); model.KindOf(err) != model.KindQuotaExceeded {
		t.Fatalf("大小限制: %v", err)
	}
	// 非 arc1 文件 -> unsupported
	other := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(other, []byte("NOPE...."), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := (&Arc1Parser{}).Probe(other); ok {
		t.Fatal("魔数不匹配不得 probe 命中")
	}
}

func TestARC1RejectsBadVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "v.arc")
	// 魔数正确但版本号 99
	bad := append([]byte(ARCMagic), 99, 0, 1, 0)
	if err := os.WriteFile(p, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (&Arc1Parser{}).Parse(p, DefaultLimits())
	if model.KindOf(err) != model.KindUnsupportedFormat {
		t.Fatalf("版本不兼容必须隔离, got %v", err)
	}
}
