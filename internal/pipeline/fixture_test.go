package pipeline

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/model"
	"mediad/internal/segment"
)

func TestGenerate_HealthyAllWalkable(t *testing.T) {
	l, err := Generate(t.TempDir(), true)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(l.Corrupt) != 0 {
		t.Fatalf("healthy tree must have no corruptions, got %d", len(l.Corrupt))
	}
	if len(l.Manifests) != 3 {
		t.Fatalf("want 3 manifests, got %v", l.Manifests)
	}
	for role, names := range l.Segments {
		dir := filepath.Join(l.Root, filepath.Dir(l.Manifests[role]))
		for i, name := range names {
			seg := model.SegmentRef{Track: role, Index: i, Sync: true}
			ev, err := segment.Validate(filepath.Join(dir, name), seg)
			if err != nil {
				t.Fatalf("healthy fragment %s/%s: %v", role, name, err)
			}
			if !ev.Report.HasMoof {
				t.Fatalf("%s/%s missing moof", role, name)
			}
		}
	}
}

func TestGenerate_CorruptionsAtExactOffsets(t *testing.T) {
	l, err := Generate(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Corrupt) != 2 {
		t.Fatalf("want 2 injected defects, got %d", len(l.Corrupt))
	}
	for _, c := range l.Corrupt {
		data, err := os.ReadFile(c.Path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = segment.WalkStream(bytes.NewReader(data), int64(len(data)), c.Track, c.Index)
		var me *model.MediaError
		if !errors.As(err, &me) {
			t.Fatalf("%s: want MediaError, got nil", c.Kind)
		}
		if !me.Recoverable {
			t.Fatalf("%s: defect must be recoverable", c.Kind)
		}
		if diff := me.Offset - c.ExpectOffset; diff > 1 || diff < -1 {
			t.Fatalf("%s: offset %d, want %d ±1", c.Kind, me.Offset, c.ExpectOffset)
		}
	}
}

func TestGenerate_PCMHasSilenceWindow(t *testing.T) {
	pcm := genPCM(segMS, sampleRate, 6000, 9200)
	silent := 0
	for _, v := range pcm {
		if v == 0 {
			silent++
		}
	}
	// 3200 silent samples = 400 ms at 8 kHz.
	if silent != 3200 {
		t.Fatalf("silent samples = %d, want 3200 (400 ms)", silent)
	}
}
