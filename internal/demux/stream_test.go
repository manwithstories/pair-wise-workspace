package demux_test

import (
	"bytes"
	"io"
	"testing"

	"mediapack/internal/codec"
	"mediapack/internal/demux"
	"mediapack/internal/fault"
	"mediapack/internal/fixture"
)

func TestChunkReaderBudget(t *testing.T) {
	data := bytes.Repeat([]byte{0xAB}, 10000)
	cr := demux.NewChunkReader(bytes.NewReader(data), 4096)
	buf := make([]byte, 9000)
	total := 0
	for total < len(data) {
		n, err := cr.Read(buf)
		if n > 4096 {
			t.Fatalf("read exceeded chunk budget: %d", n)
		}
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if total != len(data) || cr.BytesRead != int64(len(data)) {
		t.Fatalf("total=%d tracked=%d", total, cr.BytesRead)
	}
}

func TestDemuxerIncremental(t *testing.T) {
	data, _, _, err := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.H264, Seed: 5, Frames: 9, KeyEvery: 3,
	}, fixture.Fault{})
	if err != nil {
		t.Fatal(err)
	}
	d, err := demux.New(data, 4)
	if err != nil {
		t.Fatal(err)
	}
	if d.Meta().NumAU != 9 || d.Meta().TrackID != 4 {
		t.Fatalf("meta wrong: %+v", d.Meta())
	}
	var got int
	var lastPTS int64 = -1
	for {
		au, err := d.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if au.PTS <= lastPTS {
			t.Fatalf("non-monotonic PTS at AU %d", got)
		}
		lastPTS = au.PTS
		got++
		if d.Position() != got || d.Remaining() != 9-got {
			t.Fatalf("cursor accounting wrong")
		}
	}
	if got != 9 {
		t.Fatalf("emitted %d AUs", got)
	}
}

func TestDemuxerCorruptionOffset(t *testing.T) {
	data, _, keyOffs, _ := fixture.BuildStream(fixture.StreamSpec{
		Codec: codec.AV1, Seed: 5, Frames: 6, KeyEvery: 2,
	}, fixture.Fault{})
	bad := data[:keyOffs[1]+10]
	_, err := demux.New(bad, 1)
	fe, ok := fault.As(err)
	if !ok || fe.Kind != fault.KindCorruptInput || fe.Offset != int64(len(bad)) {
		t.Fatalf("got %+v", err)
	}
}
