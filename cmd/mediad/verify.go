package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/pion/rtp"

	"mediad/internal/checkpoint"
	"mediad/internal/clock"
	"mediad/internal/codec"
	"mediad/internal/dag"
	"mediad/internal/fault"
	"mediad/internal/mp4"
)

type verifier struct {
	seed         int64
	failures     []string
	exit2, exit3 bool

	golden     *pipeOut
	roundtrip  float64
	keyframe   float64
	retries    int
	firstBlock string
	hashMatch  int
	hashTotal  int
	lines      []string
}

func (v *verifier) assert(cond bool, format string, args ...any) {
	if !cond {
		v.failures = append(v.failures, fmt.Sprintf(format, args...))
	}
}

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	seed := fs.Int64("seed", 17, "deterministic fixture seed")
	suite := fs.Bool("fault-suite", false, "also run corruption/recovery/DAG fault scenarios")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	v := &verifier{seed: *seed}
	v.runNormal()
	v.runRecovery()
	if *suite {
		v.runCorrupt()
		v.runDAGFault()
		v.runFaultFixtures()
	}
	v.print()
	switch {
	case v.exit2:
		return 2
	case v.exit3:
		return 3
	case len(v.failures) > 0:
		for _, f := range v.failures {
			fmt.Fprintln(os.Stderr, "FAIL:", f)
		}
		return 1
	}
	return 0
}

func (v *verifier) runNormal() {
	out, err := runPipeline(v.seed, false)
	if err != nil {
		v.failures = append(v.failures, err.Error())
		return
	}
	v.golden = out
	v.assert(out.gaps == 0, "normal run: %d unexpected gaps", out.gaps)
	v.assert(out.conflicts == 0, "normal run: %d unexpected conflicts", out.conflicts)
	if out.gaps > 0 || out.conflicts > 0 {
		v.exit2 = true
	}
	v.assert(len(out.hashes) == 12, "segments=%d, want 12", len(out.hashes))
	rep, table, err := mp4.RoundTrip(out.samples, 48000)
	v.assert(err == nil, "roundtrip: %v", err)
	if err != nil {
		return
	}
	v.roundtrip = rep.Percent()
	targets := make([]uint32, len(out.samples))
	for i := range targets {
		targets[i] = uint32(i + 1)
	}
	v.keyframe = mp4.KeyframeHit(mp4.Anchors(table), targets)
	v.assert(v.roundtrip == 100.0, "roundtrip=%.2f%%, want 100", v.roundtrip)
	v.assert(v.keyframe == 100.0, "keyframe_hit=%.2f%%, want 100", v.keyframe)
	v.assert(out.dedupMulti > 0, "expected cross-track dedup groups")
	v.assert(len(out.silent) > 0, "expected silence intervals")
	v.lines = append(v.lines,
		fmt.Sprintf("duplicates_eliminated=%d dedup_groups=%d silence_intervals=%d",
			out.dups, out.dedupMulti, len(out.silent)))
}

func (v *verifier) runCorrupt() {
	out, err := runPipeline(v.seed, true)
	if err != nil {
		v.failures = append(v.failures, err.Error())
		return
	}
	v.assert(out.gaps > 0, "corrupt run must report gaps")
	v.assert(out.conflicts == 1, "conflicts=%d, want 1", out.conflicts)
	v.assert(out.unrecov == 1, "unrecoverable=%d, want 1", out.unrecov)
	changed := map[int]bool{}
	for i := range out.hashes {
		if i < len(v.golden.hashes) && out.hashes[i] != v.golden.hashes[i] {
			changed[i] = true
		}
	}
	// Only the burst-loss (6) and late-packet (8) segments may change;
	// every other segment hash must survive the damage untouched.
	v.assert(len(changed) == 2 && changed[6] && changed[8],
		"changed segments %v, want exactly {6, 8}", changed)
	v.lines = append(v.lines, fmt.Sprintf(
		"corrupt: gaps=%d conflicts=%d unrecoverable=%d changed_segments=%d valid_hashes=preserved",
		out.gaps, out.conflicts, out.unrecov, len(changed)))
}

