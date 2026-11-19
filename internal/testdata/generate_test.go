package testdata

import (
	"testing"

	"mediapipeline/internal/stream"
)

func TestGenerateDeterminism(t *testing.T) {
	s := DefaultSpec(42)
	f1 := Generate(s)
	f2 := Generate(s)
	if len(f1.Datagrams) != len(f2.Datagrams) {
		t.Fatal("length differs")
	}
	for i := range f1.Datagrams {
		if string(f1.Datagrams[i].Raw) != string(f2.Datagrams[i].Raw) {
			t.Fatalf("datagram %d differs for fixed seed", i)
		}
	}
	if f1.CleanAudio != s.AudioPkts || f1.CleanVideo != s.VideoPkts {
		t.Fatalf("clean counts wrong: %d/%d vs %d/%d",
			f1.CleanAudio, f1.CleanVideo, s.AudioPkts, s.VideoPkts)
	}
}

func TestGenerateOffsetsAndArrival(t *testing.T) {
	f := Generate(DefaultSpec(7))
	var offset int64
	for i, d := range f.Datagrams {
		if d.Offset != offset {
			t.Fatalf("datagram %d offset=%d want %d", i, d.Offset, offset)
		}
		if d.Arrival != int64(i) {
			t.Fatalf("datagram %d arrival=%d", i, d.Arrival)
		}
		offset += int64(len(d.Raw))
	}
	if f.TotalOffset != offset {
		t.Fatalf("total offset=%d want %d", f.TotalOffset, offset)
	}
}

func TestBoundarySpecs(t *testing.T) {
	specs := BoundarySpecs(42)
	wrap := specs["sequence_wrap"]
	if wrap.AudioSeqStart != 0xFFFC || wrap.VideoSeqStart != 0xFFFC {
		t.Fatalf("wrap fixture must start at 0xFFFC, got %x/%x",
			wrap.AudioSeqStart, wrap.VideoSeqStart)
	}
	f := Generate(wrap)
	// The first surviving audio packet must parse with sequence 0xFFFC.
	var firstSeq uint16
	found := false
	for _, d := range f.Datagrams {
		if d.IsRTCP || d.Track != wrap.AudioSSRC {
			continue
		}
		p, merr := stream.ParseRTP(d.Raw, d.Offset, d.Arrival)
		if merr != nil {
			t.Fatalf("wrap packet parse: %v", merr)
		}
		firstSeq, found = p.Sequence, true
		break
	}
	if !found || firstSeq != 0xFFFC {
		t.Fatalf("first wrap seq=%x found=%v", firstSeq, found)
	}
	empty := Generate(specs["empty_stream"])
	// Only the trailing valid RTCP control datagram remains.
	if len(empty.Datagrams) != 1 || !empty.Datagrams[0].IsRTCP {
		t.Fatalf("empty stream must contain only RTCP, got %d", len(empty.Datagrams))
	}
	nokey := Generate(specs["no_keyframe"])
	if nokey == nil || len(nokey.Datagrams) == 0 {
		t.Fatal("no-keyframe fixture must still contain packets")
	}
}

func TestEmbeddedCorruptionsAreLocated(t *testing.T) {
	s := DefaultSpec(42)
	s.AudioPkts = 20
	s.VideoPkts = 8
	s.Corruptions = []Corruption{
		{Track: s.AudioSSRC, SeqIndex: 3, Kind: CorruptTruncated},
		{Track: s.VideoSSRC, SeqIndex: 2, Kind: CorruptBadVersion},
		{Track: 0, SeqIndex: 0, Kind: CorruptBadRTCP},
		{Track: s.AudioSSRC, SeqIndex: 9, Kind: CorruptDuplicateConflict},
	}
	f := Generate(s)
	var truncated, badVer, badRTCP, dup int
	for _, d := range f.Datagrams {
		if !d.HasCorrupt {
			continue
		}
		switch d.Corrupt {
		case CorruptTruncated:
			truncated++
			if _, merr := stream.ParseRTP(d.Raw, d.Offset, d.Arrival); merr == nil {
				t.Fatal("truncated datagram must fail RTP parsing")
			}
		case CorruptBadVersion:
			badVer++
			if _, merr := stream.ParseRTP(d.Raw, d.Offset, d.Arrival); merr == nil {
				t.Fatal("bad-version datagram must fail RTP parsing")
			}
		case CorruptBadRTCP:
			badRTCP++
			if merr := stream.ParseRTCP(d.Raw); merr == nil {
				t.Fatal("bad RTCP must fail validation")
			}
		case CorruptDuplicateConflict:
			dup++
		}
	}
	if truncated != 1 || badVer != 1 || badRTCP != 1 || dup != 1 {
		t.Fatalf("corruption counts wrong: trunc=%d ver=%d rtcp=%d dup=%d",
			truncated, badVer, badRTCP, dup)
	}
}

func TestLossAndJitterReduceOrReorder(t *testing.T) {
	s := DefaultSpec(42)
	s.AudioPkts = 100
	s.VideoPkts = 25
	s.LossRate = 0.5
	s.JitterSwaps = 5
	f := Generate(s)
	if f.CleanAudio >= s.AudioPkts {
		t.Fatalf("loss did not reduce audio count: %d", f.CleanAudio)
	}
	// Reordering must not lose or duplicate datagrams.
	if f.CleanAudio+f.CleanVideo+1 != len(f.Datagrams) {
		t.Fatalf("jitter changed datagram count: %d vs %d",
			f.CleanAudio+f.CleanVideo+1, len(f.Datagrams))
	}
}
