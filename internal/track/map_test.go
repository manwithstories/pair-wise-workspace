package track

import (
	"testing"

	"mediacore/internal/fault"
	"mediacore/internal/sample"
)

func samples(n int, trackIdx int, codec sample.Codec) []*sample.Sample {
	out := make([]*sample.Sample, n)
	for i := range out {
		out[i] = &sample.Sample{TrackIndex: trackIdx, TrackID: uint32(trackIdx + 1),
			Codec: codec, Seq: i}
	}
	return out
}

func TestDefaultMappingStable(t *testing.T) {
	m := DefaultMapping(3)
	if len(m.Order) != 3 || m.Order[0] != 1 || m.Order[2] != 3 {
		t.Fatalf("unexpected order %v", m.Order)
	}
	r := m.Resolve(1, sample.CodecAAC)
	if r.Ignored || r.MappedNumber != 2 || r.Codec != sample.CodecAAC {
		t.Fatalf("resolve failed: %+v", r)
	}
}

func TestRenameAndIgnore(t *testing.T) {
	rules := []Rule{
		{SourceIndex: 0, Action: ActionKeep, MappedNumber: 1, Rename: sample.CodecH264},
		{SourceIndex: 1, Action: ActionIgnore},
		{SourceIndex: 2, Action: ActionKeep, MappedNumber: 2},
	}
	m, err := Build(rules, 3)
	if err != nil {
		t.Fatal(err)
	}
	var in []*sample.Sample
	in = append(in, samples(2, 0, sample.CodecH264)...)
	in = append(in, samples(2, 1, sample.CodecAAC)...)
	in = append(in, samples(2, 2, sample.CodecOpus)...)
	out := m.Apply(in)
	for _, s := range out {
		if s.TrackIndex == 1 {
			t.Fatal("ignored track must be dropped")
		}
		if s.TrackIndex == 2 && s.TrackID != 2 {
			t.Fatalf("mapped number not applied: %+v", s)
		}
		if s.TrackIndex == 0 && s.Codec != sample.CodecH264 {
			t.Fatal("rename failed")
		}
	}
	if len(out) != 4 {
		t.Fatalf("want 4 kept samples, got %d", len(out))
	}
}

func TestMissingSourceAborts(t *testing.T) {
	_, err := Build([]Rule{{SourceIndex: 5, Action: ActionKeep, MappedNumber: 1}}, 3)
	f := fault.As(err)
	if f == nil || f.Recoverable() || f.TrackIndex != 5 {
		t.Fatalf("want aborting missing-track fault, got %v", err)
	}
}

func TestDuplicateMappedNumber(t *testing.T) {
	rules := []Rule{
		{SourceIndex: 0, Action: ActionKeep, MappedNumber: 1},
		{SourceIndex: 1, Action: ActionKeep, MappedNumber: 1},
	}
	if _, err := Build(rules, 2); err == nil {
		t.Fatal("mapped number collision must abort")
	}
}

func TestApplyDeterministic(t *testing.T) {
	m := DefaultMapping(2)
	in := append(samples(3, 0, sample.CodecH264), samples(3, 1, sample.CodecAAC)...)
	a := m.Apply(in)
	b := m.Apply(in)
	if len(a) != len(b) {
		t.Fatal("repeated apply changed count")
	}
	for i := range a {
		if a[i].TrackID != b[i].TrackID || a[i].Seq != b[i].Seq {
			t.Fatal("repeated apply is not deterministic")
		}
	}
}