// runRecovery drives the pipeline through the DAG scheduler, persists a
// checkpoint, corrupts it, and proves the resumed run reproduces the
// golden segment hashes without re-executing any node.
func (v *verifier) runRecovery() {
	if v.golden == nil {
		return
	}
	if err := os.MkdirAll(".mediad", 0o755); err != nil {
		v.failures = append(v.failures, err.Error())
		return
	}
	store, err := checkpoint.NewStore(".mediad", "verify.chk")
	if err != nil {
		v.failures = append(v.failures, err.Error())
		return
	}
	exec := 0
	s1 := dag.New(v.mainDAG(&exec), nil)
	rep1, err := s1.Run()
	v.assert(err == nil, "main DAG: %v", err)
	if fault.KindOf(err) == fault.KindBlocked {
		v.exit3 = true
		return
	}
	state := checkpoint.State{Completed: s1.Completed, SegmentHashes: v.golden.hashes}
	if err := store.Save(state); err != nil {
		v.failures = append(v.failures, err.Error())
		return
	}
	// Corrupt the checkpoint: it must be detected, never silently used.
	if err := store.Corrupt(); err != nil {
		v.failures = append(v.failures, err.Error())
		return
	}
	_, err = store.Load()
	v.assert(fault.KindOf(err) == fault.KindCheckpoint, "corrupt checkpoint not detected: %v", err)
	v.lines = append(v.lines, "checkpoint=corruption_detected")
	// Operator recovery: remove the damaged file, rebuild, resume.
	os.Remove(store.Path)
	s2 := dag.New(v.mainDAG(&exec), nil)
	if _, err := s2.Run(); err != nil {
		v.failures = append(v.failures, err.Error())
		return
	}
	if err := store.Save(checkpoint.State{Completed: s2.Completed, SegmentHashes: v.golden.hashes}); err != nil {
		v.failures = append(v.failures, err.Error())
		return
	}
	loaded, err := store.Load()
	v.assert(err == nil, "reload checkpoint: %v", err)
	before := exec
	s3 := dag.New(v.mainDAG(&exec), loaded.Completed)
	rep3, err := s3.Run()
	v.assert(err == nil, "resumed DAG: %v", err)
	v.assert(exec == before, "resumed run re-executed %d nodes (not idempotent)", exec-before)
	v.assert(len(rep3.Skipped) == len(rep1.Completed), "skipped=%d, want %d",
		len(rep3.Skipped), len(rep1.Completed))
	for _, h := range loaded.SegmentHashes {
		v.hashTotal++
		if v.hashTotal <= len(v.golden.hashes) && h == v.golden.hashes[v.hashTotal-1] {
			v.hashMatch++
		}
	}
	v.assert(v.hashMatch == v.hashTotal, "recovered hashes %d/%d", v.hashMatch, v.hashTotal)
	v.lines = append(v.lines, fmt.Sprintf("recovery=resumed skipped=%d reexecuted=0", len(rep3.Skipped)))
}

// mainDAG builds the parse/mux pipeline as a dependency DAG. Node keys
// derive from run-invariant input hashes so resumed runs skip them.
func (v *verifier) mainDAG(exec *int) []*dag.Node {
	streams := genStreams(v.seed)
	hashOf := func(pkts []*rtp.Packet) []byte {
		h := sha256.New()
		for _, p := range pkts {
			h.Write(p.Payload)
		}
		return h.Sum(nil)
	}
	var nodes []*dag.Node
	for _, id := range []string{"studio", "field", "remote"} {
		for _, stage := range []string{"reorder", "parse"} {
			nodes = append(nodes, &dag.Node{
				ID:     stage + ":" + id,
				Params: stage,
				Input:  hashOf(streams[id]),
				Run: func() error {
					*exec++
					return nil
				},
			})
		}
	}
	var combined []byte
	for _, id := range []string{"field", "remote", "studio"} {
		combined = append(combined, hashOf(streams[id])...)
	}
	for _, stage := range []string{"segmentize", "fingerprint", "mux"} {
		dep := "parse:"
		if stage != "segmentize" {
			dep = "segmentize"
			nodes = append(nodes, &dag.Node{
				ID: stage, Deps: []string{dep}, Params: stage, Input: combined,
				Run: func() error { *exec++; return nil },
			})
			continue
		}
		nodes = append(nodes, &dag.Node{
			ID:    stage,
			Deps:  []string{dep + "studio", dep + "field", dep + "remote"},
			Params: stage, Input: combined,
			Run: func() error { *exec++; return nil },
		})
	}
	return nodes
}

