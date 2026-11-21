package segment

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/model"
)

func evidence(track model.TrackRole, idx, startMS, durMS int64, sync bool, report *WalkReport) Evidence {
	seg := model.SegmentRef{
		Track: track, Index: int(idx), URI: "f.m4s",
		StartMS: startMS, DurationMS: durMS, ByteSize: 10, Sync: sync,
	}
	return Evidence{Segment: seg, Report: report}
}

func TestPlanCuts_AlignedWindow(t *testing.T) {
	ev := []Evidence{
		evidence(model.TrackStudio, 0, 0, 2000, true, nil),
		evidence(model.TrackStudio, 1, 2000, 2000, true, nil),
		evidence(model.TrackStudio, 2, 4000, 2000, true, nil),
	}
	cuts, err := PlanCuts(ev, 2000, 6000)
	if err != nil {
		t.Fatalf("PlanCuts: %v", err)
	}
	if len(cuts) != 2 || cuts[0].Index != 1 || cuts[1].Index != 2 {
		t.Fatalf("cuts = %+v", cuts)
	}
	if !cuts[0].Keyframe {
		t.Fatal("aligned cut must be keyframe-true")
	}
}

func TestPlanCuts_RejectsInFragmentBoundary(t *testing.T) {
	ev := []Evidence{
		evidence(model.TrackField, 0, 0, 6000, true, nil),
	}
	_, err := PlanCuts(ev, 3000, 6000)
	if !errors.Is(err, model.ErrBoundaryUncertain) {
		t.Fatalf("want ErrBoundaryUncertain, got %v", err)
	}
}

func TestPlanCuts_RejectsNonSyncEvidence(t *testing.T) {
	ev := []Evidence{
		evidence(model.TrackVoice, 0, 0, 2000, true, &WalkReport{NonSync: true}),
		evidence(model.TrackVoice, 1, 2000, 2000, true, nil),
	}
	_, err := PlanCuts(ev, 0, 4000)
	if !errors.Is(err, model.ErrBoundaryUncertain) {
		t.Fatalf("non-sync tfhd must block the cut, got %v", err)
	}
}

func TestPlanCuts_InvalidWindow(t *testing.T) {
	if _, err := PlanCuts(nil, -1, 10); err == nil {
		t.Fatal("negative window start must fail")
	}
	if _, err := PlanCuts(nil, 10, 10); err == nil {
		t.Fatal("empty window must fail")
	}
	ev := []Evidence{evidence(model.TrackStudio, 0, 0, 1000, true, nil)}
	if _, err := PlanCuts(ev, 5000, 6000); !errors.Is(err, model.ErrBoundaryUncertain) {
		t.Fatalf("window without evidence must fail, got %v", err)
	}
}

func TestExtract_ByteRangeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.m4s")
	payload := bytes.Repeat([]byte("MEDIA"), 20)
	if err := os.WriteFile(path, append([]byte("PREFIX-PREFIX-"), payload...), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Cut{Track: model.TrackStudio, Index: 0, ByteStart: 14, ByteSize: int64(len(payload))}
	var out bytes.Buffer
	n, err := Extract(path, c, &out)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if n != int64(len(payload)) || !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("extracted %d bytes, mismatch", n)
	}
}

func TestValidate_MissingFileIsFatal(t *testing.T) {
	seg := model.SegmentRef{Track: model.TrackStudio, Index: 0, URI: "/nonexistent/x.m4s"}
	ev, err := Validate(seg.URI, seg)
	if err == nil {
		t.Fatal("missing file must error")
	}
	if ev != nil {
		t.Fatal("no partial evidence for an unopenable file")
	}
	var me *model.MediaError
	if !errors.As(err, &me) || me.Recoverable {
		t.Fatalf("missing file is an unrecoverable MediaError, got %v", err)
	}
}
