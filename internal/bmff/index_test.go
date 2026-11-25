package bmff

import (
	"encoding/binary"
	"testing"

	"avlab/internal/media"
)

func buildTestSegment(t *testing.T) ([]byte, FragmentSpec) {
	t.Helper()
	spec := FragmentSpec{
		Sequence: 1, TrackID: 1,
		BaseTime: media.FromDuration(2_000), // 2 s in ticks
		Samples: []SampleDesc{
			{Size: 10, Duration: 3000, Keyframe: true, Payload: []byte("0123456789")},
			{Size: 8, Duration: 3000, Keyframe: false, Payload: []byte("abcdefgh")},
			{Size: 6, Duration: 3000, Keyframe: false, Payload: []byte("zyxwvu")},
			{Size: 9, Duration: 3000, Keyframe: true, Payload: []byte("KEYFRAME!")},
		},
	}
	data, err := BuildFragment(spec)
	if err != nil {
		t.Fatalf("BuildFragment: %v", err)
	}
	return data, spec
}

func TestIndexRoundtrip(t *testing.T) {
	data, spec := buildTestSegment(t)
	idx, err := IndexFragment(data, media.Epoch(1))
	if err != nil {
		t.Fatalf("IndexFragment: %v", err)
	}
	if idx.Sequence != spec.Sequence || idx.TrackID != spec.TrackID {
		t.Fatalf("header mismatch: %+v", idx)
	}
	if idx.BaseTime != spec.BaseTime {
		t.Fatalf("base time = %d, want %d", idx.BaseTime, spec.BaseTime)
	}
	if len(idx.Samples) != len(spec.Samples) {
		t.Fatalf("sample count = %d, want %d", len(idx.Samples), len(spec.Samples))
	}
	for i, s := range idx.Samples {
		if s.Size != spec.Samples[i].Size ||
			s.Duration != media.MediaTime(spec.Samples[i].Duration) ||
			s.Keyframe != spec.Samples[i].Keyframe {
			t.Fatalf("sample %d mismatch: %+v", i, s)
		}
	}
	if len(idx.Keyframes) != 2 ||
		!idx.Keyframes[0].IsFirst || idx.Keyframes[1].IsFirst {
		t.Fatalf("keyframe index wrong: %+v", idx.Keyframes)
	}
}

func TestRejectOutOfBoundsBox(t *testing.T) {
	data, _ := buildTestSegment(t)
	// inflate the first box's declared size beyond the buffer
	binary.BigEndian.PutUint32(data[0:4], uint32(len(data))+100)
	_, err := IndexFragment(data, 0)
	if !media.IsCode(err, media.CodeBoxOutOfBounds) {
		t.Fatalf("err = %v, want CodeBoxOutOfBounds", err)
	}
}

func TestRejectTruncated(t *testing.T) {
	data, _ := buildTestSegment(t)
	// cut the buffer in half so nested box payloads run short
	_, err := IndexFragment(data[:len(data)/2], 0)
	if err == nil {
		t.Fatal("truncated fragment was accepted")
	}
	if f := media.AsFault(err); f == nil ||
		(f.Code != media.CodeBoxTruncated && f.Code != media.CodeBoxOutOfBounds) {
		t.Fatalf("err = %v, want truncated/out-of-bounds fault", err)
	}
}

func TestRejectTimestampOverflow(t *testing.T) {
	spec := FragmentSpec{
		Sequence: 9, TrackID: 1, BaseTime: media.MaxTick,
		Samples: []SampleDesc{
			{Size: 4, Duration: 3000, Keyframe: true, Payload: []byte("abcd")},
		},
	}
	data, err := BuildFragment(spec)
	if err != nil {
		t.Fatalf("BuildFragment: %v", err)
	}
	_, err = IndexFragment(data, 0)
	if !media.IsCode(err, media.CodeTimestampOverflow) {
		t.Fatalf("err = %v, want CodeTimestampOverflow", err)
	}
}

func TestRejectMdatTooSmall(t *testing.T) {
	data, _ := buildTestSegment(t)
	// inflate the first trun sample's declared size while keeping every box
	// boundary intact: the samples then no longer fit inside mdat.
	// locate trun by walking the top-level moof container
	trunPos := -1
	for p := 0; p+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[p:]))
		if string(data[p+4:p+8]) == "moof" {
			q := p + 8
			for q+8 < p+size {
				inner := int(binary.BigEndian.Uint32(data[q:]))
				if string(data[q+4:q+8]) == "trun" {
					trunPos = q
					break
				}
				q += inner
			}
			break
		}
		p += size
	}
	if trunPos < 0 {
		t.Fatal("trun not found")
	}
	// trun: 8 box + 4 fullbox + 4 count + 4 data offset, then entries
	// of [duration(4) size(4) flags(4)]
	sampleSizePos := trunPos + 8 + 4 + 4 + 4 + 4
	binary.BigEndian.PutUint32(data[sampleSizePos:], 1_000_000)
	_, err := IndexFragment(data, 0)
	if !media.IsCode(err, media.CodeBoxOutOfBounds) {
		t.Fatalf("err = %v, want CodeBoxOutOfBounds", err)
	}
}
