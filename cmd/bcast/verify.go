package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"

	"bcast/internal/engine"
	"bcast/internal/fault"
	"bcast/internal/fixture"
	"bcast/internal/index"
	"bcast/internal/model"
	"bcast/internal/safe"
)

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	rootPath := fs.String("root", "", "run root directory")
	fullScan := fs.Bool("full-scan", false, "rescan inputs and compare against a clean rerun")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return fault.E(fault.InvalidArgument, "cli", "verify.flags", err)
	}
	if *rootPath == "" {
		return fault.E(fault.InvalidArgument, "cli", "verify", errors.New("--root is required"))
	}
	root, err := safe.OpenRoot(*rootPath)
	if err != nil {
		return err
	}
	persisted := loadMetrics(root)

	tlBytes, err := root.ReadFile("timeline/main.json")
	if err != nil {
		return err
	}
	if _, err := model.ParseTimeline(tlBytes); err != nil {
		return err
	}
	scan, err := index.ScanInputs(root, defaultInputRels())
	if err != nil {
		return err
	}

	mismatch := 0
	totalSamples := scan.Store.CountSamples()
	roundtripOK := 0
	idx, idxOK, lerr := index.LoadStore(root, "state/index.json")
	if lerr != nil {
		mismatch++
	} else if idxOK && idx.CountSamples() == totalSamples {
		roundtripOK = totalSamples
	} else {
		mismatch++
	}

	kfTotal := scan.Store.CountKeyframes()
	kfHits := 0
	for _, asset := range scan.Store.Assets {
		if asset.Rejected {
			continue
		}
		for _, tr := range asset.Tracks {
			for _, n := range tr.Keyframes {
				if n == 0 || int(n) > len(tr.Samples) || !tr.Samples[n-1].Keyframe {
					mismatch++
					continue
				}
				kfHits++
			}
		}
	}

	hashes := make([]string, 0, 8)
	manifests := make([]*manifestLike, 0, 8)
	for i := 0; i < 8; i++ {
		m, err := engine.LoadManifest(root, i)
		if err != nil {
			mismatch++
			continue
		}
		if sha := m.OutputFiles["mp4_sha256"]; sha != "" {
			if err := root.VerifyHash(m.OutputFiles["mp4"], sha); err != nil {
				mismatch++
			}
		} else {
			mismatch++
		}
		hashes = append(hashes, m.SHA256)
		manifests = append(manifests, &manifestLike{Index: m.Segment, SHA: m.SHA256})
	}
	if len(hashes) != 8 {
		mismatch++
	}

	if cp, ok, err := engine.LoadCheckpoint(root); err != nil {
		mismatch++
	} else if ok {
		for _, m := range manifests {
			if cp.SegmentSHA256[m.Index] != m.SHA {
				mismatch++
			}
		}
	} else {
		mismatch++
	}

	if *fullScan {
		seed := persisted.Seed
		if seed == 0 {
			seed = 41
		}
		clean, cerr := cleanReferenceRerun(root.Path(), seed)
		if cerr != nil {
			fmt.Fprintln(os.Stderr, "clean reference error:", cerr)
			mismatch++
		} else if !reflect.DeepEqual(hashes, clean) {
			mismatch += hashDifferences(hashes, clean)
		}
	}

	rtpDup := 0
	for _, r := range scan.RTP {
		rtpDup += r.SequenceDuplicates
	}
	out := Metrics{
		Seed:           persisted.Seed,
		RoundtripOK:    roundtripOK,
		RoundtripTotal: totalSamples,
		KeyframeHits:   kfHits,
		KeyframeTotal:  kfTotal,
		SegmentSHA256:  len(hashes),
		Mismatch:       mismatch,
		Rejected:       len(scan.Rejected),
		RTPDuplicate:   rtpDup,
		Resumed:        persisted.Resumed,
		Invalidated:    persisted.Invalidated,
	}
	_ = saveMetrics(root, out)
	printMetrics(out)
	printHashes(hashes)
	if mismatch != 0 {
		return fault.E(fault.EvidenceMismatch, "cli", "verify", fmt.Errorf("%d evidence mismatch(es)", mismatch))
	}
	if out.RoundtripOK != 24 || out.KeyframeHits != 12 || out.SegmentSHA256 != 8 ||
		out.Rejected != 1 || out.RTPDuplicate != 1 || out.Resumed != 2 || out.Invalidated != 1 {
		return fault.E(fault.EvidenceMismatch, "cli", "verify", errors.New("acceptance counters do not match required values"))
	}
	return nil
}

type manifestLike struct {
	Index int
	SHA   string
}

func cleanReferenceRerun(parent string, seed int) ([]string, error) {
	dir, err := os.MkdirTemp(parent, ".clean-verify-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	set, err := fixture.Generate(dir, seed)
	if err != nil {
		return nil, err
	}
	scan, err := index.ScanInputs(set.Root, defaultInputRels())
	if err != nil {
		return nil, err
	}
	params := engine.DefaultParams()
	key, err := index.NewKey(scan.InputHashes(), set.Timeline, params)
	if err != nil {
		return nil, err
	}
	rep, _, err := engine.Run(context.Background(), engine.Config{
		Root: set.Root, Timeline: set.Timeline, Key: key, Index: scan.Store, Params: params,
	})
	if err != nil {
		return nil, err
	}
	return rep.Hashes, nil
}

func hashDifferences(a, b []string) int {
	n := 0
	max := len(a)
	if len(b) > max {
		max = len(b)
	}
	for i := 0; i < max; i++ {
		var x, y string
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			n++
		}
	}
	return n
}
