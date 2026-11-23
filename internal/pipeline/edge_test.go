package pipeline

import (
	"os"
	"path/filepath"
	"testing"

	"archive.local/archive/internal/model"
)

func TestEditListOutOfRangeSurfacesAsDiagnosis(t *testing.T) {
	work := buildWorkspace(t, false)
	// Rewrite the manifest so an edit run overshoots the declared duration.
	mf := `track 1 reels/feature.mp4 90000 10800000 video 12960000` + "\n" +
		`edit 1 10800000 99999999 1 1` + "\n"
	if err := os.WriteFile(filepath.Join(work, "material.txt"), []byte(mf), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := run(t, work, false)
	if res == nil {
		t.Fatal("nil result")
	}
	if model.CodeOf(res.Defect) != model.CodeEditListRange && model.CodeOf(err) != model.CodeEditListRange {
		t.Fatalf("defect = %+v err=%v", res.Defect, err)
	}
	if res.Defect.Line != 2 || res.Defect.Track != 1 {
		t.Fatalf("location = line %d track %d", res.Defect.Line, res.Defect.Track)
	}
	if res.Status != Damaged {
		t.Fatalf("status = %v", res.Status)
	}
}

func TestInvalidManifestReferenceSurfaces(t *testing.T) {
	work := buildWorkspace(t, false)
	mf := `track 1 missing/absent.mp4 90000 10800000 video 12960000` + "\n"
	if err := os.WriteFile(filepath.Join(work, "material.txt"), []byte(mf), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := run(t, work, false)
	if res == nil || res.Defect == nil {
		t.Fatal("no diagnosis")
	}
	if model.CodeOf(res.Defect) != model.CodeManifestReference {
		t.Fatalf("code = %q", model.CodeOf(res.Defect))
	}
	if res.Defect.Line != 1 || res.Defect.Track != 1 {
		t.Fatalf("location = line %d track %d", res.Defect.Line, res.Defect.Track)
	}
	if res.Status != Damaged {
		t.Fatalf("status = %v", res.Status)
	}
}
