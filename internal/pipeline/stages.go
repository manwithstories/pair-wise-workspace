package pipeline

import (
	"fmt"
	"path/filepath"

	"mediarun/internal/clock"
	"mediarun/internal/ingest"
	"mediarun/internal/model"
	"mediarun/internal/testkit"
)

type transportResult struct {
	eventsByTrack map[uint32][]model.Event
	gapsByTrack   map[uint32][]model.Gap
	gapList       []model.Gap
	gaps          int
	packets       int
}

func runTransport(opts Options, media *testkit.Media) (*transportResult, error) {
	out := &transportResult{
		eventsByTrack: map[uint32][]model.Event{},
		gapsByTrack:   map[uint32][]model.Gap{},
	}
	// Three capture sources, each with its own timescale start offset so
	// cross-track ordering is exercised.
	specs := []struct {
		track   uint32
		start   clock.Tick
		packets int
	}{
		{media.VideoTrackID, 0, 60},
		{media.VideoTrackID + 1, 1500, 45},
		{media.VideoTrackID + 2, 3000, 30},
	}
	bands := opts.Bandwidth
	for i, spec := range specs {
		stream, err := ingest.New(ingest.Config{
			TrackID:    spec.track,
			Timescale:  90000,
			Window:     opts.Window,
			StartTick:  spec.start,
			MaxBacklog: 16,
			SpillDir:   filepath.Join(opts.Out, "spill", fmt.Sprintf("t%d", spec.track)),
		})
		if err != nil {
			return nil, err
		}
		scenario := opts.Scenario
		if i > 0 {
			// Only the first track carries the full defect pattern.
			scenario = testkit.ScenarioClean
		}
		packets, err := testkit.GeneratePackets(opts.Seed+int64(i), scenario, spec.track, spec.packets, 0)
		if err != nil {
			stream.Close()
			return nil, err
		}
		for j, p := range packets {
			if len(bands) > 0 {
				if err := stream.SetBandwidth(bands[j%len(bands)]); err != nil {
					stream.Close()
					return nil, err
				}
			}
			wire, err := testkit.EncodeWire(p)
			if err != nil {
				stream.Close()
				return nil, err
			}
			rec, err := stream.Receive(wire)
			if err != nil {
				stream.Close()
				return nil, err
			}
			out.gaps += len(rec.Gaps)
			for _, g := range rec.Gaps {
				out.gapsByTrack[spec.track] = append(out.gapsByTrack[spec.track], g)
				out.gapList = append(out.gapList, g)
			}
			out.packets++
			evs, err := stream.Drain(0)
			if err != nil {
				stream.Close()
				return nil, err
			}
			out.eventsByTrack[spec.track] = append(out.eventsByTrack[spec.track], evs...)
		}
		rec, err := stream.Close()
		if err != nil {
			return nil, err
		}
		for _, g := range rec.Gaps {
			out.gapsByTrack[spec.track] = append(out.gapsByTrack[spec.track], g)
			out.gapList = append(out.gapList, g)
		}
		out.gaps += len(rec.Gaps)
		for {
			evs, err := stream.Drain(0)
			if err != nil {
				return nil, err
			}
			if len(evs) == 0 {
				break
			}
			out.eventsByTrack[spec.track] = append(out.eventsByTrack[spec.track], evs...)
		}
	}
	if out.gaps == 0 && opts.Scenario == testkit.ScenarioBurst {
		return nil, fmt.Errorf("%w: burst scenario produced no detectable gap", model.ErrGap)
	}
	if err := writeJSON(filepath.Join(opts.Out, "transport.json"), map[string]any{
		"scenario":  opts.Scenario,
		"packets":   out.packets,
		"gaps":      out.gaps,
		"bandwidth": opts.Bandwidth,
	}); err != nil {
		return nil, err
	}
	return out, nil
}
