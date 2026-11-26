package pipeline

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mediad/internal/verify"
)

func TestRunSelfTestPassesAndIsReproducible(t *testing.T) {
	opts := SelfTestOptions{Seed: 42, Duration: 2 * time.Second, Loss: 0.03, Burst: 7, Out: filepath.Join(t.TempDir(), "run1")}
	r1, err := RunSelfTest(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Normal) != 2 || len(r1.Damaged) < 6 || len(r1.Recovered) < 8 {
		t.Fatalf("summary counts normal=%d damaged=%d recovered=%d", len(r1.Normal), len(r1.Damaged), len(r1.Recovered))
	}
	if failures := r1.Metrics.Failures(); len(failures) != 0 {
		t.Fatalf("metric failures: %v", failures)
	}
	rep, err := verify.VerifyFile(r1.OutputPath)
	if err != nil || !rep.OK {
		t.Fatalf("verify err=%v report=%#v", err, rep)
	}

	opts2 := opts
	opts2.Out = filepath.Join(t.TempDir(), "run2")
	r2, err := RunSelfTest(opts2)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Metrics.OutputSHA256 != r2.Metrics.OutputSHA256 {
		t.Fatalf("hash mismatch %s vs %s", r1.Metrics.OutputSHA256, r2.Metrics.OutputSHA256)
	}
	if r1.Metrics != r2.Metrics {
		t.Fatalf("metrics mismatch: %#v vs %#v", r1.Metrics, r2.Metrics)
	}

	data, err := os.ReadFile(r1.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if got := formatHex(sum[:]); got != r1.Metrics.OutputSHA256 {
		t.Fatalf("file hash %s != metric %s", got, r1.Metrics.OutputSHA256)
	}
}

func formatHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, x := range b {
		out[i*2] = digits[x>>4]
		out[i*2+1] = digits[x&0xf]
	}
	return string(out)
}
