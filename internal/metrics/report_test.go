package metrics

import (
	"bytes"
	"strings"
	"testing"
)

func TestPercentileAndThresholds(t *testing.T) {
	if got := Percentile([]float64{1, 2, 3, 4, 5}, 0.95); got != 5 {
		t.Fatalf("p95=%v", got)
	}
	s := Stats{RecoveryRate: 0.9, AVSyncP95MS: 12, E2EP95MS: 170}
	for name, ok := range s.Thresholds() {
		if !ok {
			t.Fatalf("threshold %s unexpectedly failed", name)
		}
	}
	s.AVSyncP95MS = 21
	if s.Thresholds()["av_sync_p95_ms<=20"] {
		t.Fatal("AV threshold should fail")
	}
}

func TestPrintKeyValuesContainsAcceptanceFields(t *testing.T) {
	s := Stats{
		Packets: 10, Duplicates: 2, Recovered: 3, RecoverableLoss: 3,
		RecoveryRate: 1, AVSyncP95MS: 4.2, E2EP95MS: 55, OutputSHA256: "abc",
	}
	var buf bytes.Buffer
	s.PrintKeyValues(&buf)
	out := buf.String()
	for _, key := range []string{
		"packets=10", "duplicates=2", "recovered=3", "recoverable_loss=3",
		"recovery_rate=1.000", "av_sync_p95_ms=4.20", "e2e_p95_ms=55.00",
		"first_blocked=none", "output_sha256=abc",
	} {
		if !strings.Contains(out, key) {
			t.Fatalf("output missing %q:\n%s", key, out)
		}
	}
}

func TestHashArtifactStable(t *testing.T) {
	a := Artifact{SchemaVersion: "v1", Seed: 41, E2ESamplesMS: []float64{1, 2, 3}}
	h1, raw1, err := HashArtifact(a)
	if err != nil {
		t.Fatal(err)
	}
	h2, raw2, err := HashArtifact(a)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 || !bytes.Equal(raw1, raw2) {
		t.Fatal("artifact hash/bytes not deterministic")
	}
}
