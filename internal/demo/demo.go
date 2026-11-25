// Package demo orchestrates the self-generated FieldPost acceptance run.
package demo

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"

	"fieldpost/fixture"
	"fieldpost/internal/core"
	"fieldpost/pipeline"
)

// Options controls a demo invocation.
type Options struct {
	Seed      int64
	Profile   string
	MemoryCap int64
	OutputDir string
}

// Report is the deterministic acceptance result.
type Report struct {
	Seed                 int64           `json:"seed"`
	Profile              string          `json:"profile"`
	MemoryCap            int64           `json:"memory_cap_bytes"`
	OutputDir            string          `json:"output_dir"`
	Healthy              []HealthyResult `json:"-"`
	HealthyRoundtrip     float64         `json:"healthy_roundtrip"`
	EssenceOK            int             `json:"essence_ok"`
	EssenceTotal         int             `json:"essence_total"`
	KeyHits              int             `json:"key_hits"`
	KeyTotal             int             `json:"key_total"`
	KeyframeHit          float64         `json:"keyframe_hit"`
	BackpressureEvents   int64           `json:"backpressure_events"`
	PeakMemBytes         int64           `json:"peak_mem_bytes"`
	BoundaryRejected     int             `json:"boundary_rejected"`
	Unrecoverable        int             `json:"unrecoverable_isolated"`
	UnrecoverableOutputs int             `json:"unrecoverable_outputs"`
	Deterministic        bool            `json:"deterministic"`
	BandwidthProfiles    int             `json:"bandwidth_profiles"`
}

type collector struct {
	mu      sync.Mutex
	results []HealthyResult
}

// Run executes all requested demo profiles and verifies acceptance invariants.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Profile == "" {
		opts.Profile = "all"
	}
	if opts.Profile != "all" {
		return nil, core.UsageError("unsupported profile %q (want all)", opts.Profile)
	}
	if opts.MemoryCap <= 0 {
		return nil, core.UsageError("memory-cap must be positive")
	}
	if opts.MemoryCap < 1<<20 {
		return nil, core.UsageError("memory-cap must be at least 1048576 bytes for demo buffers")
	}
	outDir := opts.OutputDir
	if outDir == "" {
		var err error
		outDir, err = os.MkdirTemp("", "fieldpost-delivery-*")
		if err != nil {
			return nil, core.Wrap(core.StagePipeline, core.ExitUnrecoverable, true, err, "create delivery dir")
		}
	} else if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, core.Wrap(core.StagePipeline, core.ExitUnrecoverable, true, err, "create output directory")
	}

	if err := validateManifests(); err != nil {
		return nil, err
	}
	sig1 := canonicalSignature(opts.Seed)
	sig2 := canonicalSignature(opts.Seed)
	if sig1 != sig2 {
		return nil, core.InvariantError(core.StageInvariant, "fixture", 0, "non-deterministic canonical signature")
	}

	c := &collector{}
	p := pipeline.New(ctx, opts.MemoryCap, 2, 32, func(ctx context.Context, item pipeline.Item) error {
		camera := item.Meta["camera"]
		idx := int(item.Meta["index"][0]-'0')*10 + int(item.Meta["index"][1]-'0')
		res, err := processHealthy(ctx, opts.Seed, camera, idx, outDir)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.results = append(c.results, *res)
		c.mu.Unlock()
		return nil
	})

	var submitErr error
	for _, camera := range fixture.CameraNames {
		for i := 0; i < fixture.SegmentsPerCamera; i++ {
			item := pipeline.Item{
				ID:     fmt.Sprintf("%s-%02d", camera, i),
				Camera: camera,
				Weight: 96 << 10,
				Meta: map[string]string{
					"camera": camera,
					"index":  fmt.Sprintf("%02d", i),
				},
			}
			if err := p.Submit(ctx, item); err != nil {
				submitErr = err
				break
			}
		}
		if submitErr != nil {
			break
		}
	}
	closeErr := p.Close()
	if submitErr != nil {
		return nil, submitErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	mediaSnap := p.Snapshot()

	sort.Slice(c.results, func(i, j int) bool { return c.results[i].ID < c.results[j].ID })
	r := &Report{
		Seed: opts.Seed, Profile: opts.Profile, MemoryCap: opts.MemoryCap, OutputDir: outDir,
		Healthy: c.results, EssenceTotal: len(c.results), Deterministic: sig1 == sig2,
		BandwidthProfiles: len(fixture.BandwidthProfile(opts.Seed)),
	}
	for _, h := range c.results {
		r.EssenceOK++
		r.KeyHits += h.KeyHits
		r.KeyTotal += h.KeyTotal
	}
	r.HealthyRoundtrip = float64(r.EssenceOK) / float64(maxInt(12, r.EssenceTotal))
	if r.EssenceTotal != 12 || r.EssenceOK != 12 {
		return nil, core.InvariantError(core.StageInvariant, "essence", int64(r.EssenceOK),
			"healthy roundtrips=%d/%d want 12/12", r.EssenceOK, r.EssenceTotal)
	}
	r.KeyframeHit = float64(r.KeyHits) / float64(maxInt(1, r.KeyTotal))
	if r.KeyHits != r.KeyTotal || r.KeyTotal == 0 {
		return nil, core.InvariantError(core.StageIndex, "keyframe", int64(r.KeyHits),
			"keyframe hits=%d total=%d", r.KeyHits, r.KeyTotal)
	}

	rejected, err := runBoundaries()
	if err != nil {
		return nil, err
	}
	r.BoundaryRejected = rejected
	if r.BoundaryRejected < 4 {
		return nil, core.InvariantError(core.StageManifest, "boundary", int64(rejected),
			"boundary rejected=%d want >=4", rejected)
	}
	if err := runUnrecoverableBurst(opts.Seed, outDir); err != nil {
		return nil, err
	}
	r.Unrecoverable = 1
	r.UnrecoverableOutputs = 0

	pressureSnap, err := RunPressure(ctx, opts.MemoryCap)
	if err != nil {
		return nil, err
	}
	r.BackpressureEvents = pressureSnap.BackpressureEvents + mediaSnap.BackpressureEvents
	r.PeakMemBytes = pressureSnap.PeakInFlight
	if mediaSnap.PeakInFlight > r.PeakMemBytes {
		r.PeakMemBytes = mediaSnap.PeakInFlight
	}
	if r.BackpressureEvents == 0 {
		return nil, core.InvariantError(core.StagePipeline, "backpressure", 0, "no backpressure events")
	}
	if r.PeakMemBytes > opts.MemoryCap {
		return nil, core.InvariantError(core.StagePipeline, "memory-gate", r.PeakMemBytes,
			"peak_mem_bytes=%d cap=%d", r.PeakMemBytes, opts.MemoryCap)
	}
	return r, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
