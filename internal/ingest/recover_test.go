package ingest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"presspack/internal/adapter"
	"presspack/internal/core"
)

func renderTS(t *testing.T, seconds int) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	path := filepath.Join(t.TempDir(), "cam.ts")
	out, err := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=160x120:rate=25:duration="+itoa(seconds),
		"-c:v", "libx264", "-g", "25", "-pix_fmt", "yuv420p",
		"-f", "mpegts", path).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	return path
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestBuildPlanHealthy(t *testing.T) {
	src := renderTS(t, 2)
	if err := os.WriteFile(src+".idx", []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), adapter.FFprobeV1{}, "cam1", src, core.FromMillis(2000))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Faults) != 0 {
		t.Errorf("unexpected faults: %v", plan.Faults)
	}
	if plan.Duration.Millis() < 1900 || plan.Duration.Millis() > 2100 {
		t.Errorf("duration = %v", plan.Duration)
	}
	if len(plan.Gaps) != 0 || len(plan.Records) != 0 {
		t.Errorf("healthy plan has gaps/records: %+v %+v", plan.Gaps, plan.Records)
	}
}

func TestBuildPlanMissingIndexRecovers(t *testing.T) {
	src := renderTS(t, 2) // no .idx sidecar
	plan, err := BuildPlan(context.Background(), adapter.FFprobeV1{}, "cam2", src, core.FromMillis(2000))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(plan.Faults.OfKind(core.FaultMissingIndex)); n != 1 {
		t.Fatalf("want missing-index fault, got %v", plan.Faults)
	}
	if len(plan.Samples) == 0 {
		t.Error("scan must still recover samples")
	}
	if len(plan.Records) != 1 || plan.Records[0].Reason != string(core.FaultMissingIndex) {
		t.Errorf("missing-index scan must be recorded: %+v", plan.Records)
	}
	if len(plan.Records) > 0 && len(plan.Records[0].Hash) != 64 {
		t.Errorf("record hash = %q", plan.Records[0].Hash)
	}
}

func TestBuildPlanTruncatedMarksGap(t *testing.T) {
	full := renderTS(t, 4)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatal(err)
	}
	trunc := filepath.Join(t.TempDir(), "trunc.ts")
	half := data[:len(data)/2]
	if err := os.WriteFile(trunc, half, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trunc+".idx", []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), adapter.FFprobeV1{}, "cam3", trunc, core.FromMillis(4000))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Records) != 1 {
		t.Fatalf("want 1 recovery record, got %+v", plan.Records)
	}
	rec := plan.Records[0]
	if rec.OriginalOffset <= 0 || rec.OriginalOffset > int64(len(half)) {
		t.Errorf("record offset = %d", rec.OriginalOffset)
	}
	if rec.Reason != string(core.FaultCorruptMedia) {
		t.Errorf("reason = %q", rec.Reason)
	}
	if len(rec.Hash) != 64 {
		t.Errorf("hash = %q", rec.Hash)
	}
	if len(plan.Gaps) != 1 || plan.Gaps[0].End.Cmp(plan.Gaps[0].Start) <= 0 {
		t.Errorf("gaps = %+v", plan.Gaps)
	}
}

func TestBuildPlanUnrecoverableFailsAlone(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "junk.mp4")
	if err := os.WriteFile(junk, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context.Background(), adapter.FFprobeV1{}, "camX", junk, core.FromMillis(1000))
	if !errors.Is(err, ErrUnrecoverable) {
		t.Fatalf("err = %v", err)
	}
	if !plan.Faults.HasUnrecoverable() {
		t.Errorf("faults = %v", plan.Faults)
	}
}