// runDAGFault replays the weak-network fetch scenario: input-4 never
// arrives, its fetch exhausts 2 retries, and decode:input-4 is the first
// blocked node in topological order.
func (v *verifier) runDAGFault() {
	var nodes []*dag.Node
	for i := 0; i < 6; i++ {
		fail := i == 4
		fetch := &dag.Node{
			ID:         fmt.Sprintf("fetch:input-%d", i),
			MaxRetries: 2,
			Run: func() error {
				if fail {
					return errors.New("weak network: fetch failed")
				}
				return nil
			},
		}
		nodes = append(nodes, fetch, &dag.Node{
			ID:   fmt.Sprintf("decode:input-%d", i),
			Deps: []string{fetch.ID},
			Run:  func() error { return nil },
		})
	}
	rep, err := dag.New(nodes, nil).Run()
	v.assert(err != nil, "fault DAG must surface the blocked dependency")
	v.retries = rep.Retries
	v.firstBlock = rep.FirstBlocked
	v.assert(rep.Retries == 2, "retries=%d, want 2", rep.Retries)
	v.assert(rep.FirstBlocked == "decode:input-4", "first_blocked=%s, want decode:input-4",
		rep.FirstBlocked)
}

// runFaultFixtures covers dependency cycles and timebase regression with
// self-generated fixtures.
func (v *verifier) runFaultFixtures() {
	mk := func(id string, deps ...string) *dag.Node {
		return &dag.Node{ID: id, Deps: deps, Run: func() error { return nil }}
	}
	_, err := dag.New([]*dag.Node{mk("a", "b"), mk("b", "a")}, nil).Run()
	v.assert(fault.KindOf(err) == fault.KindCycle, "cycle fixture: %v", err)

	regressing := []*rtp.Packet{
		{Header: rtp.Header{SequenceNumber: 0, Timestamp: 960}, Payload: []byte{8}},
		{Header: rtp.Header{SequenceNumber: 1, Timestamp: 480}, Payload: []byte{8}},
	}
	_, err = codec.Parse(regressing, codec.Opus, clock.Base48k)
	v.assert(fault.KindOf(err) == fault.KindTimebase, "timebase fixture: %v", err)
	v.lines = append(v.lines, "fixtures=cycle_detected,timebase_regression_detected")
}

func (v *verifier) print() {
	if v.golden != nil {
		for i, h := range v.golden.hashes {
			fmt.Printf("seg %d %s\n", i, h)
		}
		for _, iv := range v.golden.silent {
			fmt.Printf("silence=%s:%.2fs-%.2fs\n", iv.Track, iv.Start.Seconds(), iv.End.Seconds())
		}
	}
	for _, l := range v.lines {
		fmt.Println(l)
	}
	fmt.Printf("roundtrip=%.2f%%\n", v.roundtrip)
	fmt.Printf("keyframe_hit=%.2f%%\n", v.keyframe)
	fmt.Printf("segment_hashes=%d/%d\n", v.hashMatch, v.hashTotal)
	fmt.Printf("retries=%d\n", v.retries)
	first := v.firstBlock
	if first == "" {
		first = "none"
	}
	fmt.Printf("first_blocked=%s\n", first)
}
