package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"mediarepair/internal/model"
)

func sample(seg string, seq uint64, payload string) model.RepairedSample {
	return model.RepairedSample{
		Sample:      model.Sample{TrackID: 1, Seq: seq, Data: []byte(payload)},
		RepairedDTS: seq * 1000,
		SegmentID:   seg,
	}
}

func TestSegmentHashDeterministic(t *testing.T) {
	build := func() []SegmentHash {
		h := NewHasher()
		for i, payload := range []string{"aa", "bb", "cc"} {
			if err := h.Add(sample("s1", uint64(i), payload)); err != nil {
				t.Fatal(err)
			}
		}
		return h.Finalize()
	}
	a := build()
	b := build()
	if len(a) != 1 || a[0].Digest != b[0].Digest {
		t.Fatalf("相同输入应得到相同摘要: %v vs %v", a, b)
	}
	if a[0].Samples != 3 || a[0].Bytes != 6 {
		t.Fatalf("样本/字节统计错误: %+v", a[0])
	}
}

func TestSegmentHashCoversPayloadAndTimestamps(t *testing.T) {
	h1 := NewHasher()
	_ = h1.Add(sample("s1", 0, "aa"))
	d1 := h1.Finalize()[0].Digest

	h2 := NewHasher()
	_ = h2.Add(sample("s1", 0, "ab")) // 负载不同
	d2 := h2.Finalize()[0].Digest

	h3 := NewHasher()
	s := sample("s1", 0, "aa")
	s.RepairedDTS = 500
	_ = h3.Add(s)
	d3 := h3.Finalize()[0].Digest

	if d1 == d2 || d1 == d3 {
		t.Fatal("摘要必须覆盖负载与时间戳")
	}
}

func TestHashOrderingStable(t *testing.T) {
	h := NewHasher()
	_ = h.Add(sample("b", 0, "x"))
	_ = h.Add(sample("a", 0, "y"))
	out := h.Finalize()
	if out[0].ID != "a" || out[1].ID != "b" {
		t.Fatalf("片段哈希应按 ID 排序: %v", out)
	}
}

func TestWriteSamplesEncoding(t *testing.T) {
	var buf bytes.Buffer
	rs := sample("s1", 3, "hello")
	if err := WriteSamples(&buf, rs); err != nil {
		t.Fatal(err)
	}
	// 编码 = trackID(8) + seq(8) + dts(8) + len(4) + payload
	if buf.Len() != 24+4+5 {
		t.Fatalf("编码长度 %d", buf.Len())
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("hello")) {
		t.Fatal("负载应位于编码尾部")
	}
}

func TestSummaryAndJSON(t *testing.T) {
	r := &Result{RoundtripMatch: 1, KeyframeHitRate: 0.99, PeakBufferSamples: 8, BackpressureWait: 12}
	r.SegmentHashes = []SegmentHash{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	s := r.SummaryText()
	for _, want := range []string{"roundtrip_match=1.0000", "keyframe_hit_rate=0.9900", "segment_hashes=3", "peak_buffer_samples=8", "backpressure_wait_ms=12"} {
		if !bytes.Contains([]byte(s), []byte(want)) {
			t.Fatalf("摘要缺少 %q: %s", want, s)
		}
	}
	data, err := r.MarshalIndent()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"segment_hashes"`)) {
		t.Fatal("JSON 应包含 segment_hashes")
	}
}

func TestHasherIgnoresUnattributed(t *testing.T) {
	h := NewHasher()
	if err := h.Add(model.RepairedSample{}); err != nil {
		t.Fatal(err)
	}
	if len(h.Finalize()) != 0 {
		t.Fatal("无片段归属的样本不应计入哈希")
	}
}

func TestDigestMatchesManualSHA256(t *testing.T) {
	h := NewHasher()
	rs := sample("s", 1, "abc")
	if err := h.Add(rs); err != nil {
		t.Fatal(err)
	}
	got := h.Finalize()[0].Digest

	m := sha256.New()
	var hdr [24]byte
	putU64(hdr[0:], 1)
	putU64(hdr[8:], 1)
	putU64(hdr[16:], 1000)
	m.Write(hdr[:])
	var l [4]byte
	putU32(l[:], 3)
	m.Write(l[:])
	m.Write([]byte("abc"))
	want := hex.EncodeToString(m.Sum(nil))
	if got != want {
		t.Fatalf("摘要编码不一致: %s != %s", got, want)
	}
}
