package capture_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mediad/internal/capture"
	"mediad/internal/fault"
	"mediad/internal/sample"
)

// TestGenerativeCaptureFaults generates damaged streams for many seeds and
// verifies prefix preservation, damage localization and index rejection/rebuild.
func TestGenerativeCaptureFaults(t *testing.T) {
	for seed := int64(1); seed <= 24; seed++ {
		t.Run("seed", func(t *testing.T) {
			dir := t.TempDir()
			m, err := sample.Generate(sample.Options{
				Seed: seed, Seconds: 1, Rate: 48000, Channels: 2,
				Dir: dir, Faults: true,
			})
			if err != nil {
				t.Fatal(err)
			}

			f, err := os.Open(m.CapturePath)
			if err != nil {
				t.Fatal(err)
			}
			st, _ := f.Stat()
			sc, err := capture.NewScanner(f, st.Size())
			if err != nil {
				t.Fatal(err)
			}
			records := 0
			var located int64 = -1
			var sawTruncated, sawCRCDamage bool
			for {
				rec, err := sc.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("seed %d: %v", seed, err)
				}
				records++
				if rec.DamageOffset >= 0 {
					located = rec.DamageOffset
					sawCRCDamage = true
				}
			}
			f.Close()
			for _, d := range sc.Report.Damages {
				if d.Kind == "truncated_frame" {
					sawTruncated = true
				}
			}
			if records != m.Records {
				t.Fatalf("seed %d records=%d want %d", seed, records, m.Records)
			}
			if !sawCRCDamage || located != m.BadCRCOffset {
				t.Fatalf("seed %d CRC locate=%d want %d saw=%v", seed, located, m.BadCRCOffset, sawCRCDamage)
			}
			if !sawTruncated {
				t.Fatalf("seed %d missing truncation damage", seed)
			}

			idx, err := capture.LoadIndex(m.IndexPath, st.Size())
			if err != nil {
				t.Fatalf("valid index: %v", err)
			}
			if idx.RecordCount != m.Records {
				t.Fatalf("index records=%d want %d", idx.RecordCount, m.Records)
			}
			if _, err := capture.LoadIndex(m.BadIndexPath, st.Size()); !errors.Is(err, capture.ErrIndexOutOfRange) {
				t.Fatalf("seed %d bad index error=%v", seed, err)
			}

			rebuiltPath := filepath.Join(dir, "rebuilt.json")
			b, err := os.ReadFile(m.BadIndexPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rebuiltPath, b, 0o644); err != nil {
				t.Fatal(err)
			}
			idx2, rebuilt, err := capture.LoadOrRebuild(rebuiltPath, m.CapturePath, 32)
			if err != nil {
				t.Fatalf("rebuild: %v", err)
			}
			if !rebuilt || idx2.RecordCount != m.Records {
				t.Fatalf("seed %d rebuilt=%v count=%d", seed, rebuilt, idx2.RecordCount)
			}
		})
	}
}

func TestRejectsOversizedDeclaredLengthBeforeAllocation(t *testing.T) {
	// 0xffffffff must be rejected by the shared length guard rather than
	// becoming an allocation or integer wrap.
	if _, err := fault.CheckFrameLength(0xffffffff, 9, 4, capture.MaxRecord); err == nil {
		t.Fatal("expected oversized frame rejection")
	}
	if err := fault.CheckInputSize(1 << 31); err == nil {
		t.Fatal("expected input over 1 GiB rejection")
	}
}
