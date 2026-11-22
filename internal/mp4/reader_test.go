package mp4

import (
	"bytes"
	"testing"

	"mediad/internal/fault"
)

func specs() []SynthSpec {
	return []SynthSpec{
		{Handler: "vide", Timescale: 24000, Count: 24, Delta: 1001, KeyEvery: 6, Seed: 41},
		{Handler: "soun", Timescale: 48000, Count: 48, Delta: 1024, KeyEvery: 0, Seed: 42},
	}
}

func parse(t *testing.T, raw []byte) *File {
	t.Helper()
	f, err := Parse(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("Parse returned fatal error: %v", err)
	}
	return f
}

func TestParseHealthyRoundtrip(t *testing.T) {
	raw, _, err := SynthFile(specs(), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	f := parse(t, raw)
	if f.Truncated || len(f.Faults) != 0 {
		t.Fatalf("healthy file marked faulty: truncated=%v faults=%v", f.Truncated, f.Faults)
	}
	moov := f.Root.Find("moov")
	if moov == nil || len(moov.FindAll("trak")) != 2 {
		t.Fatal("moov/trak structure missing")
	}
	// 未知盒原始字节保留。
	zz := moov.Find("zzzz")
	if zz == nil || string(zz.Payload) != "custom-payload-1234567890" {
		t.Fatalf("unknown box payload not preserved: %+v", zz)
	}
	// 整树重新序列化后与原始字节一致。
	out, err := Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, raw) {
		t.Fatalf("roundtrip differs: got %d bytes, want %d", len(out), len(raw))
	}
}

func TestParseTruncationIsRecoverable(t *testing.T) {
	raw, _, err := SynthFile(specs(), FaultTruncate)
	if err != nil {
		t.Fatal(err)
	}
	f := parse(t, raw)
	if !f.Truncated || len(f.Faults) == 0 {
		t.Fatal("truncation not detected")
	}
	for _, fe := range f.Faults {
		if fe.Kind != fault.KindRecoverable || fe.Code != fault.CodeTruncated {
			t.Fatalf("unexpected fault: %+v", fe)
		}
	}
	// moov 在 mdat 之前，截断尾部后仍可完整解析轨道。
	if len(f.Root.FindAll("trak")) != 2 {
		t.Fatal("parseable boxes must survive tail truncation")
	}
}

func TestBuildSamplesHealthy(t *testing.T) {
	raw, mdat, err := SynthFile(specs(), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	f := parse(t, raw)
	traks := f.Root.Find("moov").FindAll("trak")
	samples, errs := BuildSamples(f, traks[0], "trk-1")
	if len(errs) != 0 {
		t.Fatalf("unexpected sample faults: %v", errs)
	}
	if len(samples) != 24 {
		t.Fatalf("sample count = %d, want 24", len(samples))
	}
	// PTS 单调、时长固定、关键帧按每 6 个样本出现。
	for i, s := range samples {
		if s.Index != uint32(i+1) || s.Duration != 1001 {
			t.Fatalf("sample %d metadata wrong: %+v", i, s)
		}
		if i > 0 && s.PTS <= samples[i-1].PTS {
			t.Fatal("PTS must be strictly increasing")
		}
		if got := s.Keyframe; got != ((i % 6) == 0) {
			t.Fatalf("sample %d keyframe=%v", i, got)
		}
		if !s.Complete {
			t.Fatalf("sample %d marked incomplete on healthy file", i)
		}
	}
	// 样本偏移落在 mdat 内，逐字节可还原。
	first := samples[0]
	got := raw[first.Offset : first.Offset+uint64(first.Size)]
	if !bytes.Equal(got, mdat[:first.Size]) {
		t.Fatal("sample bytes not located correctly")
	}
}

func TestCorruptSampleBoxIsFlagged(t *testing.T) {
	raw, _, err := SynthFile(specs(), FaultCorrupt)
	if err != nil {
		t.Fatal(err)
	}
	f := parse(t, raw)
	traks := f.Root.Find("moov").FindAll("trak")
	if _, errs := BuildSamples(f, traks[0], "trk-1"); len(errs) == 0 {
		t.Fatal("stsz/stts mismatch must produce a recoverable fault")
	} else {
		found := false
		for _, fe := range errs {
			found = found || fe.Code == fault.CodeCorrupt
		}
		if !found {
			t.Fatalf("want corrupt code, got %v", errs)
		}
	}
	// 第二条轨道不受影响——局部故障不扩散。
	if _, errs := BuildSamples(f, traks[1], "trk-2"); len(errs) != 0 {
		t.Fatalf("healthy track contaminated: %v", errs)
	}
}

func TestIndexMissingAndNegativePTS(t *testing.T) {
	raw, _, err := SynthFile(specs(), FaultIndexMissing)
	if err != nil {
		t.Fatal(err)
	}
	f := parse(t, raw)
	traks := f.Root.Find("moov").FindAll("trak")
	if traks[0].Find("stss") != nil {
		t.Fatal("stss must be absent under index-missing fault")
	}
	samples, errs := BuildSamples(f, traks[0], "trk-1")
	if len(errs) != 0 || len(samples) != 24 {
		t.Fatalf("missing stss must not break sample scan: %v", errs)
	}
	for _, s := range samples {
		if s.Keyframe {
			t.Fatal("no keyframe may be fabricated when stss is absent")
		}
	}

	negSpec := []SynthSpec{{
		Handler: "vide", Timescale: 24000, Count: 8, Delta: 1001, KeyEvery: 4, Seed: 7,
	}}
	negRaw, _, err := SynthFile(negSpec, FaultNegativePTS)
	if err != nil {
		t.Fatal(err)
	}
	nf := parse(t, negRaw)
	ns, errs := BuildSamples(nf, nf.Root.Find("moov").FindAll("trak")[0], "trk-1")
	if len(errs) != 0 {
		t.Fatalf("negative PTS fault: %v", errs)
	}
	if ns[0].PTS != -1001 {
		t.Fatalf("first PTS = %d, want -1001", ns[0].PTS)
	}
}

func TestBuildTracksTimebase(t *testing.T) {
	raw, _, err := SynthFile(specs(), FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	f := parse(t, raw)
	tracks, errs := BuildTracks(f)
	if len(errs) != 0 || len(tracks) != 2 {
		t.Fatalf("BuildTracks: %v %v", tracks, errs)
	}
	if tracks[0].Timescale != 24000 || tracks[1].Handler != "soun" {
		t.Fatalf("track metadata wrong: %+v", tracks)
	}
	if tracks[0].Timebase.Num != 24000 {
		t.Fatalf("timebase wrong: %+v", tracks[0].Timebase)
	}
}
