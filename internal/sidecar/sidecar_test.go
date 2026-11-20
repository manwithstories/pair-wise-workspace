package sidecar

import (
	"bytes"
	"testing"
)

func sampleMeta() *Meta {
	return &Meta{
		Codec:         "av1",
		TrackID:       2,
		Timescale:     90000,
		NominalStep:   3000,
		KeyframeEvery: 150,
		StreamSize:    4096,
		StreamSHA256:  "abcdef0123456789",
		Frames: []FrameMeta{
			{RawTS: 0xFFFFFFF0, Keyframe: true},
			{RawTS: 0x00000008, Keyframe: false}, // 回绕
		},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	raw, err := Marshal(sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	// ISO BMFF 顶层必须以 box size + 'ftyp' 开头。
	if len(raw) < 8 || string(raw[4:8]) != "ftyp" {
		t.Fatalf("输出不是合法 ftyp 起始的 BMFF 结构: % x", raw[:8])
	}
	got, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got.Codec != "av1" || got.TrackID != 2 || got.Timescale != 90000 {
		t.Fatalf("元数据往返不匹配: %+v", got)
	}
	if len(got.Frames) != 2 || !got.Frames[0].Keyframe || got.Frames[1].RawTS != 0x00000008 {
		t.Fatalf("帧表往返不匹配: %+v", got.Frames)
	}
}

func TestReadRejectsMissingBoxes(t *testing.T) {
	if _, err := Read(bytes.NewReader([]byte{0, 0, 0, 8, 'f', 'r', 'e', 'e'})); err == nil {
		t.Fatalf("缺少 ftyp/maud 必须报错")
	}
}

func TestWriteIsDeterministic(t *testing.T) {
	a, err := Marshal(sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Marshal(sampleMeta())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("相同元数据两次写出的字节不一致")
	}
}
