package metrics

import (
	"errors"
	"testing"

	"fieldcut/internal/media"
)

func valid() Report {
	return Report{SyncMaxMs: 1.4, RecoveryRate: 1.0, Eligible: 6, Recovered: 6,
		P95FrameMs: 12, Frames: 240, RootSHA256: "deadbeef", ResumeEqual: true, Verify: "PASS"}
}

func TestCheckAcceptsBounds(t *testing.T) {
	if err := valid().Check(DefaultLimits); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
}

func TestCheckRejectsViolations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Report)
	}{
		{"sync", func(r *Report) { r.SyncMaxMs = 2.5 }},
		{"recovery", func(r *Report) { r.RecoveryRate = 0.90 }},
		{"p95", func(r *Report) { r.P95FrameMs = 140 }},
		{"frames", func(r *Report) { r.Frames = 0 }},
		{"resume", func(r *Report) { r.ResumeEqual = false }},
		{"verify", func(r *Report) { r.Verify = "FAIL" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := valid()
			tc.mutate(&r)
			err := r.Check(DefaultLimits)
			if err == nil {
				t.Fatal("violation accepted")
			}
			var me *media.MediaError
			if !errors.As(err, &me) || me.Kind != media.KindMetric {
				t.Fatalf("kind = %v (%v)", me, err)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	vals := []float64{5, 1, 3, 4, 2}
	if got := Percentile(vals, 50); got != 3 {
		t.Fatalf("p50 = %v", got)
	}
	if got := Percentile(vals, 95); got <= 4 || got > 5 {
		t.Fatalf("p95 = %v", got)
	}
	if Percentile(nil, 95) != 0 || Percentile(vals, 100) != 5 || Percentile(vals, 0) != vals[0] {
		t.Fatal("percentile edges wrong")
	}
}

func TestSyncMax(t *testing.T) {
	if got := SyncMax([]float64{0.4, 1.9, 0.8}); got != 1.9 {
		t.Fatalf("sync max = %v", got)
	}
	if SyncMax(nil) != 0 {
		t.Fatal("empty sync max")
	}
}

func TestLineShape(t *testing.T) {
	line := valid().Line()
	if len(line) == 0 {
		t.Fatal("empty line")
	}
	for _, key := range []string{"sync_max_ms=", "recovery_rate=", "eligible=", "recovered=", "p95_frame_ms=", "frames=", "root_sha256=", "resume_equal=true", "verify=PASS"} {
		if !contains(line, key) {
			t.Fatalf("line missing %q: %s", key, line)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
