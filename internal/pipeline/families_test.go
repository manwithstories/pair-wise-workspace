// The four fixture families exercised end to end: normal, damaged, reordered
// with lossy transport, and a polluted cache.
package pipeline

import (
	"path/filepath"
	"testing"

	"archive.local/archive/internal/cache"
	"archive.local/archive/internal/fixture"
	"archive.local/archive/internal/model"
)

func TestFixtureFamiliesCoverCrossModuleRecovery(t *testing.T) {
	t.Run("normal", func(t *testing.T) {
		work := buildWorkspace(t, false)
		res, err := run(t, work, false)
		if err != nil || res.Status != OK {
			t.Fatalf("normal run: %v status=%v", err, res.Status)
		}
		if res.CacheRejected != 0 {
			t.Fatalf("clean run rejected cache entries: %d", res.CacheRejected)
		}
	})
	t.Run("damaged", func(t *testing.T) {
		work := buildWorkspace(t, true)
		res, _ := run(t, work, false)
		if res.Status != Damaged || res.Defect == nil {
			t.Fatalf("damaged run: status=%v defect=%+v", res.Status, res.Defect)
		}
		if model.CodeOf(res.Defect) != model.CodeTruncatedSample {
			t.Fatalf("code = %q", model.CodeOf(res.Defect))
		}
	})
	t.Run("reordered-and-lossy", func(t *testing.T) {
		work := buildWorkspace(t, false)
		// Overwrite the trace with a heavier defect plan.
		if _, err := fixture.GenerateTrace(filepath.Join(work, "transport.trace"),
			fixture.TracePlan{Packets: 900, Payload: 64, DupEvery: 17, ReorderEvery: 23,
				Bursts:   []fixture.Burst{{At: 300, Count: 9}, {At: 700, Count: 14}},
				StartSeq: 65400}, 7); err != nil {
			t.Fatal(err)
		}
		res, err := run(t, work, false)
		if err != nil {
			t.Fatal(err)
		}
		if res.Transport.Duplicate == 0 || res.Transport.Reordered == 0 || res.Transport.BurstLoss == 0 {
			t.Fatalf("transport did not report defects: %+v", res.Transport)
		}
		if res.Status != OK {
			// A lossy transport is a defect in the trace, but the index over
			// the source material is still sound; report it as damaged only
			// when the source itself is damaged.
			t.Fatalf("lossy trace should not damage the source: %v", res.Defect)
		}
	})
	t.Run("cache-pollution", func(t *testing.T) {
		work := buildWorkspaceWith(t, false, true)
		res, err := run(t, work, false)
		if err != nil {
			t.Fatalf("seed run: %v", err)
		}
		if res.CacheRejected == 0 {
			t.Fatalf("cache pollution was not detected")
		}
		// The poisoned entry was quarantined and reprocessed. A second run
		// finds a clean store: no new quarantine should be recorded, proving
		// the rejected entry was not merely re-detected.
		res2, err := run(t, work, false)
		if err != nil {
			t.Fatalf("post-heal run: %v", err)
		}
		if res2.CacheRejected != 0 {
			t.Fatalf("reprocessing pass re-rejected a healed entry: %d", res2.CacheRejected)
		}
		store, err := cache.NewStore(filepath.Join(work, "cache"))
		if err != nil {
			t.Fatal(err)
		}
		bad, err := store.QuarantinedEntries()
		if err != nil || len(bad) == 0 {
			t.Fatalf("quarantined artefact missing: %v err=%v", bad, err)
		}
	})
}
