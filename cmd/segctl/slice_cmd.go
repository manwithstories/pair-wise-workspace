package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"golang.org/x/sync/errgroup"

	"segctl/internal/fixture"
	"segctl/internal/ingest"
	"segctl/internal/tsslice"
	"segctl/internal/verify"
)

func runSlice(args []string) error {
	fs := flag.NewFlagSet("slice", flag.ContinueOnError)
	root := fs.String("root", "testdata/raw", "raw capture root containing <rendition>/arrival.json")
	rend := fs.String("rendition", "", "single rendition to slice; empty slices all")
	out := fs.String("out", "testdata/slices", "output dir (per-rendition subdirs unless --rendition is set)")
	target := fs.Duration("target", 2*time.Second, "target segment duration (1s-10s)")
	window := fs.Int("window", 3, "retransmission window in segment periods")
	if err := fs.Parse(args); err != nil {
		return err
	}
	targetMs := int(target.Milliseconds())
	if targetMs < 1000 || targetMs > 10000 {
		return fmt.Errorf("slice: --target must be between 1s and 10s")
	}

	var renditions []string
	if *rend != "" {
		if err := verify.CheckRendition(*rend); err != nil {
			return err
		}
		renditions = []string{*rend}
	} else {
		entries, err := os.ReadDir(*root)
		if err != nil {
			return fmt.Errorf("slice: read %s: %w", *root, err)
		}
		for _, e := range entries {
			if e.IsDir() {
				renditions = append(renditions, e.Name())
			}
		}
		sort.Strings(renditions)
		if len(renditions) == 0 {
			return fmt.Errorf("slice: no renditions under %s", *root)
		}
	}

	var g errgroup.Group
	for _, r := range renditions {
		outDir := *out
		if *rend == "" {
			outDir = filepath.Join(*out, r)
		}
		g.Go(func() error {
			return sliceOne(*root, r, outDir, targetMs, *window)
		})
	}
	return g.Wait()
}

// sliceOne runs the per-rendition pipeline: ingest reorder/dedup, then
// GOP-boundary slicing, then the sidecar meta used by manifest and verify.
func sliceOne(rawRoot, rendition, outDir string, targetMs, window int) error {
	rawDir := filepath.Join(rawRoot, rendition)
	arrivals, err := fixture.ReadArrivals(filepath.Join(rawDir, "arrival.json"))
	if err != nil {
		return err
	}
	src, err := fixture.ReadSourceMeta(filepath.Join(rawDir, "source_meta.json"))
	if err != nil {
		return err
	}

	session := ingest.NewSession(rendition, window)
	for _, a := range arrivals {
		path := filepath.Join(rawDir, filepath.Clean(a.File))
		crc, err := ingest.CRC32File(path)
		if err != nil {
			return fmt.Errorf("slice: %s: %w", rendition, err)
		}
		session.Push(ingest.Item{Seq: a.Seq, File: path, CRC: crc})
	}
	session.Close()

	ordered := session.Ordered()
	gapBefore := session.GapBefore()
	if len(ordered) == 0 {
		return fmt.Errorf("slice: %s: no segments arrived", rendition)
	}
	inputs := make([]tsslice.Input, len(ordered))
	for i, it := range ordered {
		inputs[i] = tsslice.Input{Path: it.File, GapBefore: gapBefore[i]}
	}
	res, err := tsslice.Slice(inputs, outDir, targetMs)
	if err != nil {
		return fmt.Errorf("slice: %s: %w", rendition, err)
	}

	meta := &ingest.RenditionMeta{
		Rendition:         rendition,
		TargetBitrateKbps: src.BitrateKbps,
		Width:             src.Width,
		Height:            src.Height,
		TargetMs:          targetMs,
		Conflicts:         session.Conflicts(),
		Duplicates:        session.Duplicates(),
		Expired:           session.Expired(),
		RebaseEvents:      res.RebaseEvents,
	}
	for _, s := range res.Segments {
		meta.Segments = append(meta.Segments, ingest.SegmentMeta{
			Seq:        s.Seq,
			File:       s.File,
			DurationMs: s.DurationMs,
			CRC32:      fmt.Sprintf("%08x", s.CRC),
		})
	}
	for _, gp := range res.Gaps {
		meta.Gaps = append(meta.Gaps, ingest.GapMeta{Seq: gp.Seq, Count: gp.Count})
	}
	if err := ingest.WriteMetaAtomic(filepath.Join(outDir, "meta.json"), meta); err != nil {
		return err
	}
	fmt.Printf("sliced rendition=%s segments=%d gaps=%d conflicts=%d dups=%d expired=%d rebase=%d\n",
		rendition, len(res.Segments), len(meta.Gaps), len(meta.Conflicts),
		meta.Duplicates, meta.Expired, res.RebaseEvents)
	return nil
}
