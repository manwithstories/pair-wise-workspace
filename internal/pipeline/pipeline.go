// Package pipeline wires the adapters, mapping engine, kernel, checkpoint
// journal and verifier into the single archival workflow the CLI drives. It is
// the only place that knows the end-to-end order, and it keeps that order
// explicit so each stage's failure is attributable.
package pipeline

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediabridge/internal/adapter"
	mp4adapter "mediabridge/internal/adapter/mp4"
	rtpadapter "mediabridge/internal/adapter/rtp"
	"mediabridge/internal/clock"
	"mediabridge/internal/core"
	"mediabridge/internal/fault"
	"mediabridge/internal/journal"
	"mediabridge/internal/manifest"
	"mediabridge/internal/maptrack"
	"mediabridge/internal/media"
	"mediabridge/internal/verify"
)

// Options configures a pipeline run.
type Options struct {
	// OutDir is the output directory (created if absent).
	OutDir string
	// CheckpointDir is the checkpoint directory; defaults to OutDir/checkpoints.
	CheckpointDir string
	// InterruptAfter aborts after N committed packets, simulating a crash.
	// Zero means run to completion.
	InterruptAfter int64
	// KeepPayload materialises encoded bytes during demux. The kernel can stream
	// from offsets either way; keeping payload simplifies fixture-backed runs.
	KeepPayload bool
}

// Metrics is the flattened acceptance view the CLI prints.
type Metrics struct {
	SyncErrorMS              float64
	PacketRecoveryRate       float64
	P95LatencyMS             float64
	CopiedPacketHashMatchPct float64
	ResumedDuplicatePackets  int64
	ProvenanceCoveragePct    float64
}

// AssetResult is the outcome for one asset.
type AssetResult struct {
	AssetID       string
	Status        string // "copied" | "resumed" | "rejected"
	Error         *fault.Error
	Metrics       Metrics
	Failed        []string
	LatencyMS     float64
	Provenance    *core.Provenance
	OutPath       string
	ResumedTracks int
	Quarantined   int
}

// RunError aggregates run-level outcome.
type RunError struct {
	Fatal       *fault.Error
	Rejected    []*fault.Error
	Interrupted bool
}

// Error implements error.
func (e *RunError) Error() string {
	if e.Fatal != nil {
		return e.Fatal.Error()
	}
	if len(e.Rejected) > 0 {
		return e.Rejected[0].Error()
	}
	return "pipeline: ok"
}

// Registry builds the default adapter registry.
func Registry() *adapter.Registry {
	r := adapter.NewRegistry()
	r.MustRegister(&mp4adapter.Reader{})
	return r
}

// Run processes every asset in the manifest. Rejected assets do not abort the
// run; a fatal run-level error (manifest or checkpoint infrastructure) does.
func Run(manifestPath string, opts Options) ([]AssetResult, *RunError) {
	loaded, err := manifest.Load(manifestPath)
	if err != nil {
		return nil, &RunError{Fatal: asFault(err, fault.CodePath, "", manifestPath, 0)}
	}
	if opts.CheckpointDir == "" {
		opts.CheckpointDir = filepath.Join(opts.OutDir, "checkpoints")
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, &RunError{Fatal: fault.New(fault.CodeCheckpoint,
			fault.Location{SourcePath: opts.OutDir}, "create output dir failed").
			Wrap(err).WithOp("pipeline.Run")}
	}
	j, err := journal.Open(opts.CheckpointDir)
	if err != nil {
		return nil, &RunError{Fatal: fault.New(fault.CodeCheckpoint,
			fault.Location{SourcePath: opts.CheckpointDir}, "open checkpoint dir failed").
			Wrap(err).WithOp("pipeline.Run")}
	}
	reg := Registry()
	var results []AssetResult
	runErr := &RunError{}
	interruptRemaining := opts.InterruptAfter
	for _, ra := range loaded.Assets() {
		res, consumed := runAsset(reg, j, loaded.Root(), ra, opts, interruptRemaining)
		results = append(results, res)
		if res.Status == "interrupted" {
			// Simulated crash: stop the whole run so the next invocation
			// resumes this and every later asset from its checkpoint.
			runErr.Interrupted = true
			return results, runErr
		}
		if interruptRemaining > 0 {
			interruptRemaining -= consumed
			if interruptRemaining <= 0 {
				runErr.Interrupted = true
				return results, runErr
			}
		}
		if res.Status == "rejected" && res.Error != nil {
			runErr.Rejected = append(runErr.Rejected, res.Error)
		}
	}
	return results, runErr
}

