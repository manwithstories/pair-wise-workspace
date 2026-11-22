package worker

import (
	"context"
	"errors"
	"math"
	"testing"

	"mediad/internal/audio"
	"mediad/internal/checkpoint"
	"mediad/internal/fault"
	segmenthash "mediad/internal/hash"
	"mediad/internal/manifest"
	"mediad/internal/probe"
)

const testDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func tone(frames int) []float64 {
	out := make([]float64, frames*2)
	for i := 0; i < frames; i++ {
		v := 0.1 * math.Sin(2*math.Pi*440*float64(i)/48000)
		out[i*2], out[i*2+1] = v, v
	}
	return out
}

func makeInputs(n int, gapEvery int) []SegmentInput {
	inputs := make([]SegmentInput, n)
	for i := 0; i < n; i++ {
		seg := manifest.Segment{
			URI: "seg" + itoa(i) + ".m4s", Sequence: uint64(i + 1), Index: i,
			Start: int64(i) * 180000, Duration: 180000, DurationKnown: true,
			RepresentationID: "a1",
		}
		in := SegmentInput{
			Segment: seg, InputHash: segmenthash.Bytes([]byte(seg.URI)), InputSize: 100,
			Timescales: map[uint32]uint32{1: 48000},
		}
		frag := probe.Fragment{
			Sequence: uint32(i + 1), TrackID: 1, Timescale: 48000, Determined: true,
			BaseTime: uint64(i) * 96000, Samples: []probe.Sample{{Duration: 96000, Sync: true}},
			Duration: 96000, HasMoof: true, HasMdat: true,
		}
		in.Media = &probe.MediaInfo{
			URI: seg.URI, Fragments: []probe.Fragment{frag}, Determined: true, HasMoof: true, HasMdat: true,
			StartTicks: seg.Start, EndTicks: seg.Start + 180000,
		}
		if gapEvery > 0 && i%gapEvery == gapEvery-1 {
			in.Media.Determined = false
			in.Audio = &audio.Source{URI: seg.URI, StartTicks: seg.Start, DurationTicks: 180000, Decodable: false}
		} else {
			in.Audio = &audio.Source{
				URI: seg.URI, StartTicks: seg.Start, DurationTicks: 180000,
				SampleRate: 48000, Channels: 2, Samples: tone(4800), Decodable: true,
			}
		}
		inputs[i] = in
	}
	return inputs
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var digits [8]byte
	n := len(digits)
	for i > 0 {
		n--
		digits[n] = byte('0' + i%10)
		i /= 10
	}
	return string(digits[n:])
}

func TestRunDeterministicAndBounded(t *testing.T) {
	inputs := makeInputs(20, 0)
	opts := DefaultOptions()
	opts.ChunkSize, opts.Workers = 8, 4
	first, err := Run(context.Background(), inputs, opts, testDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Run(context.Background(), inputs, opts, testDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Outputs) != 20 || first.Chunks != 3 {
		t.Fatalf("outputs=%d chunks=%d", len(first.Outputs), first.Chunks)
	}
	for i := range first.Outputs {
		a, b := first.Outputs[i], second.Outputs[i]
		if a.URI != b.URI || a.OutputHash != b.OutputHash || a.StartTicks != b.StartTicks {
			t.Fatalf("run not deterministic at %d: %+v vs %+v", i, a, b)
		}
	}
	if first.Outputs[0].URI != "seg0.m4s" || first.Outputs[19].URI != "seg19.m4s" {
		t.Fatal("outputs are not in segment order")
	}
	if first.SegmentsOK() != 20 || first.SegmentsBad() != 0 {
		t.Fatalf("ok/bad = %d/%d", first.SegmentsOK(), first.SegmentsBad())
	}
	if hits, det, _ := first.KeyIndex.Stats(); hits != 20 || det != 20 || first.KeyIndex.HitRate() != 100 {
		t.Fatalf("index stats = %d/%d", hits, det)
	}
}

func TestResumeSkipsCompletedChunks(t *testing.T) {
	inputs := makeInputs(20, 0)
	opts := DefaultOptions()
	opts.ChunkSize, opts.Workers = 8, 3
	full, err := Run(context.Background(), inputs, opts, testDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := CheckpointFromResult(full, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the last chunk to simulate an interrupted run.
	prior.Chunks = prior.Chunks[:len(prior.Chunks)-1]
	resumed, err := Run(context.Background(), inputs, opts, testDigest, prior)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Recovered != 2 {
		t.Fatalf("recovered chunks = %d", resumed.Recovered)
	}
	if len(resumed.Outputs) != 20 {
		t.Fatalf("merged outputs = %d", len(resumed.Outputs))
	}
	for i := range full.Outputs {
		if full.Outputs[i].OutputHash != resumed.Outputs[i].OutputHash {
			t.Fatalf("resumed output %d differs", i)
		}
	}
}

func TestResumeRejectsChangedInput(t *testing.T) {
	inputs := makeInputs(4, 0)
	opts := DefaultOptions()
	opts.ChunkSize = 2
	full, err := Run(context.Background(), inputs, opts, testDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := CheckpointFromResult(full, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	other := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := Run(context.Background(), inputs, opts, other, prior); !errors.Is(err, checkpoint.ErrInputChanged) {
		t.Fatalf("expected input changed, got %v", err)
	}
}

func TestGapsAndFaultPropagation(t *testing.T) {
	inputs := makeInputs(8, 4)
	opts := DefaultOptions()
	opts.ChunkSize, opts.Workers = 4, 2
	res, err := Run(context.Background(), inputs, opts, testDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.TimelineGaps() != 2 {
		t.Fatalf("gaps = %d", res.TimelineGaps())
	}
	if res.SegmentsBad() != 2 {
		t.Fatalf("bad = %d", res.SegmentsBad())
	}
	if !res.KeyIndex.Incomplete {
		t.Fatal("index should be incomplete")
	}
	var undecodable int
	for _, f := range res.Faults {
		if f.Kind == fault.KindUndecodableAudio {
			undecodable++
		}
	}
	if undecodable == 0 {
		t.Fatal("undecodable faults did not propagate")
	}
	// Gap outputs keep the manifest position.
	if res.Outputs[3].StartTicks != 3*180000 {
		t.Fatalf("gap start = %d", res.Outputs[3].StartTicks)
	}
}

func TestProbeFaultsPreserved(t *testing.T) {
	inputs := makeInputs(1, 0)
	inputs[0].ProbeFaults = []fault.Fault{
		fault.MustNew(fault.KindBoxOverflow, fault.Location{Segment: "seg0.m4s", Offset: 42}, "overflow", true, false),
	}
	res, err := Run(context.Background(), inputs, DefaultOptions(), testDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Faults) != 1 || res.Faults[0].Loc.Offset != 42 {
		t.Fatalf("faults = %+v", res.Faults)
	}
}

func TestCheckpointMergeKeepsPriorChunks(t *testing.T) {
	inputs := makeInputs(8, 0)
	opts := DefaultOptions()
	opts.ChunkSize = 4
	full, err := Run(context.Background(), inputs, opts, testDigest, nil)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := CheckpointFromResult(full, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	partial := &Result{ManifestSHA: testDigest, Chunks: 2, Outputs: full.Outputs[:4]}
	merged, err := CheckpointFromResult(partial, opts, prior)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Chunks) != 2 {
		t.Fatalf("chunks = %d", len(merged.Chunks))
	}
	if merged.CompletedSegments() != 8 {
		t.Fatalf("segments = %d", merged.CompletedSegments())
	}
}
