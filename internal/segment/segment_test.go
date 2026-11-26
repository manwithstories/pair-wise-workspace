package segment

import (
	"errors"
	"testing"

	"tsproof/internal/clock"
)

func makePackets(n, size int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		p := make([]byte, size)
		p[0] = 0x47
		p[1] = byte(i >> 8)
		p[2] = byte(i)
		out[i] = p
	}
	return out
}

func TestFixedSegmentHashes(t *testing.T) {
	pkts := makePackets(70, 188)
	h, err := NewHasher(32)
	if err != nil {
		t.Fatal(err)
	}
	hashes, err := h.HashPackets(pkts)
	if err != nil {
		t.Fatal(err)
	}
	// 70 = 32 + 32 + 6。
	if len(hashes) != 3 {
		t.Fatalf("segments=%d want 3", len(hashes))
	}
	if hashes[0].Packets != 32 || hashes[2].Packets != 6 {
		t.Fatalf("segment sizes wrong: %d %d", hashes[0].Packets, hashes[2].Packets)
	}
	if hashes[0].SHA256 == hashes[1].SHA256 {
		t.Fatal("distinct segments must have distinct hashes")
	}
	// 独立复算必须一致。
	h2, _ := NewHasher(32)
	again, _ := h2.HashPackets(pkts)
	if mm := Verify(again, hashes); len(mm) != 0 {
		t.Fatalf("rehash mismatch at %v", mm)
	}
}

func TestHashIndependentOfRecompute(t *testing.T) {
	pkts := makePackets(100, 188)
	h, _ := NewHasher(25)
	first, _ := h.HashPackets(pkts)
	// 修改第 30 个包（落在第 2 段），只应改变该段哈希。
	pkts[30][10] ^= 0xFF
	second, _ := h.HashPackets(pkts)
	if first[0].SHA256 != second[0].SHA256 || first[2].SHA256 != second[2].SHA256 {
		t.Fatal("unaffected segments must stay identical")
	}
	if first[1].SHA256 == second[1].SHA256 {
		t.Fatal("mutated segment hash must change")
	}
}

func TestSegmentValidation(t *testing.T) {
	if _, err := NewHasher(0); !errors.Is(err, ErrSegment) {
		t.Fatalf("want ErrSegment, got %v", err)
	}
	h, _ := NewHasher(8)
	if _, err := h.HashPackets(nil); !errors.Is(err, ErrSegment) {
		t.Fatalf("empty input must fail, got %v", err)
	}
}

func TestVerifyDetectsBoundaryChange(t *testing.T) {
	pkts := makePackets(40, 188)
	h, _ := NewHasher(20)
	want, _ := h.HashPackets(pkts)
	// 用不同分片大小重算，边界不同应被 Verify 识别。
	hOther, _ := NewHasher(10)
	got, _ := hOther.HashPackets(pkts)
	if mm := Verify(got, want); len(mm) == 0 {
		t.Fatal("different segmentation must not verify")
	}
}

func TestKeyframeSeekAndHitRate(t *testing.T) {
	tl := clock.NewTimeline()
	tr := tl.EnsureTrack(0x100)
	// 每 4 帧一个关键帧，PTS 间隔 900k。
	var keyPTS []clock.Tick
	for i := 0; i < 16; i++ {
		pts := clock.Tick(i) * 900_000
		key := i%4 == 0
		tr.Append(clock.TimelineEvent{
			Index: i, At: pts, RawPTS: pts,
			Kind: map[bool]clock.FrameKind{true: clock.FrameKey, false: clock.FrameInter}[key],
		})
		if key {
			keyPTS = append(keyPTS, pts)
		}
	}
	loc := BuildKeyframeLocator(tr)
	if loc.Size() != 4 {
		t.Fatalf("keyframes=%d want 4", loc.Size())
	}
	// 精确命中关键帧。
	at, idx, err := loc.Seek(keyPTS[2])
	if err != nil || at != keyPTS[2] {
		t.Fatalf("exact seek: at=%d idx=%d err=%v", at, idx, err)
	}
	// 落在两关键帧之间，回退到不晚于目标的最近关键帧。
	mid := keyPTS[1] + 100
	at, _, err = loc.Seek(mid)
	if err != nil || at != keyPTS[1] {
		t.Fatalf("floor seek: at=%d want %d err=%v", at, keyPTS[1], err)
	}
	// 早于首关键帧必须失败。
	if _, _, err := loc.Seek(keyPTS[0] - 1); !errors.Is(err, ErrSeek) {
		t.Fatalf("early seek must fail, got %v", err)
	}
	rep := loc.VerifySeeds(keyPTS)
	if rep.Hits != 4 || rep.Misses != 0 || rep.HitRate() != 100.0 {
		t.Fatalf("seek report wrong: %+v", rep)
	}
}
