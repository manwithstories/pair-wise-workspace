package pipeline

import (
	"path/filepath"
	"sort"
	"sync"

	"github.com/mediamend/mediamend/internal/fixture"
	"github.com/mediamend/mediamend/internal/graph"
	"github.com/mediamend/mediamend/internal/ingest"
	"github.com/mediamend/mediamend/internal/model"
	"github.com/mediamend/mediamend/internal/timeline"
)

// specByID indexes project track specs by id.
func specByID(cfg Config) map[string]fixture.TrackSpec {
	m := map[string]fixture.TrackSpec{}
	for _, s := range cfg.Project.Tracks {
		m[s.ID] = s
	}
	return m
}

// runIngest streams every input file with bounded concurrency and keeps the
// parsed frames for downstream stages.
func runIngest(cfg Config, sem chan struct{}, res *Result) error {
	specs := specByID(cfg)
	res.FramesTotal = 0 // fresh ingest owns the authoritative frame total
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, in := range cfg.Inputs {
		in := in
		spec := specs[in.TrackID]
		res.ensureTrack(in.TrackID, spec.Kind)
		res.fileByTrack[in.TrackID] = in.File
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r, err := ingest.Open(in.File, in.TrackID)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			defer r.Close()
			frames, _, faults, err := ingest.ReadAll(r)
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			res.addFaults(faults)
			res.frameStore[in.TrackID] = frames
			res.FramesTotal += len(frames)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return firstErr
}

// loadFramesForResume re-reads inputs when resuming after a prior ingest stage
// so later stages have their frame data without re-running ingest logic.
func loadFramesForResume(cfg Config, res *Result) error {
	specs := specByID(cfg)
	for _, in := range cfg.Inputs {
		r, err := ingest.Open(in.File, in.TrackID)
		if err != nil {
			return err
		}
		frames, _, _, err := ingest.ReadAll(r)
		r.Close()
		if err != nil {
			return err
		}
		spec := specs[in.TrackID]
		res.ensureTrack(in.TrackID, spec.Kind)
		res.fileByTrack[in.TrackID] = in.File
		res.frameStore[in.TrackID] = frames
		// On resume the authoritative frame total was backfilled from the
		// checkpoint before this loader ran; do not add to it again.
	}
	return nil
}

// runTimeline repairs each track against its project clock.
func runTimeline(cfg Config, sem chan struct{}, res *Result) {
	specs := specByID(cfg)
	var wg sync.WaitGroup
	for _, id := range res.sortedTrackIDs() {
		id := id
		frames := res.frameStore[id]
		spec := specs[id]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			clock := model.Clock{
				TrackID: id, Rate: spec.Rate, AnchorSeq: 0,
				AnchorDeviceTS: 0, AnchorMasterTS: 0, FrameStep: spec.FrameStep,
			}
			rep := timeline.Repair(frames, clock, res.fileByTrack[id])
			tr := res.ensureTrack(id, spec.Kind)
			res.mu.Lock()
			tr.Repaired = rep
			tr.FrameCount = len(frames)
			res.addFaults(rep.Faults)
			res.mu.Unlock()
		}()
	}
	wg.Wait()
}

// runGraph builds the dependency graph and stops on a cycle.
func runGraph(cfg Config, res *Result) error {
	specs := specByID(cfg)
	g := graph.New()
	for _, s := range cfg.Project.Tracks {
		if err := g.AddTrack(s.ID, s.Kind); err != nil {
			return err
		}
	}
	// Video aligns to its companion audio; index stage depends on repair,
	// expressed by wiring each video track to a matching audio track when both
	// kinds exist. Deterministic pairing by sorted ids keeps it stable.
	var audios, videos []string
	for _, s := range cfg.Project.Tracks {
		switch s.Kind {
		case graph.KindAudio:
			audios = append(audios, s.ID)
		case graph.KindVideo:
			videos = append(videos, s.ID)
		}
	}
	sort.Strings(audios)
	sort.Strings(videos)
	for i, v := range videos {
		if len(audios) > 0 {
			_ = g.AddDependency(v, audios[i%len(audios)])
		}
	}
	layers, cycle, faults, err := g.Order(filepath.Join(cfg.Root, "project.json"))
	if err != nil {
		res.addFaults(faults)
		// A cycle blocks every track; record that and surface exit code 3.
		for id := range res.Tracks {
			tr := res.ensureTrack(id, specs[id].Kind)
			tr.Complete = false
			tr.BlockedBy = cycle
		}
		return err
	}
	res.layers = layers
	return nil
}

// failedTracks returns the ids whose own faults contain a fatal entry.
func (r *Result) failedTracks() []string {
	var out []string
	for _, id := range r.sortedTrackIDs() {
		tr := r.Tracks[id]
		for _, f := range tr.Faults {
			if f.Severity == model.SeverityFatal {
				out = append(out, id)
				break
			}
		}
	}
	return out
}
