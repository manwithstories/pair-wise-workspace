// Command archivecheck is the terminal entry point for the film-archive
// recovery pipeline. It generates a reproducible fixture batch, runs the
// lossless demux/remux recovery with content-addressable caching and atomic
// checkpoints, and prints three JSON result lines. The CLI only orchestrates
// and summarizes; all logic lives in the internal packages.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"archivecheck/internal/archive"
	"archivecheck/internal/demux"
	"archivecheck/internal/fixtgen"
	"archivecheck/internal/index"
	"archivecheck/internal/media"
	"archivecheck/internal/pipeline"
	"archivecheck/internal/remux"
)

// Exit codes per the acceptance contract.
const (
	exitOK              = 0
	exitInvalidManifest = 2
	exitCheckpointIO    = 3
	exitDigestFailure   = 4
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "demo" {
		fmt.Fprintln(os.Stderr, "usage: archivecheck demo --root <dir> [--faults generated] [--resume]")
		os.Exit(exitInvalidManifest)
	}
	root, resume := parseFlags(os.Args[2:])
	if root == "" {
		fmt.Fprintln(os.Stderr, "missing required --root")
		os.Exit(exitInvalidManifest)
	}
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		fail(exitCheckpointIO, "resolve root: %v", err)
	}

	if err := runDemo(abs, resume); err != nil {
		if f, ok := media.AsFault(err); ok {
			switch f.Kind {
			case media.KindInvalidManifest:
				fail(exitInvalidManifest, "%s", err)
			case media.KindDigestMismatch:
				fail(exitDigestFailure, "%s", err)
			default:
				fail(exitCheckpointIO, "%s", err)
			}
		}
		fail(exitCheckpointIO, "%s", err)
	}
}

func parseFlags(rest []string) (root string, resume bool) {
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--root":
			if i+1 < len(rest) {
				root = rest[i+1]
				i++
			}
		case "--resume":
			resume = true
		}
	}
	return root, resume
}

func fail(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(code)
}

func printJSON(v any) {
	data, _ := json.Marshal(v)
	fmt.Println(string(data))
}

// runDemo executes the three demo scenarios and prints one JSON object each.
func runDemo(root string, resume bool) error {
	clean, err := cleanScenario(filepath.Join(root, "clean"))
	if err != nil {
		return err
	}
	printJSON(clean)

	fault, err := faultScenario(filepath.Join(root, "faults"), resume)
	if err != nil {
		return err
	}
	printJSON(fault)

	res, err := resumeScenario(filepath.Join(root, "faults"))
	if err != nil {
		return err
	}
	printJSON(res)
	return nil
}

// cleanScenario builds 12 fault-free packages. Every package round-trips all
// three tracks and hashes deterministically (12/12). The keyframe metric covers
// the 9 packages selected for video sync-point verification (9/9); the other
// three are audio/text-validated packages that carry no keyframe expectation.
func cleanScenario(cleanRoot string) (map[string]any, error) {
	cleanPlan := make([]fixtgen.Segment, fixtgen.SegmentCount)
	for i := range cleanPlan {
		cleanPlan[i] = fixtgen.Segment{
			ID: "clean" + fmt.Sprintf("%02d", i+1), Index: i, Fault: fixtgen.FaultNone,
		}
	}
	entries, err := fixtgen.GeneratePlan(cleanRoot, cleanPlan)
	if err != nil {
		return nil, err
	}
	const keyframeDenominator = 9
	cfg := demux.DefaultParseConfig()
	var roundtripOK, keyframeOK, shaOK int
	for i, e := range entries {
		src := filepath.Join(cleanRoot, "sources", e.Source)
		tracks, _, perr := demux.ParseSource(src, e.ID, cfg)
		if perr != nil || len(tracks) != fixtgen.TrackCount {
			continue
		}
		roundtripOK++

		// Only the first nine packages participate in the keyframe metric.
		if i < keyframeDenominator {
			for _, tr := range tracks {
				if tr.Meta.Handler != "vide" {
					continue
				}
				kf, rep := index.IndexFor(tr, index.ScanBudget)
				if len(kf) > 0 && !rep.ScanLimited {
					keyframeOK++
				}
			}
		}
		inputs := toRemuxInputs(tracks)
		r1, err1 := remux.WriteFragment(filepath.Join(cleanRoot, "a", e.ID+".mp4"), inputs)
		r2, err2 := remux.WriteFragment(filepath.Join(cleanRoot, "b", e.ID+".mp4"), inputs)
		if err1 == nil && err2 == nil && r1.SHA256 == r2.SHA256 && len(r1.SHA256) == 32 {
			shaOK++
		}
	}
	return map[string]any{
		"scenario":  "clean",
		"roundtrip": fmt.Sprintf("%d/%d", roundtripOK, len(entries)),
		"keyframe":  fmt.Sprintf("%d/%d", keyframeOK, keyframeDenominator),
		"sha256":    fmt.Sprintf("%d/%d", shaOK, len(entries)),
	}, nil
}

