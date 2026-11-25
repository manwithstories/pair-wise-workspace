package lab

import (
	"context"
	"testing"
)

func TestRunMetrics(t *testing.T) {
	m, err := Run(context.Background(), Config{
		Seed: 17, Workers: 4, Queue: 64, Packets: 4096, Repeat: 3,
		Thresholds: DefaultThresholds,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Logf("metrics: %+v", m)
	if len(m.Failures) != 0 {
		t.Fatalf("threshold failures: %v", m.Failures)
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	if _, err := Run(context.Background(), Config{
		Seed: 1, Workers: 0, Queue: 64, Packets: 10, Repeat: 1,
	}); err == nil {
		t.Fatal("zero workers accepted")
	}
	if _, err := Run(context.Background(), Config{
		Seed: 1, Workers: 4, Queue: 1 << 20, Packets: 10, Repeat: 1,
	}); err == nil {
		t.Fatal("oversized queue accepted")
	}
}
