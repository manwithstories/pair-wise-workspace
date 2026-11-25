package fieldpost_test

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"fieldpost/internal/demo"
)

func TestIntegrationDemoMetricsAndDeterminism(t *testing.T) {
	ctx := context.Background()
	capBytes := int64(8388608)
	r1, err := demo.Run(ctx, demo.Options{Seed: 42, Profile: "all", MemoryCap: capBytes, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := demo.Run(ctx, demo.Options{Seed: 42, Profile: "all", MemoryCap: capBytes, OutputDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if r1.HealthyRoundtrip != 1 || r2.HealthyRoundtrip != 1 {
		t.Fatalf("roundtrip %f %f", r1.HealthyRoundtrip, r2.HealthyRoundtrip)
	}
	if r1.EssenceOK != 12 || r1.EssenceTotal != 12 {
		t.Fatalf("essence=%d/%d", r1.EssenceOK, r1.EssenceTotal)
	}
	if r1.KeyframeHit != 1 || r1.KeyHits != r1.KeyTotal {
		t.Fatalf("key hit=%f %d/%d", r1.KeyframeHit, r1.KeyHits, r1.KeyTotal)
	}
	if r1.PeakMemBytes > capBytes || r2.PeakMemBytes > capBytes {
		t.Fatalf("peak %d/%d cap %d", r1.PeakMemBytes, r2.PeakMemBytes, capBytes)
	}
	if r1.BackpressureEvents == 0 || r1.BoundaryRejected < 4 {
		t.Fatalf("bp=%d boundary=%d", r1.BackpressureEvents, r1.BoundaryRejected)
	}
	if r1.Unrecoverable != 1 || r1.UnrecoverableOutputs != 0 {
		t.Fatalf("unrecoverable=%d outputs=%d", r1.Unrecoverable, r1.UnrecoverableOutputs)
	}
	if len(r1.Healthy) != len(r2.Healthy) {
		t.Fatal("healthy result length differs")
	}
	for i := range r1.Healthy {
		a, b := r1.Healthy[i], r2.Healthy[i]
		if a.ID != b.ID || a.Hash != b.Hash || a.KeyHits != b.KeyHits || a.KeyTotal != b.KeyTotal {
			t.Fatalf("nondeterministic result %d: %+v vs %+v", i, a, b)
		}
	}
}

func TestCLIInvalidProfileExitsTwo(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fieldpost")
	build := exec.Command("go", "build", "-o", bin, "./cmd/fieldpost")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}
	cmd := exec.Command(bin, "demo", "--profile", "bogus")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("invalid profile succeeded")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("unexpected error %v\n%s", err, out)
	}
	if exitErr.ExitCode() != 2 {
		t.Fatalf("exit=%d output=%s", exitErr.ExitCode(), out)
	}
}
