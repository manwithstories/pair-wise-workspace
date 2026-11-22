package testdata

import (
	"bytes"
	"testing"

	"fieldcut/internal/parse"
)

func TestGeneratorDeterministic(t *testing.T) {
	g1 := NewRNG(41)
	g2 := NewRNG(41)
	a, samplesA := BuildMP4(g1, DefaultSpecs()[0])
	b, samplesB := BuildMP4(g2, DefaultSpecs()[0])
	if !bytes.Equal(a, b) || len(samplesA) != len(samplesB) {
		t.Fatal("same seed must produce identical bytes")
	}
	g3 := NewRNG(42)
	c, _ := BuildMP4(g3, DefaultSpecs()[0])
	if bytes.Equal(a, c) {
		t.Fatal("different seed must differ")
	}
}

func TestGeneratedMP4Parses(t *testing.T) {
	g := NewRNG(41)
	data, samples := BuildMP4(g, DefaultSpecs()[0])
	roots, rep := parse.Walk(bytes.NewReader(data), parse.DefaultLimits())
	if rep.HasFatal() {
		t.Fatalf("generated mp4 must be healthy: %v", rep.Errors)
	}
	tis, err := parse.ParseTracks(bytes.NewReader(data), roots,
		parse.DefaultLimits(), rep)
	if err != nil || len(tis) != 1 {
		t.Fatalf("parse generated mp4: err=%v tracks=%d", err, len(tis))
	}
	if len(tis[0].Samples) != len(samples) {
		t.Fatalf("sample count mismatch: %d vs %d",
			len(tis[0].Samples), len(samples))
	}
}

func TestCorruptionTruncatedTail(t *testing.T) {
	g := NewRNG(7)
	data, _ := BuildMP4(g, DefaultSpecs()[0])
	// 在 moov 中途截断。
	cut := len(data) - 40
	trunc := TruncateTail(data, cut)
	roots, rep := parse.Walk(bytes.NewReader(trunc), parse.DefaultLimits())
	if !rep.Truncated {
		t.Fatal("truncation must be detected")
	}
	rc := parse.Recover(bytes.NewReader(trunc), roots, int64(len(trunc)), rep)
	if rc.Reclaimed <= 0 || rc.LastGoodBox == "-" {
		t.Fatal("recoverable prefix must be retained")
	}
}

func TestCorruptionBadBox(t *testing.T) {
	g := NewRNG(7)
	data, _ := BuildMP4(g, DefaultSpecs()[0])
	// 定位 moov 盒头并伪造其长度。
	moovOff := indexOf(data, "moov")
	if moovOff < 0 {
		t.Fatal("moov not found")
	}
	bad := CorruptBoxHeader(data, uint32(moovOff), uint32(len(data)+1000))
	roots, rep := parse.Walk(bytes.NewReader(bad), parse.DefaultLimits())
	if len(roots) == 0 {
		t.Fatal("boxes before bad box must survive")
	}
	if len(rep.Boundaries) == 0 {
		t.Fatal("bad box boundary must be recorded")
	}
}

func TestCorruptionIndexGapRebuild(t *testing.T) {
	g := NewRNG(7)
	data, _ := BuildMP4(g, DefaultSpecs()[1]) // camB 默认无 stss
	if indexOf(data, "stss") >= 0 {
		t.Fatal("fixture should lack stss")
	}
	roots, rep := parse.Walk(bytes.NewReader(data), parse.DefaultLimits())
	tis, _ := parse.ParseTracks(bytes.NewReader(data), roots,
		parse.DefaultLimits(), rep)
	if len(tis) != 1 || !tis[0].IndexGap {
		t.Fatal("missing stss must surface as index gap")
	}
	parse.RebuildKeyframeIndex(bytes.NewReader(data), tis[0],
		parse.DefaultLimits(), rep)
	if len(tis[0].SyncSamples()) == 0 {
		t.Fatal("IDR scan must rebuild keyframe index")
	}

	// StripSTSS 对带索引的 camA 同样生效。
	g2 := NewRNG(7)
	full, _ := BuildMP4(g2, DefaultSpecs()[0])
	stripped := StripSTSS(full)
	if indexOf(stripped, "stss") >= 0 {
		t.Fatal("StripSTSS must remove stss box")
	}
}

func TestCorruptionWrapSeries(t *testing.T) {
	series := WrapRTPSeries(90000)
	if len(series) < 2 {
		t.Fatal("series too short")
	}
	wrapped := false
	for i := 1; i < len(series); i++ {
		if series[i].TS < series[i-1].TS {
			wrapped = true
		}
		if series[i].Seq != series[i-1].Seq+1 {
			t.Fatal("sequence must stay monotonic across wrap")
		}
	}
	if !wrapped {
		t.Fatal("series must cross 32-bit wrap")
	}
}

func TestEmsgGeneration(t *testing.T) {
	raw := BuildEmsg(2000, 1000, 500, 9, "cue")
	roots, rep := parse.Walk(bytes.NewReader(raw), parse.DefaultLimits())
	if rep.HasFatal() {
		t.Fatal(rep.Errors)
	}
	evs := parse.ParseEvents(bytes.NewReader(raw), roots, nil, rep)
	if len(evs) != 1 || evs[0].EventID != 9 || evs[0].Text != "cue" {
		t.Fatalf("generated emsg parse wrong: %+v", evs)
	}
}

func indexOf(data []byte, typ string) int {
	needle := []byte(typ)
	for i := 0; i+8 <= len(data); i++ {
		if bytes.Equal(data[i+4:i+8], needle) {
			return i
		}
	}
	return -1
}
