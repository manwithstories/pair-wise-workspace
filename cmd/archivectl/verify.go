package main

import (
	"flag"
	"fmt"
	"os"

	"archivectl/internal/container"
	"archivectl/internal/manifest"
	"archivectl/internal/timeline"
)

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	source := fs.String("source", "", "original source MP4")
	output := fs.String("output", "", "remuxed output MP4")
	manifestPath := fs.String("manifest", "", "event ledger CSV")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *source == "" || *output == "" || *manifestPath == "" {
		fmt.Fprintln(os.Stderr, "verify: -source, -output and -manifest are required")
		return 2
	}

	src, err := container.ParseFile(*source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify source failed: %v\n", err)
		return timeline.ExitCode(err)
	}
	out, err := container.ParseFile(*output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify output failed: %v\n", err)
		return timeline.ExitCode(err)
	}
	fidelity, err := container.VerifyAssets(src, out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integrity verification failed: %v\n", err)
		printFidelity(fidelity, 0, *manifestPath)
		return 1
	}

	entries, err := manifest.ReadCSV(*manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "manifest read failed: %v\n", err)
		return timeline.ExitCode(err)
	}
	sourceEvents := container.CollectEvents(src)
	maxMS, _, err := manifest.CheckEvents(entries, sourceEvents)
	if err != nil {
		fmt.Fprintf(os.Stderr, "manifest verification failed: %v\n", err)
		return 1
	}
	// The output must carry the same event attributions and hashes.
	outputEvents := container.CollectEvents(out)
	if _, _, err := manifest.CheckEvents(entries, outputEvents); err != nil {
		fmt.Fprintf(os.Stderr, "output event verification failed: %v\n", err)
		return 1
	}
	printFidelity(fidelity, maxMS, *manifestPath)
	fmt.Println("verify=ok")
	return 0
}

func printFidelity(r container.FidelityReport, maxMS float64, manifestPath string) {
	manifestHash, _ := manifest.HashFile(manifestPath)
	fmt.Printf("payload_hash_mismatch=%d\n", r.PayloadHashMismatch)
	fmt.Printf("timestamp_mismatch=%d\n", r.TimestampMismatch)
	fmt.Printf("missing_tracks=%d\n", r.MissingTracks)
	fmt.Printf("samples=%d\n", r.SampleCount)
	fmt.Printf("source_payload_bytes=%d\n", r.SourcePayloadBytes)
	fmt.Printf("output_payload_bytes=%d\n", r.OutputPayloadBytes)
	fmt.Printf("byte_count_mismatch=%d\n", r.ByteCountMismatch)
	fmt.Printf("payload_bytes=%d\n", r.PayloadBytes)
	fmt.Printf("manifest_max_error_ms=%.6f\n", maxMS)
	if manifestHash != "" {
		fmt.Printf("manifest_sha256=%s\n", manifestHash)
	}
}
