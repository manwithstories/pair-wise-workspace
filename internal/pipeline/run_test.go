package pipeline

import (
	"mediaqc/internal/media"
	"testing"
)

func TestNormalDeterministicHashAndThresholds(t *testing.T) {
	cfg := Config{Profile: "normal", Codec: "opus", Seed: 41}
	a, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if a.Stats.OutputSHA256 == "" || a.Stats.OutputSHA256 != b.Stats.OutputSHA256 {
		t.Fatalf("hash not stable: %q vs %q", a.Stats.OutputSHA256, b.Stats.OutputSHA256)
	}
	if a.Stats.Duplicates != 2 {
		t.Fatalf("duplicates=%d want 2", a.Stats.Duplicates)
	}
	if a.Stats.RecoveryRate < 0.80 {
		t.Fatalf("recovery rate %.2f", a.Stats.RecoveryRate)
	}
	if a.Stats.AVSyncP95MS > 20 {
		t.Fatalf("av p95 %.2f >20", a.Stats.AVSyncP95MS)
	}
	if a.Stats.E2EP95MS > 180 {
		t.Fatalf("e2e p95 %.2f >180", a.Stats.E2EP95MS)
	}
}

func TestBurstRecoveryFailOncePreservesArtifactHash(t *testing.T) {
	clean, err := Run(Config{Profile: "burst", Codec: "aac", Seed: 41})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := Run(Config{Profile: "burst", Codec: "aac", Seed: 41, FailOnce: "audio.repair"})
	if err != nil {
		t.Fatalf("retry should recover: %v", err)
	}
	if retry.Stats.Recovered < 4 || retry.Stats.RecoverableLoss < 4 {
		t.Fatalf("recovery stats=%+v", retry.Stats)
	}
	if retry.Stats.RecoveryRate < 0.80 {
		t.Fatalf("rate=%.3f", retry.Stats.RecoveryRate)
	}
	if retry.Stats.AVSyncP95MS > 20 || retry.Stats.E2EP95MS > 180 {
		t.Fatalf("thresholds failed: av=%.2f e2e=%.2f", retry.Stats.AVSyncP95MS, retry.Stats.E2EP95MS)
	}
	if retry.DAG == nil || len(retry.DAG.Retried) != 1 || retry.DAG.Retried[0] != "audio.repair" {
		t.Fatalf("dag report=%+v", retry.DAG)
	}
	if retry.Stats.FirstBlocked != "audio.repair" {
		t.Fatalf("first_blocked=%q", retry.Stats.FirstBlocked)
	}
	if clean.Stats.OutputSHA256 != retry.Stats.OutputSHA256 {
		t.Fatalf("retry changed artifact hash: clean=%s retry=%s", clean.Stats.OutputSHA256, retry.Stats.OutputSHA256)
	}

	again, err := Run(Config{Profile: "burst", Codec: "aac", Seed: 41, FailOnce: "audio.repair"})
	if err != nil {
		t.Fatal(err)
	}
	if again.Stats.OutputSHA256 != retry.Stats.OutputSHA256 {
		t.Fatal("repeated fail-once run hash changed")
	}
}

func TestCorruptProfileClassifiesMalformed(t *testing.T) {
	out, err := Run(Config{Profile: "corrupt", Codec: "opus", Seed: 41})
	if err == nil {
		t.Fatal("expected classified corruption error")
	}
	if media.Classify(err) != media.KindMalformed {
		t.Fatalf("kind=%s err=%v", media.Classify(err), err)
	}
	if media.ExitCode(err) != 2 {
		t.Fatalf("exit code=%d", media.ExitCode(err))
	}
	if out == nil || out.Stats.Malformed == 0 {
		t.Fatalf("malformed count missing: %+v", out)
	}
	if out.Stats.Conflicts == 0 {
		t.Fatal("conflict sample should also be generated")
	}
	if out.Stats.OutputSHA256 == "" {
		t.Fatal("corrupt run should still seal diagnostic artifact")
	}
}

func TestUnsupportedCodecClassified(t *testing.T) {
	_, err := Run(Config{Profile: "normal", Codec: "mp3", Seed: 1})
	if media.Classify(err) != media.KindMalformed {
		t.Fatalf("err=%v kind=%s", err, media.Classify(err))
	}
}