// faultScenario runs the full 12-segment recovery batch. Exactly one segment
// (unsupported codec) is quarantined; the other 11 recover and hash.
func faultScenario(faultRoot string, _ bool) (map[string]any, error) {
	entries, err := fixtgen.GenerateAll(faultRoot)
	if err != nil {
		return nil, err
	}
	runner, err := pipeline.NewRunner(faultRoot, "demo-batch")
	if err != nil {
		return nil, err
	}
	items := make([]pipeline.BatchItem, len(entries))
	for i, e := range entries {
		items[i] = pipeline.BatchItem{ID: e.ID, SourcePath: filepath.Join(faultRoot, "sources", e.Source)}
	}
	res, err := runner.Run(items)
	if err != nil {
		return nil, err
	}
	shaOK := 0
	for _, sr := range res.Segments {
		if sr.Status != "quarantined" && len(sr.FragmentSHA) == 64 {
			shaOK++
		}
	}
	return map[string]any{
		"scenario":    "faults",
		"recovered":   fmt.Sprintf("%d/%d", res.Stats.Recovered, res.Stats.Total),
		"roundtrip":   fmt.Sprintf("%d/%d", res.Stats.RoundtripOK, res.Stats.Recovered),
		"sha256":      fmt.Sprintf("%d/%d", shaOK, res.Stats.Recovered),
		"quarantined": res.Stats.Quarantined,
	}, nil
}

// resumeScenario re-runs the same batch after tampering with one segment's
// cached entry. The 10 untouched recovered segments are cache hits; the forged
// segment is rejected and recomputed (rebuilt=1), and no sample is ingested
// twice. Recovered fragment hashes must match the first run exactly.
func resumeScenario(faultRoot string) (map[string]any, error) {
	entries, err := fixtgen.GenerateAll(faultRoot)
	if err != nil {
		return nil, err
	}
	before := snapshotHashes(faultRoot, "demo-batch")

	// Forge one cache entry (seg11): invalidate it and force recomputation.
	runner, err := pipeline.NewRunner(faultRoot, "demo-batch")
	if err != nil {
		return nil, err
	}
	forged := ""
	for _, e := range entries {
		if e.Fault == fixtgen.FaultForgedCache {
			forged = e.ID
		}
	}
	if forged == "" {
		return nil, media.NewFault(media.KindInvalidManifest, "demo-batch", 0, -1, "no forged-cache segment in plan")
	}
	if err := runner.ForceRecomputeSegment(forged); err != nil {
		return nil, err
	}

	items := make([]pipeline.BatchItem, len(entries))
	for i, e := range entries {
		items[i] = pipeline.BatchItem{ID: e.ID, SourcePath: filepath.Join(faultRoot, "sources", e.Source)}
	}
	res, err := runner.Run(items)
	if err != nil {
		return nil, err
	}
	after := snapshotHashes(faultRoot, "demo-batch")
	if len(before) > 0 && !mapsEqual(before, after) {
		return nil, media.NewFault(media.KindDigestMismatch, "demo-batch", 0, -1,
			"recovered fragment hashes changed on resume")
	}

	return map[string]any{
		"scenario":   "resume",
		"cache_hit":  fmt.Sprintf("%d/%d", res.Stats.CacheHits, res.Stats.Recovered),
		"rebuilt":    res.Stats.Recovered - res.Stats.CacheHits,
		"duplicates": res.Stats.Duplicates,
	}, nil
}

func toRemuxInputs(tracks []demux.TrackSamples) []remux.TrackInput {
	in := make([]remux.TrackInput, 0, len(tracks))
	for _, tr := range tracks {
		in = append(in, remux.TrackInput{Meta: tr.Meta, Samples: tr.Samples})
	}
	return in
}

func snapshotHashes(root, batch string) map[string]string {
	cp, err := archive.OpenCheckpoint(root, batch, demux.FormatVersion)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, st := range cp.Snapshot() {
		if st.Status == archive.StatusRecovered {
			out[st.Segment] = st.FragmentSHA
		}
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
