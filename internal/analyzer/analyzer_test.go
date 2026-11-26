package analyzer_test

import (
	"testing"

	"tsproof/internal/analyzer"
	"tsproof/internal/clock"
	"tsproof/internal/fixture"
)

func TestCleanStreamEndToEnd(t *testing.T) {
	clean := fixture.GenerateClean(20261126)
	buf := packetsToBytes(clean.Packets)
	a := analyzer.New()
	res, err := a.Parse(buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if res.Map == nil {
		t.Fatal("PSI map must be established from clean stream")
	}
	if res.Metrics.CRCReject != 0 || res.Metrics.CCGaps != 0 ||
		res.Metrics.Duplicates != 0 || res.Metrics.AdapterErrors != 0 {
		t.Fatalf("clean stream has damage: %+v", res.Metrics)
	}
	video := res.Blocks[fixture.PIDVideo]
	audio := res.Blocks[fixture.PIDAudio]
	if len(video) != fixture.FrameCount {
		t.Fatalf("video blocks=%d want %d", len(video), fixture.FrameCount)
	}
	if len(audio) != fixture.AudioBlocks {
		t.Fatalf("audio blocks=%d want %d", len(audio), fixture.AudioBlocks)
	}
	// 关键帧真值比对。
	tr := res.Timeline.Track(fixture.PIDVideo)
	if tr == nil {
		t.Fatal("video track missing")
	}
	kf := tr.Keyframes()
	if len(kf) != len(clean.KeyIndexes) {
		t.Fatalf("keyframes=%d want %d", len(kf), len(clean.KeyIndexes))
	}
	for i, ki := range clean.KeyIndexes {
		if kf[i] != ki {
			t.Fatalf("keyframe[%d]=%d want %d", i, kf[i], ki)
		}
	}
	// VFR 间隔必须原样保留在时间线事件上。
	for i := 1; i < len(clean.FrameVFRGap); i++ {
		got := tr.Events[i].VFRGap
		if got != clock.Tick(clean.FrameVFRGap[i]) {
			t.Fatalf("VFR gap[%d]=%d want %d", i, got, clean.FrameVFRGap[i])
		}
	}
	if !res.Timeline.Monotonic() {
		t.Fatal("timeline must be monotonic")
	}
}

func TestCCJumpAndUntrust(t *testing.T) {
	clean := fixture.GenerateClean(42)
	damaged, hits := fixture.InjectCCJump(clean.Packets)
	if hits != 1 {
		t.Fatalf("cc jump hits=%d", hits)
	}
	res, err := analyzer.New().Parse(packetsToBytes(damaged))
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.CCGaps == 0 {
		t.Fatal("CC jump must produce gap metric")
	}
	if res.Metrics.Untrusted == 0 {
		t.Fatal("gap must propagate untrust to track blocks")
	}
}

func TestDuplicateAndReorder(t *testing.T) {
	clean := fixture.GenerateClean(7)
	dup, nd := fixture.InjectDuplicate(clean.Packets)
	if nd != 1 {
		t.Fatalf("dup hits=%d", nd)
	}
	res, err := analyzer.New().Parse(packetsToBytes(dup))
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.Duplicates != 1 {
		t.Fatalf("duplicates=%d want 1", res.Metrics.Duplicates)
	}
	// 重复包被丢弃后，视频帧数应与干净流一致。
	if len(res.Blocks[fixture.PIDVideo]) != fixture.FrameCount {
		t.Fatalf("dup changed frame count: %d", len(res.Blocks[fixture.PIDVideo]))
	}
	reord, nr := fixture.InjectReorder(clean.Packets)
	if nr != 1 {
		t.Fatalf("reorder hits=%d", nr)
	}
	res2, err := analyzer.New().Parse(packetsToBytes(reord))
	if err != nil {
		t.Fatal(err)
	}
	if res2.Metrics.Reorders == 0 {
		t.Fatal("reorder must be counted")
	}
}

func TestTruncatedStream(t *testing.T) {
	clean := fixture.GenerateClean(99)
	cut, removed := fixture.InjectTruncate(clean.Packets)
	if removed == 0 {
		t.Fatal("truncation removed nothing")
	}
	res, err := analyzer.New().Parse(packetsToBytes(cut))
	if err != nil {
		t.Fatalf("truncated stream must not hard-fail: %v", err)
	}
	if res.Metrics.TruncatedPES == 0 {
		t.Fatal("truncation must leave an incomplete PES")
	}
}

func TestBadCRCMappingHeld(t *testing.T) {
	clean := fixture.GenerateClean(123)
	bad, hits := fixture.InjectBadCRCPI(clean.Packets)
	if hits != 1 {
		t.Fatalf("crc inject hits=%d", hits)
	}
	res, err := analyzer.New().Parse(packetsToBytes(bad))
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.CRCReject == 0 {
		t.Fatal("corrupt PSI CRC must be rejected")
	}
	// 首包 PAT 即损坏 → 无映射 → 不应解出任何媒体块。
	if len(res.Blocks) != 0 {
		t.Fatalf("no valid PSI means no media decode, got %d tracks", len(res.Blocks))
	}
}

func TestPSIUpdateSwitchesVersion(t *testing.T) {
	clean := fixture.GenerateClean(555)
	updated, n := fixture.InjectPSIUpdate(clean.Packets, len(clean.Packets)/2)
	if n == 0 {
		t.Fatal("psi update inserted nothing")
	}
	res, err := analyzer.New().Parse(packetsToBytes(updated))
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.PSISwitches == 0 {
		t.Fatal("valid versioned PSI must record a switch")
	}
	if res.Map == nil || res.Map.Version != 2 {
		t.Fatalf("active map version=%v want 2", res.Map)
	}
	if res.Metrics.CRCReject != 0 {
		t.Fatal("valid PSI update must not be CRC-rejected")
	}
}

func TestPCRWrapCounted(t *testing.T) {
	clean := fixture.GenerateClean(31)
	wrapped, hits := fixture.InjectPCRWrap(clean.Packets)
	if hits == 0 {
		t.Fatal("no PCR packets rewritten")
	}
	res, err := analyzer.New().Parse(packetsToBytes(wrapped))
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.PCRWraps == 0 {
		t.Fatal("PCR wrap events must be counted")
	}
	if !res.Timeline.Monotonic() {
		t.Fatal("unwrapped timeline must stay monotonic")
	}
}

func packetsToBytes(pkts [][]byte) []byte {
	n := 0
	for _, p := range pkts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range pkts {
		out = append(out, p...)
	}
	return out
}
