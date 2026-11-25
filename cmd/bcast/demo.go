package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"bcast/internal/engine"
	"bcast/internal/fault"
	"bcast/internal/fixture"
	"bcast/internal/index"
	"bcast/internal/provenance"
	"bcast/internal/safe"
)

var errHelp = errors.New("help requested")

func runDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	rootPath := fs.String("root", "", "run root directory")
	seed := fs.Int("seed", 0, "deterministic fixture seed")
	selfcheck := fs.Bool("selfcheck", false, "run built-in acceptance self-checks")
	interruptAfter := fs.Int("interrupt-after", 0, "segments to commit before simulated interrupt")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return fault.E(fault.InvalidArgument, "cli", "demo.flags", err)
	}
	if *rootPath == "" {
		return fault.E(fault.InvalidArgument, "cli", "demo", errors.New("--root is required"))
	}
	if *interruptAfter < 0 {
		return fault.E(fault.InvalidArgument, "cli", "demo", errors.New("--interrupt-after must be non-negative"))
	}
	if err := os.MkdirAll(*rootPath, 0o755); err != nil {
		return fault.E(fault.PathRejected, "cli", "demo.root", err).WithPath(*rootPath)
	}

	set, err := fixture.Generate(*rootPath, *seed)
	if err != nil {
		return err
	}
	root := set.Root
	if err := resetDemoBatch(root); err != nil {
		return err
	}

	scan, err := index.ScanInputs(root, set.InputRels)
	if err != nil {
		return err
	}
	params := engine.ParamsForTimeline(set.Timeline)
	key, err := index.NewKey(scan.InputHashes(), set.Timeline, params)
	if err != nil {
		return err
	}
	store := &index.Store{Key: key, Assets: scan.Store.Assets}
	store.KeyHash = key.Hash()
	if err := store.Save(root, "state/index.json"); err != nil {
		return err
	}
	loaded, ok, err := index.LoadStore(root, "state/index.json")
	if err != nil || !ok {
		return fmt.Errorf("index roundtrip failed: ok=%v err=%w", ok, err)
	}

	rtpDup := 0
	for _, rep := range scan.RTP {
		rtpDup += rep.SequenceDuplicates
	}
	m := Metrics{
		Seed:           *seed,
		RoundtripOK:    loaded.CountSamples(),
		RoundtripTotal: scan.Store.CountSamples(),
		KeyframeHits:   scan.Store.CountKeyframes(),
		KeyframeTotal:  scan.Store.CountKeyframes(),
		Rejected:       len(scan.Rejected),
		RTPDuplicate:   rtpDup,
	}

	if *selfcheck {
		if err := selfCheckExtras(root, store, key); err != nil {
			return err
		}
		m.Invalidated = 1
		if m.Rejected != 1 {
			return fault.E(fault.EvidenceMismatch, "cli", "demo.selfcheck", fmt.Errorf("rejected=%d want 1", m.Rejected))
		}
		if m.RTPDuplicate != 1 {
			return fault.E(fault.EvidenceMismatch, "cli", "demo.selfcheck", fmt.Errorf("rtp_duplicate=%d want 1", m.RTPDuplicate))
		}
		if m.RoundtripTotal != 24 || m.RoundtripOK != 24 {
			return fault.E(fault.EvidenceMismatch, "cli", "demo.selfcheck", fmt.Errorf("roundtrip=%d/%d want 24/24", m.RoundtripOK, m.RoundtripTotal))
		}
		if m.KeyframeTotal != 12 {
			return fault.E(fault.EvidenceMismatch, "cli", "demo.selfcheck", fmt.Errorf("keyframes=%d want 12", m.KeyframeTotal))
		}
	}
	if err := saveMetrics(root, m); err != nil {
		return err
	}

	rep, _, runErr := engine.Run(context.Background(), engine.Config{
		Root: root, Timeline: set.Timeline, Key: key, Index: store,
		Params: params, InterruptAfter: *interruptAfter,
	})
	if runErr != nil {
		if fault.IsCode(runErr, fault.Interrupted) {
			fmt.Printf("interrupt: committed=%d next_segment=%d resumable=true exit=75\n",
				rep.Processed, rep.Processed)
			return runErr
		}
		return runErr
	}
	m.SegmentSHA256 = len(rep.Hashes)
	m.Resumed = rep.Resumed
	if err := saveMetrics(root, m); err != nil {
		return err
	}
	printHashes(rep.Hashes)
	return nil
}

func resetDemoBatch(root *safe.Root) error {
	for _, rel := range []string{"frames", "mp4", "manifests"} {
		p, err := root.Resolve(rel)
		if err != nil {
			return err
		}
		if err := os.RemoveAll(p); err != nil {
			return fault.E(fault.WriteFailed, "cli", "resetDemoBatch.dir", err).WithPath(rel)
		}
	}
	cp, err := root.Resolve("state/checkpoint.json")
	if err != nil {
		return err
	}
	if err := os.Remove(cp); err != nil && !os.IsNotExist(err) {
		return fault.E(fault.WriteFailed, "cli", "resetDemoBatch.checkpoint", err).WithPath("state/checkpoint.json")
	}
	return nil
}

func selfCheckExtras(root *safe.Root, store *index.Store, key index.Key) error {
	changed := key
	changed.Inputs = make(map[string]string, len(key.Inputs))
	for k, v := range key.Inputs {
		changed.Inputs[k] = v
	}
	changed.Inputs["assets/a.mp4"] = provenance.HashBytes([]byte("selfcheck-mutated-input"))
	if store.Matches(changed) {
		return fault.E(fault.EvidenceMismatch, "cli", "selfcheck.index", errors.New("mutated input did not invalidate index"))
	}

	boundaryAbs, err := root.Resolve("assets/boundary-1frame.mp4")
	if err != nil {
		return err
	}
	bi, err := index.ScanMP4(boundaryAbs, "assets/boundary-1frame.mp4", index.ScanOptions{})
	if err != nil {
		return err
	}
	if len(bi.Tracks) == 0 || len(bi.Tracks[0].Samples) != 1 {
		return fault.E(fault.EvidenceMismatch, "cli", "selfcheck.boundary", errors.New("boundary fixture must contain one sample"))
	}
	return nil
}
