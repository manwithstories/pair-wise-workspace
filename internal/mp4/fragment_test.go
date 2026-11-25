package mp4

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"relaycheck/internal/model"
)

// ---- 最小 fMP4 盒构造（仅用于测试夹具，不依赖外部文件） ----

func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

func fullBox(typ string, version uint8, flags uint32, body []byte) []byte {
	hdr := []byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}
	return box(typ, append(hdr, body...))
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return b
}

func u64(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// buildFragment 构造含两个样本的 moof+mdat 分片。
// 样本时长 3000 ticks（timescale 90k => 33.33ms），
// sample0 为关键帧，sample1 为非关键帧。
func buildFragment(t *testing.T, baseDecodeTime uint64, payloadLen int) []byte {
	t.Helper()
	mfhd := fullBox("mfhd", 0, 0, u32(0)) // mfhd 是 fullbox：version/flags + sequence_number
	tfhd := fullBox("tfhd", 0, 0x020000, u32(1)) // default-base-is-moof
	tfdt := fullBox("tfdt", 1, 0, u64(baseDecodeTime))
	// trun: data-offset + duration + sample-size + sample-flags；两样本。
	sample0 := make([]byte, payloadLen/2)
	sample1 := make([]byte, payloadLen-len(sample0))
	trunBody := append(u32(2), u32(0)...) // count + dataOffset 占位后填
	entry := func(dur, size uint32, flags uint32) []byte {
		return append(append(u32(dur), u32(size)...), u32(flags)...)
	}
	trunBody = append(trunBody, entry(3000, uint32(len(sample0)), 0x02000000)...) // sync
	trunBody = append(trunBody, entry(3000, uint32(len(sample1)), 0x01000000)...) // depends
	trun := fullBox("trun", 0, trunDataOffset|trunSampleDuration|trunSampleSize|trunSampleFlags, trunBody)
	trafBody := append(append(tfhd, tfdt...), trun...)
	traf := box("traf", trafBody)
	moof := box("moof", append(mfhd, traf...))
	mdatPayload := append(sample0, sample1...)
	mdat := box("mdat", mdatPayload)
	frag := append(moof, mdat...)
	// trun DataOffset 相对 moof 起点：越过整个 moof 与 mdat 盒头即到负载。
	dataOffset := len(moof) + 8
	// trun 在整个分片内的位置：moof头 + mfhd + traf头 + tfhd + tfdt + trun头/字段。
	trunOff := 8 /*moof*/ + len(mfhd) + 8 /*traf*/ + len(tfhd) + len(tfdt) +
		8 /*trun hdr*/ + 4 /*fullbox hdr*/ + 4 /*count*/
	binary.BigEndian.PutUint32(frag[trunOff:trunOff+4], uint32(dataOffset))
	return frag
}

func writeFrag(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseFragmentSamplesAndKeyframes(t *testing.T) {
	dir := t.TempDir()
	frag := buildFragment(t, 0, 200)
	p := writeFrag(t, dir, "seg0.m4s", frag)
	mem := model.NewMemTracker(1 << 20)
	rep, err := ParseFragment(p, 0, 90000, mem)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(rep.Samples) != 2 {
		t.Fatalf("samples = %d", len(rep.Samples))
	}
	if !rep.Samples[0].IsSync || rep.Samples[1].IsSync {
		t.Fatalf("sync flags = %v,%v", rep.Samples[0].IsSync, rep.Samples[1].IsSync)
	}
	if len(rep.SyncTimes) != 1 {
		t.Fatalf("sync times = %d", len(rep.SyncTimes))
	}
	if len(rep.Faults) != 0 || rep.FirstBadOff != -1 {
		t.Fatalf("unexpected faults: %+v", rep.Faults)
	}
	// 样本时长 3000 ticks @90k = 33.33ms（非整数毫秒，验证有理数不丢精度）。
	// 第二个样本 PTS = 3000 ticks = 33_333_333 ns。
	if got := rep.Samples[1].Pts.RoundNanos(); got != 33_333_333 {
		t.Fatalf("pts1 = %d", got)
	}
}

func TestKeyframeGapsAcrossFragments(t *testing.T) {
	dir := t.TempDir()
	mem := model.NewMemTracker(1 << 20)
	// 每个分片一个关键帧（baseDecodeTime 步进 90000 ticks = 1s）。
	var sync []model.Rat
	for seq := 0; seq < 3; seq++ {
		frag := buildFragment(t, uint64(seq*90000), 100)
		p := writeFrag(t, dir, "seg.m4s", frag)
		rep, err := ParseFragment(p, seq, 90000, mem)
		if err != nil {
			t.Fatal(err)
		}
		sync = append(sync, rep.SyncTimes...)
	}
	gaps := KeyframeGaps(sync)
	if len(gaps) != 2 {
		t.Fatalf("gaps = %d", len(gaps))
	}
	if got := gaps[0].RoundMillis(); got != 1000 {
		t.Fatalf("keyframe gap = %d ms", got)
	}
}

func TestTruncatedBoxReportsFirstOffset(t *testing.T) {
	dir := t.TempDir()
	frag := buildFragment(t, 0, 200)
	// 在 mdat 中段截断：盒头声明长度保持不变。
	cut := len(frag) - 40
	p := writeFrag(t, dir, "seg0.m4s", frag[:cut])
	mem := model.NewMemTracker(1 << 20)
	rep, err := ParseFragment(p, 0, 90000, mem)
	if err != nil {
		t.Fatalf("ParseFragment returned error: %v", err)
	}
	// moof 仍完整，样本已解析；截断在 mdat 被定位。
	if len(rep.Faults) == 0 {
		t.Fatal("expected truncated fault")
	}
	first := rep.Faults[0]
	if first.Kind != model.FaultBoxTruncated {
		t.Fatalf("first fault kind = %s", first.Kind)
	}
	// 故障偏移必须指向被截断 mdat 的盒头（即完整 moof 之后的位置）。
	// 重新解析完整分片以取得 moof 长度作为 mdat 盒头基准。
	full := buildFragment(t, 0, 200)
	wantMdatOff := int64(0)
	{
		fullReader := &limitedReader{r: bytes.NewReader(full), base: 0, end: int64(len(full))}
		fb, ferr := fullReader.nextBox(0)
		if ferr != nil {
			t.Fatal(ferr)
		}
		wantMdatOff = fb.off + int64(fb.size)
	}
	if delta := first.Offset - wantMdatOff; delta < -16 || delta > 16 {
		t.Fatalf("first bad offset %d not within 16 bytes of mdat header %d", first.Offset, wantMdatOff)
	}
}

func TestZeroTimescaleFaultAndRecovery(t *testing.T) {
	dir := t.TempDir()
	frag := buildFragment(t, 0, 100)
	p := writeFrag(t, dir, "seg0.m4s", frag)
	mem := model.NewMemTracker(1 << 20)
	rep, err := ParseFragment(p, 0, 0, mem)
	if err != nil {
		t.Fatalf("parser should not hard-fail: %v", err)
	}
	found := false
	for _, f := range rep.Faults {
		if f.Kind == model.FaultTimeScaleZero {
			found = true
		}
	}
	if !found {
		t.Fatalf("zero timescale not reported: %+v", rep.Faults)
	}
}

func TestOversizedFragmentRejectedByMemoryLimit(t *testing.T) {
	dir := t.TempDir()
	frag := buildFragment(t, 0, 400)
	p := writeFrag(t, dir, "seg0.m4s", frag)
	mem := model.NewMemTracker(64) // 远小于分片
	rep, err := ParseFragment(p, 0, 90000, mem)
	if err != model.ErrMemoryLimit {
		t.Fatalf("err = %v, want ErrMemoryLimit", err)
	}
	found := false
	for _, f := range rep.Faults {
		if f.Kind == model.FaultOversized {
			found = true
		}
	}
	if !found {
		t.Fatalf("oversized fault missing: %+v", rep.Faults)
	}
	if mem.Peak() > 64 {
		t.Fatalf("rejected allocation must not raise peak, got %d", mem.Peak())
	}
}
