package fixture

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateIsReproducible(t *testing.T) {
	spec := DefaultSpec()
	a := t.TempDir()
	b := t.TempDir()
	fa, err := Generate(a, spec)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := Generate(b, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{fa.Timeline, fb.Timeline}, {fa.Sidecar, fb.Sidecar}, {fa.RTPDump, fb.RTPDump}} {
		da, _ := os.ReadFile(pair[0])
		db, _ := os.ReadFile(pair[1])
		if string(da) != string(db) {
			t.Fatalf("夹具不确定: %s vs %s", filepath.Base(pair[0]), filepath.Base(pair[1]))
		}
	}
}

func TestSpecValidation(t *testing.T) {
	bad := []Spec{
		{VideoSamples: 0, AudioSamples: 1, VideoTimescale: 90_000, AudioTimescale: 48_000},
		{VideoSamples: 5000, AudioSamples: 1, VideoTimescale: 90_000, AudioTimescale: 48_000},
		{VideoSamples: 10, AudioSamples: -1, VideoTimescale: 90_000, AudioTimescale: 48_000},
		{VideoSamples: 10, AudioSamples: 1, VideoTimescale: 0, AudioTimescale: 48_000},
		{VideoSamples: 10, AudioSamples: 1, VideoTimescale: 90_000, AudioTimescale: 0},
		{VideoSamples: 10, AudioSamples: 1, VideoTimescale: 90_000, AudioTimescale: 48_000, Pattern: "nope"},
	}
	for i, s := range bad {
		if err := s.Validate(); err == nil {
			t.Fatalf("第 %d 组非法参数应被拒绝", i)
		}
	}
}

func TestCorruptIsPrefixOfValid(t *testing.T) {
	dir := t.TempDir()
	files, err := Generate(dir, DefaultSpec())
	if err != nil {
		t.Fatal(err)
	}
	valid, _ := os.ReadFile(files.Timeline)
	corrupt, _ := os.ReadFile(files.Corrupt)
	if len(corrupt) >= len(valid) {
		t.Fatal("截断样例应短于有效样例")
	}
	if string(corrupt) != string(valid[:len(corrupt)]) {
		t.Fatal("截断样例应是有效样例的前缀")
	}
}

func TestPayloadDeterministic(t *testing.T) {
	a := payloadFor(1, 42, 64)
	b := payloadFor(1, 42, 64)
	c := payloadFor(2, 42, 64)
	if string(a) != string(b) {
		t.Fatal("同参数负载应一致")
	}
	if string(a[:12]) == string(c[:12]) {
		t.Fatal("不同轨道负载应可区分")
	}
}

func TestRTPDumpParseable(t *testing.T) {
	dir := t.TempDir()
	files, err := Generate(dir, DefaultSpec())
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(files.RTPDump)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("RTP dump 为空")
	}
	// 首条记录长度前缀应等于 RTP 包长（最小 12 字节头）。
	n := int(data[0])<<8 | int(data[1])
	if n < 12 || n+2 > len(data) {
		t.Fatalf("RTP 记录长度异常: %d", n)
	}
}
