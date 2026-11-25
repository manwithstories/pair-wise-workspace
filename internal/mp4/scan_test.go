package mp4

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// ---- minimal, test-only ISO-BMFF box writer (metadata sufficient for scan) ----

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:], uint32(8+len(payload)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func fullBox(ver byte, flags uint32) []byte {
	return []byte{ver, byte(flags >> 16), byte(flags >> 8), byte(flags)}
}

// mdhdV0 builds a version-0 media header with the given timescale/duration.
func mdhdV0(timescale, duration uint32) []byte {
	p := fullBox(0, 0)
	p = append(p, make([]byte, 8)...)           // creation/modification
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, timescale)
	p = append(p, b...)
	binary.BigEndian.PutUint32(b, duration)
	p = append(p, b...)
	p = append(p, 0x55, 0xC4, 0, 0) // language 'und' + pre-defined
	return box("mdhd", p)
}

// sttsBox builds a decoding time-to-sample box from (count,delta) runs.
func sttsBox(runs [][2]uint32) []byte {
	p := fullBox(0, 0)
	c := make([]byte, 4)
	binary.BigEndian.PutUint32(c, uint32(len(runs)))
	p = append(p, c...)
	for _, rd := range runs {
		e := make([]byte, 8)
		binary.BigEndian.PutUint32(e[0:], rd[0])
		binary.BigEndian.PutUint32(e[4:], rd[1])
		p = append(p, e...)
	}
	return box("stts", p)
}

// stssBox builds a sync-sample box from 1-based keyframe sample numbers.
func stssBox(keys []uint32) []byte {
	p := fullBox(0, 0)
	c := make([]byte, 4)
	binary.BigEndian.PutUint32(c, uint32(len(keys)))
	p = append(p, c...)
	for _, k := range keys {
		e := make([]byte, 4)
		binary.BigEndian.PutUint32(e, k)
		p = append(p, e...)
	}
	return box("stss", p)
}

func buildMP4(t *testing.T, path string, timescale, duration uint32, runs [][2]uint32, keys []uint32) {
	t.Helper()
	stbl := sttsBox(runs)
	if keys != nil {
		stbl = append(stbl, stssBox(keys)...)
	}
	minf := box("minf", box("stbl", stbl))
	mdia := box("mdia", append(mdhdV0(timescale, duration), minf...))
	trak := box("trak", mdia)
	moov := box("moov", trak)
	ftyp := box("ftyp", []byte("isom"+"\x00\x00\x00\x00"+"isomavc1mp41"))
	mdat := box("mdat", []byte("stub-media-payload"))
	data := append(append(ftyp, moov...), mdat...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCFRKeyframeGaps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cfr.mp4")
	// 24 fps, timescale 24000 -> delta 1000 ticks each. 48 samples.
	runs := [][2]uint32{{48, 1000}}
	// keyframes every 12 samples: 1,13,25,37 (1-based) -> 3 gaps of 500ms.
	keys := []uint32{1, 13, 25, 37}
	buildMP4(t, p, 24000, 48000, runs, keys)
	res, err := Scan(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tracks) != 1 {
		t.Fatalf("tracks=%d want 1", len(res.Tracks))
	}
	tr := res.Tracks[0]
	if tr.SampleCount != 48 {
		t.Fatalf("samples=%d want 48", tr.SampleCount)
	}
	if tr.IsVFR {
		t.Fatal("constant delta must not be VFR")
	}
	if len(tr.KeyframeGapsMs) != 3 {
		t.Fatalf("gaps=%d want 3", len(tr.KeyframeGapsMs))
	}
	for _, g := range tr.KeyframeGapsMs {
		if g != 500.0 {
			t.Fatalf("gap=%.2f want 500ms", g)
		}
	}
}

func TestVFRVariableKeyframeGaps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vfr.mp4")
	// timescale 1000 (1 tick = 1ms). Alternating deltas 33ms / 50ms -> VFR.
	// 30 samples: 15 of each delta.
	runs := [][2]uint32{{15, 33}, {15, 50}}
	// keyframes at samples 1 and 10 and 30 (1-based).
	keys := []uint32{1, 10, 30}
	buildMP4(t, p, 1000, 15*33+15*50, runs, keys)
	res, err := Scan(p)
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tracks[0]
	if !tr.IsVFR {
		t.Fatal("mixed deltas must report VFR")
	}
	if len(tr.KeyframeGapsMs) != 2 {
		t.Fatalf("gaps=%d want 2", len(tr.KeyframeGapsMs))
	}
	// Gap 1->10: DTS(index9)-DTS(index0) = 9*33 = 297ms.
	if tr.KeyframeGapsMs[0] != 297.0 {
		t.Fatalf("first gap=%.2f want 297", tr.KeyframeGapsMs[0])
	}
	// Gap 10->30: DTS(index29)-DTS(index9) = (15*33+14*50) - 297 = 898ms.
	if tr.KeyframeGapsMs[1] != 898.0 {
		t.Fatalf("second gap=%.2f want 898", tr.KeyframeGapsMs[1])
	}
	if tr.MaxGapMs != 898.0 || tr.MinGapMs != 297.0 {
		t.Fatalf("min/max wrong: min=%.2f max=%.2f", tr.MinGapMs, tr.MaxGapMs)
	}
}

func TestNoStssMeansAllKeyframes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "nokey.mp4")
	runs := [][2]uint32{{10, 1000}}
	buildMP4(t, p, 24000, 10000, runs, nil)
	res, err := Scan(p)
	if err != nil {
		t.Fatal(err)
	}
	tr := res.Tracks[0]
	for i, s := range tr.Samples {
		if !s.IsKeyframe {
			t.Fatalf("sample %d should be keyframe without stss", i)
		}
	}
}
