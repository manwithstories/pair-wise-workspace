package fixture

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestGenerateDeterministic(t *testing.T) {
	p := DefaultParams()
	a := Generate(p)
	b := Generate(p)
	for _, rep := range p.Reps {
		if len(a[rep]) != p.Frames || len(b[rep]) != p.Frames {
			t.Fatalf("帧数错误: %d/%d", len(a[rep]), len(b[rep]))
		}
		for i := range a[rep] {
			if !bytes.Equal(a[rep][i].Payload, b[rep][i].Payload) {
				t.Fatalf("%s 帧 %d 两次生成不一致", rep, i)
			}
			if !bytes.Equal(a[rep][i].PayloadHash, b[rep][i].PayloadHash) {
				t.Fatalf("%s 帧 %d 哈希不一致", rep, i)
			}
		}
	}
}

func TestCorruptFrameHashMismatch(t *testing.T) {
	p := DefaultParams()
	frames := Generate(p)["360p"]
	f := frames[p.CorruptFrame]
	sum := sha256.Sum256(f.Payload)
	if bytes.Equal(sum[:], f.PayloadHash) {
		t.Fatal("损坏帧的载荷哈希不应与记录哈希一致")
	}
	// 其余帧哈希必须仍然自洽。
	for i, f := range frames {
		if i == p.CorruptFrame {
			continue
		}
		sum := sha256.Sum256(f.Payload)
		if !bytes.Equal(sum[:], f.PayloadHash) {
			t.Fatalf("帧 %d 意外哈希不符", i)
		}
	}
}

func TestWrapActuallyHappens(t *testing.T) {
	p := DefaultParams()
	frames := Generate(p)["360p"]
	wrapped := false
	for i := 1; i < len(frames); i++ {
		if frames[i].RTPTimestamp < frames[i-1].RTPTimestamp &&
			int64(frames[i-1].RTPTimestamp)-int64(frames[i].RTPTimestamp) > int64(p.WrapAt)/2 {
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatal("起始偏移应保证演示序列内至少发生一次回绕")
	}
}

func TestVFRIntervalsAndKeyframes(t *testing.T) {
	p := DefaultParams()
	frames := Generate(p)["360p"]
	seen := map[int64]bool{}
	for i := 1; i < 10; i++ {
		seen[frames[i].TruthNS-frames[i-1].TruthNS] = true
	}
	if !p.VFR || len(seen) < 2 {
		t.Fatalf("VFR 应产生多档帧间隔, got %d 档", len(seen))
	}
	for _, f := range frames {
		if int(f.Seq)%p.KeyInterval == 0 && !f.IsKey {
			t.Fatalf("帧 %d 应为关键帧", f.Seq)
		}
	}
}

func TestInputDigestStable(t *testing.T) {
	p := DefaultParams()
	if InputDigest(p) != InputDigest(p) {
		t.Fatal("相同参数摘要必须稳定")
	}
	q := p
	q.DriftPPM++
	if InputDigest(p) == InputDigest(q) {
		t.Fatal("参数变化必须改变输入摘要")
	}
}
