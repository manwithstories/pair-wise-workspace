package txn_test

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/fault"
	"mediad/internal/txn"
)

func TestGenerativeCommitAndCorruptWALRecovery(t *testing.T) {
	for seed := int64(1); seed <= 30; seed++ {
		t.Run("seed", func(t *testing.T) {
			dir := t.TempDir()
			txID := txn.StableTxID("gen", string(rune(seed)))
			m, err := txn.OpenManager(dir, txID, "")
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewSource(seed))
			stageCount := 1 + rng.Intn(5)
			var lastRel string
			var lastData []byte
			for i := 0; i < stageCount; i++ {
				data := make([]byte, 32+rng.Intn(4096))
				_, _ = rng.Read(data)
				rel := filepath.Join("blob", "stage.bin")
				if i == stageCount-1 {
					rel = "last.bin"
					lastRel = rel
					lastData = data
				}
				if _, err := m.CommitStage(stageName(i), rel, data); err != nil {
					t.Fatal(err)
				}
			}
			lastPath := m.Path(lastRel)
			if err := m.Close(); err != nil {
				t.Fatal(err)
			}

			// Reopen: checkpoint boundary must validate.
			m2, err := txn.OpenManager(dir, txID, "")
			if err != nil {
				t.Fatal(err)
			}
			if !m2.IsCommitted(stageName(stageCount - 1)) {
				t.Fatalf("seed %d last stage not committed", seed)
			}
			_ = m2.Close()

			// Corrupt one random WAL byte or append a torn record.
			walPath := filepath.Join(dir, txn.WALFileName)
			b, err := os.ReadFile(walPath)
			if err != nil {
				t.Fatal(err)
			}
			if seed%2 == 0 && len(b) > 32 {
				pos := 16 + rng.Intn(len(b)-16)
				b[pos] ^= 0xff
				if err := os.WriteFile(walPath, b, 0o644); err != nil {
					t.Fatal(err)
				}
			} else {
				f, err := os.OpenFile(walPath, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = f.WriteString(`{"lsn":999,"op":"begin",`)
				_ = f.Close()
			}

			m3, err := txn.OpenManager(dir, txID, "")
			if err != nil {
				t.Fatalf("seed %d recovery after corruption: %v", seed, err)
			}
			if !m3.CorruptTail {
				t.Fatalf("seed %d expected corrupt tail detection", seed)
			}
			got, err := os.ReadFile(lastPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(lastData) {
				t.Fatalf("seed %d committed output changed", seed)
			}
			if !m3.IsCommitted(stageName(stageCount - 1)) {
				t.Fatalf("seed %d checkpoint boundary lost", seed)
			}
			_ = m3.Close()
		})
	}
}

func TestInjectedInterruptRollbackAndResume(t *testing.T) {
	dir := t.TempDir()
	txID := "interrupt-tx"
	m, err := txn.OpenManager(dir, txID, "mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.CommitStage("dsp", "master.l16", []byte("master")); err != nil {
		t.Fatal(err)
	}
	_, err = m.CommitStage("mp4", "final.mp4", []byte("mp4"))
	if fault.ExitCode(err) != fault.ExitInterrupted {
		t.Fatalf("err=%v code=%d", err, fault.ExitCode(err))
	}
	_ = m.Close()

	m2, err := txn.OpenManager(dir, txID, "")
	if err != nil {
		t.Fatal(err)
	}
	if m2.Rollbacks != 1 {
		t.Fatalf("rollbacks=%d want 1", m2.Rollbacks)
	}
	if !m2.IsCommitted("dsp") {
		t.Fatal("dsp boundary missing")
	}
	if _, err := m2.CommitStage("mp4", "final.mp4", []byte("mp4")); err != nil {
		t.Fatal(err)
	}
	if !m2.IsCommitted("mp4") {
		t.Fatal("mp4 not committed after resume")
	}
	_ = m2.Close()

	m3, err := txn.OpenManager(dir, txID, "")
	if err != nil {
		t.Fatal(err)
	}
	if m3.Rollbacks != 0 {
		t.Fatalf("second recovery rollbacks=%d want 0", m3.Rollbacks)
	}
	_ = m3.Close()
}

func TestInjectedFailureDoesNotOverwriteCommittedFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "final.mp4")
	if err := os.WriteFile(out, []byte("committed"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := txn.OpenManager(dir, "tx", "mp4")
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.CommitStage("mp4", out, []byte("new-uncommitted"))
	if !errors.Is(err, fault.Interrupted("mp4", errors.New("x"))) && fault.ExitCode(err) != 75 {
		t.Fatalf("expected 75, got %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "committed" {
		t.Fatalf("committed output overwritten: %q", b)
	}
}

func stageName(i int) string {
	return "stage-" + string(rune('a'+i))
}