// runAsset processes one asset and returns the number of packets it committed
// (used by the interrupt simulator).
func runAsset(reg *adapter.Registry, j *journal.Journal, root string, ra manifest.ResolvedAsset, opts Options, interruptAfter int64) (AssetResult, int64) {
	start := time.Now()
	res := AssetResult{AssetID: ra.ID, Status: "copied"}

	src := adapter.Source{
		AssetID: ra.ID, Path: ra.PrimaryPath, Format: ra.Primary.Format,
		Version: ra.Primary.Version, RootDir: root,
	}
	ad, err := reg.Resolve(src.Format, src.Version, src)
	if err != nil {
		res.Status = "rejected"
		res.Error = asFault(err, fault.CodeUnknownVersion, ra.ID, ra.Primary.Path, 0)
		return res, 0
	}

	parseOpts := adapter.Options{
		KeepPayload:        true,
		MaxPacketsPerTrack: clock.MaxPacketsPerTrack,
		MaxAssetDurationNS: clock.MaxAssetDurationNanos,
	}
	asset, err := ad.Parse(src, parseOpts)
	if err != nil {
		res.Status = "rejected"
		res.Error = asFault(err, fault.CodeCorruptContainer, ra.ID, ra.Primary.Path, 0)
		return res, 0
	}

	auxTracks, auxTotals, err := mergeAux(asset, ra, parseOpts)
	if err != nil {
		res.Status = "rejected"
		res.Error = asFault(err, fault.CodeCorruptContainer, ra.ID, ra.Primary.Path, 0)
		return res, 0
	}

	rules := maptrack.Rule{
		VideoTrackID: ra.Mapping.VideoTrackID,
		AudioTrackID: ra.Mapping.AudioTrackID,
		DropUnmapped: ra.Mapping.DropUnmapped,
	}
	for _, a := range ra.Mapping.Auxiliary {
		rules.Auxiliary = append(rules.Auxiliary, maptrack.AuxSlot{
			Name: a.Name, Source: a.Source, TrackID: a.MPTrackID,
		})
	}
	plan, err := maptrack.Apply(rules, asset, auxTracks)
	if err != nil {
		res.Status = "rejected"
		res.Error = asFault(err, fault.CodeMapping, ra.ID, ra.Primary.Path, 0)
		return res, 0
	}

	prov := core.NewProvenance(ra.ID, asset.SourceSHA, ad.Format(), ad.Version())
	prov.Warnings = append(prov.Warnings, asset.Warnings...)
	for _, a := range plan.Assignments {
		if t, ok := asset.TrackByIndex(a.TrackIndex); ok {
			prov.AddTrack(a.Slot, t)
		}
	}

	outPath := filepath.Join(opts.OutDir, sanitizeAsset(ra.ID)+".mbx")
	res.OutPath = outPath
	// Provide a streaming source so the kernel can copy payloads straight from
	// the primary container when they were not materialised in memory.
	var srcReader core.SampleSource
	if f, err := os.Open(ra.PrimaryPath); err == nil {
		defer f.Close()
		srcReader = core.NewFileSampleSource(f)
	}
	outcome, consumed, err := runResumable(j, asset, plan, prov, ra, ad, opts, outPath, interruptAfter, srcReader)
	res.ResumedTracks = outcome.Resumed
	res.Quarantined = outcome.Quarantined
	if err != nil {
		if IsInterrupted(err) {
			// Simulated crash: committed work is durable, nothing is rejected.
			res.Status = "interrupted"
			res.Provenance = prov
			return res, consumed
		}
		if fault.CodeOf(err) == fault.CodeCheckpoint {
			res.Status = "rejected"
			res.Error = asFault(err, fault.CodeCheckpoint, ra.ID, outPath, 0)
			return res, consumed
		}
		res.Status = "rejected"
		res.Error = asFault(err, fault.CodeVerify, ra.ID, outPath, 0)
		return res, consumed
	}

	res.LatencyMS = float64(time.Since(start).Microseconds()) / 1000.0
	vr, err := verifyOutput(outPath, prov, auxTotals, 0, time.Since(start))
	if err != nil {
		res.Status = "rejected"
		res.Error = asFault(err, fault.CodeVerify, ra.ID, outPath, 0)
		return res, consumed
	}
	res.Metrics = Metrics{
		SyncErrorMS:              vr.Metrics.SyncErrorMS,
		PacketRecoveryRate:       vr.Metrics.PacketRecoveryRate,
		P95LatencyMS:             vr.Metrics.P95LatencyMS,
		CopiedPacketHashMatchPct: vr.Metrics.CopiedPacketHashMatchPct,
		ResumedDuplicatePackets:  vr.Metrics.ResumedDuplicatePackets,
		ProvenanceCoveragePct:    vr.Metrics.ProvenanceCoveragePct,
	}
	res.Failed = vr.Failed
	if len(vr.Failed) > 0 {
		res.Status = "rejected"
		res.Error = fault.New(fault.CodeVerify, fault.Location{AssetID: ra.ID, SourcePath: outPath},
			"acceptance metrics failed: %v", vr.Failed).WithOp("pipeline.Verify")
		return res, consumed
	}
	if outcome.Resumed > 0 || outcome.Quarantined > 0 || outcome.PriorCommitted > 0 {
		res.Status = "resumed"
	}
	res.Provenance = prov
	res.OutPath = outPath
	// Persist provenance next to the output for audit.
	if data, err := prov.JSON(); err == nil {
		_ = os.WriteFile(outPath+".provenance.json", data, 0o644)
	}
	return res, consumed
}

