package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sync"

	"github.com/mediamend/mediamend/internal/audio"
	"github.com/mediamend/mediamend/internal/color"
	"github.com/mediamend/mediamend/internal/graph"
	"github.com/mediamend/mediamend/internal/index"
	"github.com/mediamend/mediamend/internal/model"
)

// runAudio normalizes audio tracks under the explicit project policy. A fatal
// audio fault marks that track incomplete and blocks its dependent videos.
func runAudio(cfg Config, sem chan struct{}, res *Result) {
	specs := specByID(cfg)
	var wg sync.WaitGroup
	for _, id := range res.sortedTrackIDs() {
		spec := specs[id]
		if spec.Kind != graph.KindAudio {
			continue
		}
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			frames := res.frameStore[id]
			in := audio.Spec{
				TrackID: id, Layout: spec.Layout,
				SampleRate: uint32(spec.Rate), FrameSize: uint32(spec.FrameStep),
				FrameCount: uint64(len(frames)),
			}
			out, faults := audio.Normalize(in, cfg.Project.AudioPolicy, res.fileByTrack[id])
			tr := res.ensureTrack(id, spec.Kind)
			res.mu.Lock()
			tr.Audio = &out
			tr.Faults = append(tr.Faults, faults...)
			res.addFaults(faults)
			if model.HasFatal(faults) {
				tr.Complete = false
			}
			res.mu.Unlock()
		}()
	}
	wg.Wait()
	propagateBlocked(res)
}

// runColor reconciles color metadata for video tracks against project decls.
func runColor(cfg Config, sem chan struct{}, res *Result) {
	specs := specByID(cfg)
	var wg sync.WaitGroup
	for _, id := range res.sortedTrackIDs() {
		spec := specs[id]
		if spec.Kind != graph.KindVideo {
			continue
		}
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			var proj *color.Descriptor
			if d, ok := cfg.Project.Colors[id]; ok && d != nil {
				pd := color.Descriptor{
					Primaries: d.Primaries, Transfer: d.Transfer,
					Matrix: d.Matrix, FullRange: d.FullRange, Source: id,
				}
				proj = &pd
			}
			frames := res.frameStore[id]
			desc, faults := color.Reconcile(id, res.fileByTrack[id], frames, proj)
			tr := res.ensureTrack(id, spec.Kind)
			res.mu.Lock()
			tr.ColorSummary = color.TrackSummary(desc)
			tr.Faults = append(tr.Faults, faults...)
			res.addFaults(faults)
			if model.HasFatal(faults) {
				tr.Complete = false
			}
			res.mu.Unlock()
		}()
	}
	wg.Wait()
	propagateBlocked(res)
}

// propagateBlocked marks dependents of any failed track incomplete using the
// graph's transitive blocking relation.
func propagateBlocked(res *Result) {
	failed := res.failedTracks()
	if len(failed) == 0 {
		return
	}
	blocked := graphBlocked(res, failed)
	for _, id := range blocked {
		tr := res.Tracks[id]
		if tr == nil {
			continue
		}
		tr.Complete = false
		tr.BlockedBy = append(append([]string{}, failed...), blocked...)
	}
}

// runIndex builds and verifies a per-track MP4, then runs random seeks.
func runIndex(cfg Config, sem chan struct{}, res *Result) error {
	outDir := filepath.Join(cfg.Root, "output")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	keyEvery := cfg.KeyEvery
	if keyEvery == 0 {
		keyEvery = cfg.Project.KeyEvery
	}
	var wg sync.WaitGroup
	for _, id := range res.sortedTrackIDs() {
		tr := res.Tracks[id]
		if !tr.Complete || tr.Repaired == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			idx, faults := index.Build(tr.Repaired, outDir, keyEvery)
			res.mu.Lock()
			tr.Index = idx
			tr.IndexFile = idx.File
			tr.Faults = append(tr.Faults, faults...)
			res.addFaults(faults)
			buildFatal := model.HasFatal(faults)
			if buildFatal {
				tr.Complete = false
			}
			res.mu.Unlock()
			if buildFatal {
				return
			}

			rng := rand.New(rand.NewSource(cfg.SeekSeed))
			checked, sf := index.RandomSeeks(idx, cfg.RandomSeeks, rng)
			res.mu.Lock()
			res.SeekChecked += checked
			res.addFaults(sf)
			if model.HasFatal(sf) {
				tr.Complete = false
			}
			res.mu.Unlock()
		}()
	}
	wg.Wait()
	return nil
}

// runManifest writes the JSON manifest and computes the stable output hash
// over every generated index file in sorted track order.
func runManifest(cfg Config, res *Result) error {
	finalizeTrackStatus(res)
	h := sha256.New()
	for _, id := range res.sortedTrackIDs() {
		tr := res.Tracks[id]
		if tr.IndexFile == "" {
			continue
		}
		b, err := os.ReadFile(tr.IndexFile)
		if err != nil {
			return err
		}
		h.Write([]byte(id))
		h.Write(b)
	}
	res.OutputSHA256 = hex.EncodeToString(h.Sum(nil))

	manifestPath := filepath.Join(cfg.Root, "manifest.json")
	data, err := json.MarshalIndent(res.snapshot(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, data, 0o600)
}

// snapshot returns a map view of the manifest including all required JSON keys.
func (r *Result) snapshot() map[string]any {
	return map[string]any{
		"frames_total":          r.FramesTotal,
		"corrupt_located":       r.CorruptLocated,
		"location_precision":    r.LocationPrecision,
		"resume_ms":             r.ResumeMS,
		"resume_skipped_stages": r.SkippedStages,
		"seek_checked":          r.SeekChecked,
		"output_sha256":         r.OutputSHA256,
		"input_digest":          r.InputDigest,
		"complete_tracks":       r.CompleteTracks,
		"incomplete_tracks":     r.IncompleteTracks,
		"fault_count":           r.FaultCount,
		"fatal_count":           r.FatalCount,
		"tracks":                r.Tracks,
		"faults":                r.sortedFaults(),
	}
}

func finalizeTrackStatus(res *Result) {
	res.CompleteTracks, res.IncompleteTracks = 0, 0
	for _, id := range res.sortedTrackIDs() {
		tr := res.Tracks[id]
		if tr.Complete {
			res.CompleteTracks++
		} else {
			res.IncompleteTracks++
		}
	}
}
