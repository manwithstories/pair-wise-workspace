package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"archive.local/archive/internal/cache"
	"archive.local/archive/internal/checkpoint"
	"archive.local/archive/internal/manifest"
	"archive.local/archive/internal/pipeline"
)

// printResult renders the human-readable summary. Field names that the
// acceptance script greps for are printed verbatim.
func printResult(res *pipeline.Result) {
	if res == nil {
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	totalSamples := 0
	for _, t := range res.Tracks {
		totalSamples += t.Samples
		fmt.Fprintf(w, "track\t%d\tsamples\t%d\tkeyframes\t%d\n", t.TrackID, t.Samples, t.Keyframes)
	}
	fmt.Fprintf(w, "tracks\t%d\n", len(res.Tracks))
	fmt.Fprintf(w, "samples_total\t%d\n", totalSamples)
	fmt.Fprintf(w, "recovered_samples\t%d\n", totalSamples)
	fmt.Fprintf(w, "regressed_timestamps\t%d\n", sumTracks(res, func(t pipeline.TrackStats) int { return t.Regressed }))
	fmt.Fprintf(w, "repaired_gaps\t%d\n", sumTracks(res, func(t pipeline.TrackStats) int { return t.Missing }))
	fmt.Fprintf(w, "discontinuities\t%d\n", sumTracks(res, func(t pipeline.TrackStats) int { return t.Gaps }))
	fmt.Fprintf(w, "duplicate_packets\t%d\n", res.Transport.Duplicate)
	fmt.Fprintf(w, "reordered_packets\t%d\n", res.Transport.Reordered)
	fmt.Fprintf(w, "burst_loss_packets\t%d\n", res.Transport.BurstLoss)
	fmt.Fprintf(w, "gap_ranges\t%d\n", res.Transport.GapRanges)
	fmt.Fprintf(w, "sequence_wraps\t%d\n", res.Transport.Wraps)
	fmt.Fprintf(w, "adaptive_batches\t%d\n", res.Transport.Batches)
	fmt.Fprintf(w, "confirmed_seq_high\t%d\n", res.Transport.ConfirmedHigh)
	fmt.Fprintf(w, "cache_hits\t%d\n", res.CacheHits)
	fmt.Fprintf(w, "cache_misses\t%d\n", res.CacheMisses)
	fmt.Fprintf(w, "cache_rejected\t%d\n", res.CacheRejected)
	fmt.Fprintf(w, "index_blocks\t%d/%d\n", res.BlocksDone, res.BlocksTotal)
	fmt.Fprintf(w, "resumed\t%t\n", res.ResumeUsed)
	fmt.Fprintf(w, "resume_ms\t%d\n", res.ResumeMs)
	fmt.Fprintf(w, "damage_offset_bytes\t%d\n", res.DamageOffset)
	fmt.Fprintf(w, "index_sha256\t%s\n", res.IndexDigest)
	if res.Defect != nil {
		fmt.Fprintf(w, "first_error_code\t%s\n", res.Defect.Code)
		fmt.Fprintf(w, "first_error\t%s\n", res.Defect.Error())
		if res.Defect.Recoverable != nil {
			rc := res.Defect.Recoverable
			fmt.Fprintf(w, "recoverable_samples\t%d..%d\n", rc.FirstSample, rc.LastSample)
			fmt.Fprintf(w, "recoverable_bytes\t%d..%d\n", rc.FirstByte, rc.EndByte)
		}
	} else {
		fmt.Fprintf(w, "first_error_code\t-\n")
	}
	fmt.Fprintf(w, "status\t%s\n", statusName(res.Status))
	fmt.Fprintf(w, "elapsed_ms\t%d\n", res.Elapsed.Milliseconds())
	_ = w.Flush()
}

func sumTracks(res *pipeline.Result, f func(pipeline.TrackStats) int) int {
	sum := 0
	for _, t := range res.Tracks {
		sum += f(t)
	}
	return sum
}

func statusName(s pipeline.Status) string {
	switch s {
	case pipeline.OK:
		return "ok"
	case pipeline.Damaged:
		return "damaged"
	default:
		return "failed"
	}
}

// summaryJSON is the machine-readable form, keyed exactly like the text fields.
type summaryJSON struct {
	DamageOffset     int64            `json:"damage_offset_bytes"`
	RecoveredSamples int              `json:"recovered_samples"`
	ResumeMs         int64            `json:"resume_ms"`
	Resumed          bool             `json:"resumed"`
	DuplicatePackets int              `json:"duplicate_packets"`
	ReorderedPackets int              `json:"reordered_packets"`
	BurstLossPackets int              `json:"burst_loss_packets"`
	CacheRejected    int              `json:"cache_rejected"`
	CacheHits        int              `json:"cache_hits"`
	IndexSHA256      string           `json:"index_sha256"`
	IndexBlocksDone  int              `json:"index_blocks_done"`
	IndexBlocksTotal int              `json:"index_blocks_total"`
	Status           string           `json:"status"`
	FirstErrorCode   string           `json:"first_error_code,omitempty"`
	Tracks           []map[string]any `json:"tracks"`
}

func emitJSON(res *pipeline.Result) {
	j := summaryJSON{
		DamageOffset:     res.DamageOffset,
		RecoveredSamples: sumTracks(res, func(t pipeline.TrackStats) int { return t.Samples }),
		ResumeMs:         res.ResumeMs,
		Resumed:          res.ResumeUsed,
		DuplicatePackets: res.Transport.Duplicate,
		ReorderedPackets: res.Transport.Reordered,
		BurstLossPackets: res.Transport.BurstLoss,
		CacheRejected:    res.CacheRejected,
		CacheHits:        res.CacheHits,
		IndexSHA256:      res.IndexDigest,
		IndexBlocksDone:  res.BlocksDone,
		IndexBlocksTotal: res.BlocksTotal,
		Status:           statusName(res.Status),
	}
	if res.Defect != nil {
		j.FirstErrorCode = string(res.Defect.Code)
	}
	for _, t := range res.Tracks {
		j.Tracks = append(j.Tracks, map[string]any{
			"track_id": t.TrackID, "samples": t.Samples, "keyframes": t.Keyframes,
			"regressed": t.Regressed, "missing": t.Missing, "gaps": t.Gaps,
		})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(j)
}

// runVerify re-checks a prior run's checkpoint and index digest without
// touching the source material.
func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	workdir := fs.String("workdir", "", "working directory of the previous run")
	if err := fs.Parse(args); err != nil {
		return exitFailed
	}
	if *workdir == "" {
		fmt.Fprintln(os.Stderr, "verify: --workdir is required")
		return exitFailed
	}
	abs, err := filepath.Abs(*workdir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitFailed
	}
	ck, err := checkpoint.NewStore(filepath.Join(abs, "state"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitFailed
	}
	st, err := ck.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "checkpoint invalid: %v\n", err)
		return exitFailed
	}
	if st == nil {
		fmt.Fprintln(os.Stderr, "no checkpoint found")
		return exitFailed
	}
	mf, err := parseManifest(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return exitFailed
	}
	digest, err := cache.FileDigest(filepath.Join(abs, "material.txt"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return exitFailed
	}
	if st.InputDigest != digest {
		fmt.Fprintln(os.Stderr, "input digest changed since the checkpoint")
		return exitFailed
	}
	fmt.Printf("manifest\t%s\n", mf.Path)
	fmt.Printf("checkpoint_input_digest\t%s\n", st.InputDigest)
	fmt.Printf("completed_blocks\t%d\n", len(st.Completed))
	fmt.Printf("index_sha256\t%s\n", st.IndexDigest)
	fmt.Printf("confirmed_seq_high\t%d\n", st.ConfirmedHigh)
	sortBlocksForReport(st)
	return exitOK
}

func sortBlocksForReport(st *checkpoint.State) {
	sort.Slice(st.Completed, func(i, j int) bool {
		a, b := st.Completed[i].ID, st.Completed[j].ID
		if a.TrackID != b.TrackID {
			return a.TrackID < b.TrackID
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	for _, b := range st.Completed {
		fmt.Printf("block\t%d\t%d\t%d\t%s\n", b.ID.TrackID, b.ID.From, b.ID.To, b.Fingerprint[:12])
	}
}

func parseManifest(dir string) (*manifest.Manifest, error) {
	return manifest.ParseFileWithRoot(filepath.Join(dir, "material.txt"), dir)
}