// auxTotals accumulates RTP recovery accounting across a whole asset.
type auxTotals struct {
	Delivered  int64
	Lost       int64
	Duplicates int64
}

func mergeAux(asset *media.Asset, ra manifest.ResolvedAsset, opts adapter.Options) ([]maptrack.AuxTrack, auxTotals, error) {
	var tracks []maptrack.AuxTrack
	var totals auxTotals
	paths := make([]string, 0, len(ra.AuxiliaryPaths))
	for p := range ra.AuxiliaryPaths {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		abs := ra.AuxiliaryPaths[p]
		aux, err := readAux(abs, opts)
		if err != nil {
			return nil, totals, err
		}
		idx := len(asset.Tracks)
		aux.Track.Index = idx
		asset.Tracks = append(asset.Tracks, aux.Track)
		for i := range aux.Packets {
			aux.Packets[i].TrackIndex = idx
			aux.Packets[i].SourceTrackID = aux.Track.ID
			aux.Packets[i].PacketIndex = int64(i)
		}
		asset.Packets = append(asset.Packets, aux.Packets...)
		tracks = append(tracks, maptrack.AuxTrack{
			Source: p, Index: idx, ID: aux.Track.ID, Kind: media.KindAux,
		})
		totals.Delivered += aux.Stats.Delivered
		totals.Lost += aux.Stats.Lost
		totals.Duplicates += aux.Stats.Duplicates
	}
	if len(tracks) > 0 {
		asset.SortPackets()
	}
	return tracks, totals, nil
}

func readAux(path string, opts adapter.Options) (*rtpadapter.Result, error) {
	rd, err := rtpadapter.Open(path)
	if err != nil {
		return nil, fault.New(fault.CodeCorruptContainer,
			fault.Location{SourcePath: path, Adapter: "rtp"}, "open rtp dump failed").
			Wrap(err).WithOp("pipeline.ReadAux")
	}
	res, err := rtpadapter.Demux(rd, rtpadapter.Options{
		SampleRate:    90000,
		KeepPayload:   opts.KeepPayload,
		ReorderWindow: 512,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// verifyOutput runs the independent verifier and maps its metrics into the
// pipeline's result type.
func verifyOutput(outPath string, prov *core.Provenance, aux auxTotals, resumedDupes int64, latency time.Duration) (*verify.Result, error) {
	return verify.Verify(verify.Input{
		OutputPath:        outPath,
		Provenance:        prov,
		SourceDelivered:   aux.Delivered,
		SourceLost:        aux.Lost,
		SourceDuplicates:  aux.Duplicates,
		Latency:           latency,
		ResumedDuplicates: resumedDupes,
	})
}

func sanitizeAsset(id string) string {
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "asset"
	}
	return string(out)
}

func asFault(err error, code fault.Code, assetID, path string, track uint32) *fault.Error {
	if e, ok := fault.As(err); ok {
		return e
	}
	loc := fault.Location{AssetID: assetID, SourcePath: path}
	if track != 0 {
		loc = loc.WithTrack(track)
	}
	return fault.New(code, loc, "%v", err)
}
