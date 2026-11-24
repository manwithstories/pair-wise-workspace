package ingest_test

import (
	"os"
	"testing"

	"mediad/internal/ingest"
	"mediad/internal/model"
	"mediad/internal/synth"
)

func TestScanResumeReproducesUninterruptedSuffix(t *testing.T) {
	spec := synth.DefaultSpec(9, 60)
	spec.CorruptFrames = 1
	dir := t.TempDir()
	path := dir + "/track.mediad"
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := synth.Generate(f, spec, 11); err != nil {
		t.Fatal(err)
	}
	f.Close()

	open := func() *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}

	// Uninterrupted scan for the reference timeline.
	var want []model.Frame
	rf := open()
	if _, err := ingest.Scan(rf, func(fr model.Frame) error {
		want = append(want, fr)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rf.Close()

	// Scan the prefix, capturing the durable checkpoint at frame 25.
	var got []model.Frame
	var cp ingest.Checkpoint
	pf := open()
	if _, err := ingest.ScanResume(pf, nil, func(fr model.Frame) error {
		got = append(got, fr)
		return nil
	}, func(c ingest.Checkpoint) error {
		if c.NextFrame == 25 {
			cp = c
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	pf.Close()
	if cp.NextFrame != 25 {
		t.Fatalf("checkpoint not captured: %+v", cp)
	}

	// Resume from the checkpoint; the suffix must match tick for tick.
	sf := open()
	defer sf.Close()
	var suffix []model.Frame
	sum, err := ingest.ScanResume(sf, &cp.Resume, func(fr model.Frame) error {
		suffix = append(suffix, fr)
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(suffix) != len(want)-25 {
		t.Fatalf("suffix frames = %d, want %d", len(suffix), len(want)-25)
	}
	for i, fr := range suffix {
		ref := want[25+i]
		if fr.Meta.FrameNo != ref.Meta.FrameNo || fr.PTS != ref.PTS {
			t.Fatalf("suffix %d: frame %d pts %d, want %d pts %d", i, fr.Meta.FrameNo, fr.PTS, ref.Meta.FrameNo, ref.PTS)
		}
	}
	// The persisted state must not re-emit earlier frames.
	for _, fr := range suffix {
		if fr.Meta.FrameNo <= 25 {
			t.Fatalf("resumed scan re-emitted frame %d", fr.Meta.FrameNo)
		}
	}
	_ = sum
}
