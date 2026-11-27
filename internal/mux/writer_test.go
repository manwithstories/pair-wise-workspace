package mux

import (
	"bytes"
	"crypto/sha256"
	"testing"

	"bcastqc/internal/ebml"
	"bcastqc/internal/media"
)

func sampleBlocks() []MuxBlock {
	var out []MuxBlock
	for i := 0; i < 25; i++ {
		track := media.TrackVideo
		key := i%10 == 0
		if i%2 == 1 {
			track = media.TrackAudio
		}
		out = append(out, MuxBlock{
			Track:   track,
			TimeNS:  int64(i) * 20_000_000,
			Key:     key,
			Payload: bytes.Repeat([]byte{byte(i + 1)}, 40),
		})
	}
	return out
}

func TestWriteAndIndependentVerify(t *testing.T) {
	tracks := []TrackSpec{
		{Number: media.TrackAudio, Type: ebml.TrackTypeAudio},
		{Number: media.TrackVideo, Type: ebml.TrackTypeVideo},
	}
	blocks := sampleBlocks()
	out, err := Write(tracks, blocks)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := Verify(out, blocks)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.TrackCount != 2 || res.BlockCount != len(blocks) {
		t.Fatalf("counts wrong: %+v", res)
	}
	if !res.Monotonic || !res.PayloadOK {
		t.Fatalf("verify flags wrong: %+v", res)
	}
	// 独立解读器直接解析，确认关键帧位被保留。
	doc, err := ebml.ParseDocument(ebml.NewBytesReader(out))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	keyCount := 0
	for _, b := range doc.Blocks {
		if b.Keyframe {
			keyCount++
		}
	}
	if keyCount == 0 {
		t.Fatal("no keyframe flags survived remux")
	}
}

func TestDeterministicOutputHash(t *testing.T) {
	tracks := []TrackSpec{{Number: 2, Type: ebml.TrackTypeVideo}}
	blocks := sampleBlocks()
	h := func() [32]byte {
		out, err := Write(tracks, blocks)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(out)
	}
	if h() != h() {
		t.Fatal("remux output nondeterministic")
	}
}

func TestEmptyInputProducesValidShell(t *testing.T) {
	out, err := Write(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ebml.ParseDocument(ebml.NewBytesReader(out)); err != nil {
		t.Fatalf("empty output should still parse: %v", err)
	}
}
