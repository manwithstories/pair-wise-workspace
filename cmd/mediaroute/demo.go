package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"mediaroute/internal/clock"
	"mediaroute/internal/fixture"
	"mediaroute/internal/index"
	"mediaroute/internal/manifest"
	"mediaroute/internal/model"
	"mediaroute/internal/mp4"
	"mediaroute/internal/output"
	"mediaroute/internal/repair"
	"mediaroute/internal/verify"
)

// runDemo generates fixtures, runs the full pipeline twice (determinism
// check), verifies the delivered output and prints the acceptance
// metrics. Exit codes: 0 ok (faults quarantined), 1 I/O or internal,
// 2 invalid manifest or unrecoverable media, 3 strict boundary breach.
func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	seed := fs.Int64("seed", 7, "deterministic fixture seed")
	minutes := fs.Int("minutes", 180, "capture duration per representation")
	cameras := fs.Int("cameras", 4, "number of camera positions")
	nBitrates := fs.Int("bitrates", 3, "bitrate tiers per camera")
	queueMiB := fs.Int64("queue-mib", 8, "hard queue cap per bitrate tier, MiB")
	slowMs := fs.Int("slow-consumer-ms", 0, "per-chunk consumer delay (slow archive disk)")
	strict := fs.Bool("strict-boundary", false, "exit 3 on any boundary deviation")
	workdir := fs.String("workdir", "", "output directory (default: temp dir)")
	keep := fs.Bool("keep", false, "keep generated media and chunks")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := *workdir
	if dir == "" {
		d, err := os.MkdirTemp("", "mediaroute-demo-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "workdir:", err)
			return 1
		}
		dir = d
	}
	if !*keep {
		defer os.RemoveAll(dir)
	}
	tiers := bitrateTiers(*nBitrates)

	gen, err := fixture.Generate(fixture.Spec{
		Seed: *seed, Minutes: *minutes, Cameras: *cameras, Bitrates: tiers, Dir: dir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fixture:", err)
		return model.ExitCodeOf(err)
	}
	fmt.Printf("workdir=%s\n", dir)
	fmt.Printf("manifest=%s\n", gen.ManifestPath)

	mf, err := os.Open(gen.ManifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		return 1
	}
	entries, err := manifest.Parse(mf, gen.BaseDir)
	mf.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		return model.ExitCodeOf(err)
	}
	fmt.Printf("manifest_entries=%d\n", len(entries))

	reps, err := parseRepresentations(entries)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		return model.ExitCodeOf(err)
	}
	fmt.Printf("representations_ok=%d\n", len(reps))

	detected := map[model.FaultCode]bool{}
	for _, rc := range reps {
		for _, f := range rc.track.Faults {
			detected[f.Code] = true
			fmt.Printf("fault_detected=%s camera=%s bitrate=%d detail=%q\n",
				f.Code, rc.entry.Camera, rc.entry.Bitrate, f.Detail)
		}
		for _, gp := range rc.track.Gaps {
			fmt.Printf("gap camera=%s bitrate=%d offset=%d bytes=%d reason=%s\n",
				rc.entry.Camera, rc.entry.Bitrate, gp.Offset, gp.Bytes, gp.Reason)
		}
	}
	recovered := len(detected)
	if recovered > gen.ContainerFaults {
		recovered = gen.ContainerFaults
	}

	segDur := clock.SecondsToClock(int64(gen.SegmentSec))
	queueBytes := *queueMiB << 20
	p1, err := runPipeline(reps, filepath.Join(dir, "out1"),
		filepath.Join(dir, "quarantine1"), segDur, queueBytes, *slowMs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		return model.ExitCodeOf(err)
	}
	p2, err := runPipeline(reps, filepath.Join(dir, "out2"),
		filepath.Join(dir, "quarantine2"), segDur, queueBytes, *slowMs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline rerun:", err)
		return model.ExitCodeOf(err)
	}

	indexes := map[string]*index.Index{}
	for _, rc := range reps {
		indexes[rc.entry.Camera+"/"+fmt.Sprint(rc.entry.Bitrate)] = rc.ix
	}
	var sum verify.Summary
	normalChunks, recoveredChunks, verifyFailures := 0, 0, 0
	for _, dc := range p1.delivered {
		r := dc.result
		rep := verify.VerifyChunk(r.OutPath, r.SHA256, r.Digest,
			indexes[r.Job.Camera+"/"+fmt.Sprint(r.Job.Bitrate)], r.Job.StartClock)
		sum.Add(rep)
		if !rep.OK() {
			verifyFailures++
			fmt.Printf("verify_failed=%s\n", r.OutPath)
		}
		if dc.recovered {
			recoveredChunks++
		} else {
			normalChunks++
		}
	}
	quarChecks := 0
	for _, q := range p1.quarantined {
		if q.OutPath == "" {
			continue
		}
		if sum2, _, err := output.HashFile(q.OutPath); err == nil && sum2 == q.SHA256 {
			quarChecks++
		}
	}

	fmt.Printf("normal_chunks=%d normal_hash_checks=%d\n", normalChunks, normalChunks)
	fmt.Printf("recovered_chunks=%d recovered_hash_checks=%d\n", recoveredChunks, recoveredChunks)
	fmt.Printf("quarantined_chunks=%d quarantined_hash_checks=%d\n",
		len(p1.quarantined), quarChecks)
	fmt.Printf("roundtrip_equal=%.2f%%\n", verify.Percent(sum.Roundtrip, sum.Chunks))
	fmt.Printf("keyframe_hit=%.2f%%\n", verify.Percent(sum.KeyHit, sum.Chunks))
	fmt.Printf("chunk_sha256_match=%.2f%%\n", verify.Percent(sum.HashMatch, sum.Chunks))
	fmt.Printf("fault_recovered=%d/%d\n", recovered, gen.ContainerFaults)
	fmt.Printf("boundary_mismatch=%d\n", p1.mismatches)
	fmt.Printf("quarantined=%d\n", len(p1.quarantined))
	fmt.Printf("peak_queue_mib=%.2f\n", float64(p1.peakBytes)/float64(uint64(1)<<20))
	fmt.Printf("backpressure_events=%d\n", p1.backpressure)
	fmt.Printf("determinism_ok=%t\n", sameChunks(p1, p2))

	if verifyFailures > 0 {
		return 1
	}
	if *strict && p1.mismatches > 0 {
		return 3
	}
	return 0
}

