package pipeline

import (
	"path/filepath"
	"testing"
)

func TestRunDemoShortIncrementalPipeline(t *testing.T) {
	work := filepath.Join(t.TempDir(), "demo")
	res, err := RunDemo(Options{Work: work, Seed: 17, Seconds: 2, TargetKbps: 1200})
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.Manifest.Stats.DecodeErrors != 0 {
		t.Fatalf("decode errors: %d", res.Manifest.Stats.DecodeErrors)
	}
	if !res.Cache.Hit || !res.Cache.SourceOK || !res.Cache.SHA256OK {
		t.Fatalf("cache verification failed: %+v", res.Cache)
	}
	if res.Manifest.Stats.TargetKbps != 1200 {
		t.Fatalf("target = %d", res.Manifest.Stats.TargetKbps)
	}
	// A short run is primarily an integration smoke test; rerun must resume.
	res2, err := RunDemo(Options{Work: work, Seed: 17, Seconds: 2, TargetKbps: 1200})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res2.Manifest.Stats.ActualKbps != res.Manifest.Stats.ActualKbps {
		t.Fatal("bitrate changed on rerun")
	}
}

func TestFaultSuiteAllExpectedRejections(t *testing.T) {
	work := filepath.Join(t.TempDir(), "faults")
	lines := RunFaultSuite(work, nil)
	if len(lines) < 7 {
		t.Fatalf("only %d fault lines", len(lines))
	}
	for _, l := range lines {
		if !l.OK {
			t.Fatalf("case %s not expected rejection: %s", l.Case, l.Detail)
		}
		switch l.Case {
		case "truncated_moov", "timestamp_rollback":
			if l.ExitCode != 2 {
				t.Fatalf("%s exit=%d", l.Case, l.ExitCode)
			}
		case "unrecoverable_loss", "digest_tampered", "source_digest_tampered":
			if l.ExitCode != 3 {
				t.Fatalf("%s exit=%d", l.Case, l.ExitCode)
			}
		case "checkpoint_corrupt", "io_error":
			if l.ExitCode != 4 {
				t.Fatalf("%s exit=%d", l.Case, l.ExitCode)
			}
		}
	}
}

func TestDemoAudioDeterminism(t *testing.T) {
	a := DemoAudio(30)
	b := DemoAudio(30)
	if DigestFloats(a) != DigestFloats(b) {
		t.Fatal("demo audio is not deterministic")
	}
	if len(a) != 30*demoSampleRate {
		t.Fatalf("audio samples = %d", len(a))
	}
}
