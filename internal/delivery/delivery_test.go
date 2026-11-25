package delivery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/abema/go-mp4"

	"cinepipe/internal/index"
	"cinepipe/internal/synth"
)

func produceSample(t *testing.T, kind synth.SampleKind, opts Options) (*index.Index, *Result, string) {
	t.Helper()
	s := synth.Build(kind, synth.Defaults(kind, 42))
	idx, units := index.RunWithSamples(s.Data)
	opts.OutDir = t.TempDir()
	res, err := Produce(idx, units, opts)
	if err != nil {
		t.Fatalf("produce: %v", err)
	}
	return idx, res, opts.OutDir
}

// boxVisitor records box types and, for trun, its sample count.
type boxVisitor struct {
	types       map[string]int
	sampleCount uint32
}

func visitBoxes(data []byte) (*boxVisitor, error) {
	v := &boxVisitor{types: map[string]int{}}
	_, err := mp4.ReadBoxStructure(bytes.NewReader(data), func(h *mp4.ReadHandle) (interface{}, error) {
		v.types[h.BoxInfo.Type.String()]++
		if h.BoxInfo.Type == mp4.BoxTypeTrun() {
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			if trun, ok := box.(*mp4.Trun); ok {
				v.sampleCount += trun.SampleCount
			}
		}
		return nil, nil
	})
	return v, err
}

func TestProduceWritesParseableInit(t *testing.T) {
	_, res, dir := produceSample(t, synth.SampleNormal, Options{TargetKBPS: 1800})
	initPath := filepath.Join(dir, res.InitFiles[0])
	data, err := os.ReadFile(initPath)
	if err != nil {
		t.Fatal(err)
	}
	v, err := visitBoxes(data)
	if err != nil {
		t.Fatalf("init not parseable: %v", err)
	}
	if v.types["ftyp"] != 1 || v.types["moov"] != 1 {
		t.Fatalf("init missing core boxes: %v", v.types)
	}
	if v.types["trak"] < 1 || v.types["avc1"] != 1 || v.types["mp4a"] != 1 {
		t.Fatalf("init sample descriptions missing: %v", v.types)
	}
	if v.types["avcC"] != 1 {
		t.Fatalf("init missing avcC: %v", v.types)
	}
}

func TestMediaSegmentsParseAndCarrySamples(t *testing.T) {
	_, res, dir := produceSample(t, synth.SampleNormal, Options{TargetKBPS: 1800})
	if len(res.VideoSegments) == 0 {
		t.Fatal("no video segments produced")
	}
	for _, seg := range res.VideoSegments {
		data, err := os.ReadFile(filepath.Join(dir, seg.File))
		if err != nil {
			t.Fatal(err)
		}
		v, err := visitBoxes(data)
		if err != nil {
			t.Fatalf("segment %s not parseable: %v", seg.File, err)
		}
		if v.types["moof"] != 1 || v.types["mdat"] != 1 {
			t.Fatalf("segment %s missing moof/mdat: %v", seg.File, v.types)
		}
		if v.sampleCount == 0 {
			t.Fatalf("segment %s has no samples", seg.File)
		}
		if v.types["tfdt"] != 1 || v.types["trun"] != 1 {
			t.Fatalf("segment %s missing fragment timing: %v", seg.File, v.types)
		}
	}
}

func TestSegmentBoundariesOnKeyframes(t *testing.T) {
	idx, res, _ := produceSample(t, synth.SampleNormal, Options{TargetKBPS: 1800})
	video := idx.Tracks[0]
	keyTicks := map[int64]bool{}
	for _, k := range video.Keyframes {
		if k.Keyframe {
			keyTicks[k.Ticks] = true
		}
	}
	for _, seg := range res.VideoSegments {
		if !keyTicks[seg.StartTicks] {
			t.Fatalf("segment %s starts at %d, not a keyframe time", seg.File, seg.StartTicks)
		}
	}
}

func TestManifestRecordsGapsAndRecovery(t *testing.T) {
	idx, res, dir := produceSample(t, synth.SampleContinuity, Options{TargetKBPS: 1800})
	if len(idx.Ranges) < 2 {
		t.Fatalf("expected damaged ranges: %d", len(idx.Ranges))
	}
	if len(res.Manifest.Gaps) == 0 {
		t.Fatal("manifest should record gaps for damaged ranges")
	}
	video := idx.Tracks[0]
	keyTicks := map[int64]bool{}
	for _, k := range video.Keyframes {
		if k.Keyframe {
			keyTicks[k.Ticks] = true
		}
	}
	for _, seg := range res.VideoSegments {
		if !keyTicks[seg.StartTicks] {
			t.Fatalf("segment %s starts at non-keyframe %d", seg.File, seg.StartTicks)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if res.Manifest.OutputSHA256 == "" {
		t.Fatal("manifest missing output digest")
	}
	// A recovered gap must name a real byte offset past its own start.
	for _, g := range res.Manifest.Gaps {
		if g.RecoveredAt >= 0 && g.RecoveredAt < g.StartByte {
			t.Fatalf("gap recovery %d precedes start %d", g.RecoveredAt, g.StartByte)
		}
	}
}

func TestAlignmentReported(t *testing.T) {
	_, res, _ := produceSample(t, synth.SampleNormal, Options{TargetKBPS: 1800})
	if res.Manifest.AlignmentMaxMS > 1.0 {
		t.Fatalf("alignment %f ms exceeds 1 ms budget", res.Manifest.AlignmentMaxMS)
	}
}

func TestTargetBitrateReported(t *testing.T) {
	_, res, _ := produceSample(t, synth.SampleNormal, Options{TargetKBPS: 1800})
	if res.Manifest.ActualKBPS <= 0 {
		t.Fatal("actual bitrate not computed")
	}
	if res.Manifest.DurationTicks <= 0 {
		t.Fatal("duration not computed")
	}
}

func TestManifestDeterministic(t *testing.T) {
	s := synth.Build(synth.SampleNormal, synth.Defaults(synth.SampleNormal, 42))
	idx1, units1 := index.RunWithSamples(s.Data)
	dir1 := t.TempDir()
	r1, err := Produce(idx1, units1, Options{TargetKBPS: 1800, OutDir: dir1})
	if err != nil {
		t.Fatal(err)
	}
	idx2, units2 := index.RunWithSamples(s.Data)
	dir2 := t.TempDir()
	r2, err := Produce(idx2, units2, Options{TargetKBPS: 1800, OutDir: dir2})
	if err != nil {
		t.Fatal(err)
	}
	if r1.Manifest.OutputSHA256 != r2.Manifest.OutputSHA256 {
		t.Fatalf("output digest differs across identical runs: %s vs %s",
			r1.Manifest.OutputSHA256, r2.Manifest.OutputSHA256)
	}
	m1, _ := os.ReadFile(filepath.Join(dir1, "manifest.json"))
	m2, _ := os.ReadFile(filepath.Join(dir2, "manifest.json"))
	if string(m1) != string(m2) {
		t.Fatal("manifest bytes differ across identical runs")
	}
}

func TestMetadataTrackDescriptionsPreserved(t *testing.T) {
	idx, res, _ := produceSample(t, synth.SampleMetadata, Options{TargetKBPS: 1800})
	audio := idx.Tracks[1]
	if len(audio.Descriptions) == 0 {
		t.Fatal("metadata sample should carry track descriptions")
	}
	found := false
	for _, mt := range res.Manifest.Tracks {
		if mt.Kind != "audio" {
			continue
		}
		for _, d := range mt.Descriptions {
			if d.Key == audio.Descriptions[0].Key {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("manifest did not preserve the audio description key")
	}
}
