package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"archive/internal/index"
	"archive/internal/model"
)

// runVerify independently re-checks the committed output: each segment
// file is re-hashed and compared to the report, and the index manifest and
// chunk checksums are revalidated. No in-memory state from the demo run is
// trusted.
func runVerify(_ context.Context, args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	outDir := fs.String("out", "", "output directory produced by demo")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "parameter error:", err)
		return exitParam
	}
	if *outDir == "" {
		fmt.Fprintln(os.Stderr, "parameter error: --out is required")
		return exitParam
	}

	rep, err := readReport(*outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify failed:", err)
		return exitCorrupt
	}
	if err := verifySegments(*outDir, rep); err != nil {
		fmt.Fprintln(os.Stderr, "verify failed:", err)
		return exitCorrupt
	}
	if err := verifyIndex(*outDir); err != nil {
		fmt.Fprintln(os.Stderr, "verify failed:", err)
		return exitCorrupt
	}
	fmt.Println("verification=PASS")
	printReport(*rep)
	return exitOK
}

func readReport(outDir string) (*report, error) {
	data, err := os.ReadFile(filepath.Join(outDir, "report.json"))
	if err != nil {
		return nil, model.New(model.KindIndex, "report.json", "cannot read report", err)
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, model.New(model.KindIndex, "report.json", "corrupt report json", err)
	}
	return &r, nil
}

func verifySegments(outDir string, r *report) error {
	segDir := filepath.Join(outDir, "segments")
	found := 0
	for i := 0; i < r.Segments; i++ {
		name := filepath.Join(segDir, fmt.Sprintf("segment_%04d.m4s", i))
		data, err := os.ReadFile(name)
		if err != nil {
			return model.New(model.KindIO, name, "segment missing", err)
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if i >= len(r.SegmentSHA256) || got != r.SegmentSHA256[i] {
			return model.Newf(model.KindIndex, name, "segment hash mismatch: got %s", got)
		}
		found++
	}
	if found != r.Segments {
		return model.Newf(model.KindIndex, "segments", "expected %d segments, found %d", r.Segments, found)
	}
	return nil
}

func verifyIndex(outDir string) error {
	dir := filepath.Join(outDir, "index")
	manifest, err := index.LoadManifest(dir)
	if err != nil {
		return err
	}
	if manifest == nil {
		return model.New(model.KindIndex, "manifest.json", "index manifest missing", nil)
	}
	res, err := index.VerifyAndResume(dir, manifest.InputDigest, manifest.ParamDigest)
	if err != nil {
		return err
	}
	if res.CompleteChunks != index.CountCompleteChunks(manifest) {
		return model.Newf(model.KindIndex, "chunks",
			"complete chunk mismatch: verified=%d manifest=%d", res.CompleteChunks,
			index.CountCompleteChunks(manifest))
	}
	return nil
}