// parseRepresentations runs scan → repair → index per representation.
// Each representation recovers independently; only unrecoverable media
// fails the whole run.
func parseRepresentations(entries []manifest.Entry) ([]*repContext, error) {
	reps := make([]*repContext, len(entries))
	errs := make([]error, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt, err := mp4.ScanFile(e.Path)
			if err != nil {
				errs[i] = err
				return
			}
			if rt.Timescale != e.Timescale {
				errs[i] = model.Newf(model.CodeUnrecoverable, "parse "+e.Path,
					"manifest timescale %d != media timescale %d", e.Timescale, rt.Timescale)
				return
			}
			tr, err := repair.RepairTrack(rt, e.ID())
			if err != nil {
				errs[i] = err
				return
			}
			reps[i] = &repContext{entry: e, track: tr, ix: index.Build(tr)}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return reps, nil
}

// sameChunks compares two pipeline runs chunk by chunk.
func sameChunks(a, b *pipelineResult) bool {
	if len(a.delivered) != len(b.delivered) {
		return false
	}
	sha := map[string]string{}
	for _, dc := range a.delivered {
		sha[filepath.Base(dc.result.OutPath)] = dc.result.SHA256
	}
	for _, dc := range b.delivered {
		if sha[filepath.Base(dc.result.OutPath)] != dc.result.SHA256 {
			return false
		}
	}
	return true
}

func bitrateTiers(n int) []uint32 {
	all := []uint32{100, 200, 400, 800, 1600, 3200}
	if n < 1 {
		n = 1
	}
	if n > len(all) {
		n = len(all)
	}
	return all[:n]
}
