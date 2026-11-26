package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"mediad/internal/model"
	"mediad/internal/pipeline"
)

func cmdVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	ckptPath := fs.String("checkpoint", "", "checkpoint JSON path")
	outputPath := fs.String("output", "", "output MP4/MOV path (defaults to checkpoint output)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *ckptPath == "" || fs.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: mediad verify --checkpoint FILE.json [--output FILE.mp4]")
		return 2
	}
	ckpt, err := pipeline.LoadCheckpoint(*ckptPath)
	if err != nil {
		printError(err)
		return model.ExitCode(err)
	}
	out := *outputPath
	if out == "" {
		out = ckpt.OutputPath
	}
	if out == "" {
		fmt.Fprintln(os.Stderr, "--output is required when checkpoint has no output path")
		return 2
	}

	if ckpt.Complete {
		sha, err := pipeline.HashFileSHA256(out)
		if err != nil {
			printError(err)
			return model.ExitCode(err)
		}
		if sha != ckpt.OutputSHA256 {
			err := model.New(model.CodeIntegrity, model.StageCheckpoint, "completed output SHA-256 mismatch").
				WithDigest(ckpt.OutputSHA256, sha)
			printError(err)
			return 4
		}
		fmt.Printf("complete=true\noutput_sha256=%s\nverification=ok\n", sha)
		return 0
	}

	f, err := os.Open(out)
	if err != nil {
		structured := model.Wrap(model.CodeIntegrity, model.StageCheckpoint, "open checkpoint output", err)
		printError(structured)
		return 4
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, ckpt.PrefixBytes); err != nil {
		structured := model.Wrap(model.CodeIntegrity, model.StageCheckpoint, "hash checkpoint prefix", err)
		printError(structured)
		return 4
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != ckpt.PrefixSHA256 {
		err := model.New(model.CodeIntegrity, model.StageCheckpoint, "prefix SHA-256 mismatch").
			WithDigest(ckpt.PrefixSHA256, got)
		printError(err)
		return 4
	}
	fmt.Printf("complete=false\ncommitted_samples=%d\nprefix_sha256=%s\nverification=ok\n",
		ckpt.CommittedSamples, got)
	return 0
}
