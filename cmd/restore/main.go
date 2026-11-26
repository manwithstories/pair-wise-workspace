// Command restore runs the archive verification pipeline entirely on the local
// terminal: it synthesizes damaged RTP/RTCP fixtures, restores MPEG-TS segments
// through reorder/segment/cache/checkpoint stages, emits aligned HLS manifests,
// and prints normal/damage/resume summaries with per-segment hashes.
//
// Exit codes:
//
//	0 - all assertions passed
//	2 - unrecoverable input (fatal media error)
//	3 - source or checkpoint integrity failure
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/archive/tsrestore/internal/cache"
	"github.com/archive/tsrestore/internal/checkpoint"
	"github.com/archive/tsrestore/internal/pipeline"
	"github.com/archive/tsrestore/internal/sample"
	"github.com/archive/tsrestore/internal/timeline"
)

const (
	exitOK        = 0
	exitFatal     = 2
	exitIntegrity = 3
)

// verifyFlags holds the parsed `verify` subcommand options.
type verifyFlags struct {
	seed         int64
	loss         float64
	reorder      int
	slowConsumer time.Duration
	resume       bool
	hashes       bool
	out          string
	seconds      int
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitFatal)
	}
	switch os.Args[1] {
	case "verify":
		code := runVerify(os.Args[2:])
		os.Exit(code)
	case "fixture":
		code := runFixture(os.Args[2:])
		os.Exit(code)
	default:
		usage()
		os.Exit(exitFatal)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr,
		"usage: restore verify --seed N --loss F --reorder N --slow-consumer D --resume --hashes --out DIR")
	fmt.Fprintln(os.Stderr,
		"       restore fixture (truncated|bad-rtcp|tamper-cache|slow-consumer) --out DIR")
}

func parseVerify(args []string) (*verifyFlags, error) {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	v := &verifyFlags{}
	fs.Int64Var(&v.seed, "seed", 41, "deterministic fixture seed")
	fs.Float64Var(&v.loss, "loss", 0.02, "fraction of non-IDR RTP packets dropped")
	fs.IntVar(&v.reorder, "reorder", 9, "reorder perturbation parameter")
	fs.DurationVar(&v.slowConsumer, "slow-consumer", 0, "artificial per-segment sink delay")
	fs.BoolVar(&v.resume, "resume", false, "run a resume phase reusing cache/checkpoint")
	fs.BoolVar(&v.hashes, "hashes", false, "print each segment id and its sha256")
	fs.StringVar(&v.out, "out", "", "output directory")
	fs.IntVar(&v.seconds, "seconds", 360, "program length in seconds")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if v.out == "" {
		return nil, errors.New("--out is required")
	}
	return v, nil
}

// runVerify executes the normal, damage and (optionally) resume phases.
func runVerify(args []string) int {
	v, err := parseVerify(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		return exitFatal
	}
	if err := os.MkdirAll(v.out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "cannot create output dir:", err)
		return exitFatal
	}
	ctx := context.Background()

	// Phase 1+2: one damaged run produces the normal and damage summaries.
	normal, res, code := runNormalAndDamage(ctx, v)
	if code != exitOK {
		return code
	}

	// Phase 3: resume reuses cache and checkpoint; a tampered cache entry is
	// planted first to prove the rejection path (cache_reject=1).
	var resume *resumeSummary
	if v.resume {
		rs, code2 := runResumePhase(ctx, v)
		if code2 != exitOK {
			return code2
		}
		resume = rs
	}

	printReport(v, normal, res, resume)
	return assertAll(v, normal, res, resume)
}

// buildInputs synthesizes every rendition's damaged framed stream.
func buildInputs(v *verifyFlags) []*pipeline.RenditionInput {
	cfg := sample.DamageConfig{Loss: v.loss, ReorderDist: v.reorder, Duplicates: 0.02}
	rends := sample.DefaultRenditions()
	ins := make([]*pipeline.RenditionInput, 0, len(rends))
	for i, r := range rends {
		stream, _, _ := sample.BuildRendition(r, sample.ProgramDuration(v.seconds),
			v.seed+int64(i), cfg)
		ins = append(ins, &pipeline.RenditionInput{
			Name: r.Name, SSRC: r.SSRC, Bandwidth: r.Bandwidth,
			Width: r.Width, Height: r.Height, Datagrams: stream,
		})
	}
	return ins
}

func openStores(dir string) (*cache.Store, *checkpoint.Store, error) {
	return openStoresWithSecret(dir, []byte(fmt.Sprintf("archive-seed-%d", 41)))
}

// openStoresWithSecret opens the CAS and checkpoint under dir bound to secret,
// letting tests simulate a checkpoint integrity failure with the wrong secret.
func openStoresWithSecret(dir string, secret []byte) (*cache.Store, *checkpoint.Store, error) {
	cas, err := cache.Open(filepath.Join(dir, "cas"))
	if err != nil {
		return nil, nil, err
	}
	ckpt, err := checkpoint.Open(filepath.Join(dir, "checkpoint.json"), secret)
	if err != nil {
		return nil, nil, err
	}
	ckpt.SetMeta(41, 360)
	return cas, ckpt, nil
}

// classify maps a pipeline error to the required terminal exit code.
func classify(err error) int {
	var te *timeline.Error
	if errors.As(err, &te) {
		if errors.Is(err, timeline.ErrIntegrity) || errors.Is(err, timeline.ErrCheckpoint) {
			return exitIntegrity
		}
		if te.Class == timeline.ClassFatal {
			return exitFatal
		}
	}
	return exitFatal
}
