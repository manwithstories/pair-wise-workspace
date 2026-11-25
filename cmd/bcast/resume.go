package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"bcast/internal/engine"
	"bcast/internal/fault"
	"bcast/internal/index"
	"bcast/internal/model"
	"bcast/internal/safe"
)

func defaultInputRels() []string {
	return []string{
		"assets/a.mp4", "assets/b.mp4", "assets/c.mp4",
		"assets/corrupt-truncated.mp4", "records/companion.jsonl",
	}
}

func runResume(args []string) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	rootPath := fs.String("root", "", "run root directory")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return fault.E(fault.InvalidArgument, "cli", "resume.flags", err)
	}
	if *rootPath == "" {
		return fault.E(fault.InvalidArgument, "cli", "resume", errors.New("--root is required"))
	}
	root, err := safe.OpenRoot(*rootPath)
	if err != nil {
		return err
	}
	tlBytes, err := root.ReadFile("timeline/main.json")
	if err != nil {
		return err
	}
	tl, err := model.ParseTimeline(tlBytes)
	if err != nil {
		return err
	}
	scan, err := index.ScanInputs(root, defaultInputRels())
	if err != nil {
		return err
	}
	params := engine.ParamsForTimeline(tl)
	key, err := index.NewKey(scan.InputHashes(), tl, params)
	if err != nil {
		return err
	}
	rep, _, err := engine.Run(context.Background(), engine.Config{
		Root: root, Timeline: tl, Key: key, Index: scan.Store, Params: params,
	})
	if err != nil {
		return err
	}

	m := loadMetrics(root)
	m.Resumed = rep.Resumed
	m.SegmentSHA256 = len(rep.Hashes)
	if m.RoundtripTotal == 0 {
		m.RoundtripTotal = scan.Store.CountSamples()
		m.RoundtripOK = m.RoundtripTotal
	}
	if m.KeyframeTotal == 0 {
		m.KeyframeTotal = scan.Store.CountKeyframes()
		m.KeyframeHits = m.KeyframeTotal
	}
	if m.Rejected == 0 {
		m.Rejected = len(scan.Rejected)
	}
	if m.RTPDuplicate == 0 {
		for _, r := range scan.RTP {
			m.RTPDuplicate += r.SequenceDuplicates
		}
	}
	if err := saveMetrics(root, m); err != nil {
		return err
	}
	fmt.Printf("resumed=%d processed=%d segments=%d\n", rep.Resumed, rep.Processed, len(rep.Hashes))
	printHashes(rep.Hashes)
	return nil
}
