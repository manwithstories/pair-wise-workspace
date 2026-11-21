package driver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"tsbatch/internal/fault"
	"tsbatch/internal/repack"
	"tsbatch/internal/sample"
)

func Demo(args []string) int {
	packets, e1 := flagInt(args, "--packets", 12000)
	workers, e2 := flagInt(args, "--workers", 4)
	queue, e3 := flagInt(args, "--queue", 8)
	slowMS, e4 := flagInt(args, "--slow-consumer-ms", 0)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil || packets < 400 || workers < 1 || queue < 1 {
		return ExitArgs
	}
	dir, err := os.MkdirTemp("", "tsbatch-demo-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	defer os.RemoveAll(dir)

	// --- Path 1: normal stream -------------------------------------
	normal := sample.Generate(sample.Options{Packets: packets})
	nres := runStream(context.Background(), bytes.NewReader(normal),
		repack.New(ioDiscard{}, repack.PolicyIsolate), workers, queue, GroupPackets, 1000)
	printReport("normal", nres)

	// --- Path 2: damaged + boundary stream, repack archived --------
	damaged := sample.Generate(sample.Options{
		Packets: packets, PCRWrap: true,
		CCGapAt: packets / 2, CCGapSize: 2,
		PSIUpdateAt: packets / 2, TruncateBytes: 100,
	})
	src := filepath.Join(dir, "damaged.ts")
	out := filepath.Join(dir, "archive.ts")
	if err := os.WriteFile(src, damaged, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	fw, err := repack.Create(out, src, repack.PolicyIsolate)
	if err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	psiCC := map[uint16]uint8{}
	fw.PSIHook = psiHook(psiCC)
	fw.DelayPerPacket = time.Duration(slowMS) * time.Millisecond
	dres := runStream(context.Background(), bytes.NewReader(damaged), fw.Writer,
		workers, queue, GroupPackets, 1000)
	if err := fw.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	printReport("damaged", dres)
	for _, iso := range fw.Isolations {
		fmt.Printf("recovery_isolation packets=%d-%d reason=%s dropped=%t\n",
			iso.StartPacket, iso.EndPacket, iso.Reason, iso.Dropped)
	}

	// --- Verification: re-parse archive, recompute hashes ----------
	af, err := os.Open(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "io error:", err)
		return ExitIO
	}
	sizes := make([]int, len(fw.Segments))
	for i, s := range fw.Segments {
		sizes[i] = s.Packets
	}
	hashes, outKFs, verr := repack.Verify(af, sizes, sample.MediaPIDs())
	af.Close()
	if verr != nil {
		fmt.Fprintln(os.Stderr, "io error:", verr)
		return ExitIO
	}
	match := 0
	for i, s := range fw.Segments {
		if i < len(hashes) && hashes[i] == s.SHA256 {
			match++
		}
		fmt.Printf("segment_sha256[%d]=%s\n", s.ID, s.SHA256)
	}
	roundtrip := rate(match, len(fw.Segments))
	kfRate := rate(outKFs, len(dres.report.Keyframes))
	manifestPath := out + ".mp4"
	manifestOK := false
	if err := repack.WriteManifest(manifestPath, fw.Segments); err == nil {
		if mh, err := repack.ReadManifest(manifestPath); err == nil && len(mh) == len(fw.Segments) {
			manifestOK = true
			for i := range mh {
				manifestOK = manifestOK && mh[i] == fw.Segments[i].SHA256
			}
		}
	}

	// --- Path 3: slow consumer, downstream write failure, cancel ---
	cancelDrained := runCancelScenario(damaged, workers, queue, slowMS)

	// --- Acceptance fields ------------------------------------------
	fmt.Printf("roundtrip_rate=%.3f\n", roundtrip)
	fmt.Printf("keyframe_hit_rate=%.3f\n", kfRate)
	fmt.Printf("segments=%d\n", len(fw.Segments))
	fmt.Printf("pcr_wraps=%d\n", dres.report.PCRWraps)
	fmt.Printf("cc_gaps=%d\n", dres.report.Faults.Count(fault.CatCCGap))
	fmt.Printf("psi_updates=%d\n", dres.report.PSIUpdates)
	fmt.Printf("peak_queue=%d\n", dres.peak)
	fmt.Printf("cancel_drained=%t\n", cancelDrained)
	fmt.Printf("manifest_ok=%t\n", manifestOK)

	ok := roundtrip == 1.0 && kfRate >= 0.990 &&
		dres.report.PCRWraps == 1 &&
		dres.report.Faults.Count(fault.CatCCGap) >= 1 &&
		dres.report.PSIUpdates == 1 &&
		dres.peak <= int64(queue) && cancelDrained && manifestOK &&
		dres.readErr == nil && dres.parseErr == nil &&
		!dres.report.Faults.HasUnrecoverable()
	if !ok {
		fmt.Println("demo_ok=false")
		return ExitIO
	}
	fmt.Println("demo_ok=true")
	return ExitOK
}

func rate(part, whole int) float64 {
	if whole == 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// failAfterWriter simulates a downstream disk failure after n writes.
type failAfterWriter struct {
	n, limit int
}

func (f *failAfterWriter) Write(p []byte) (int, error) {
	f.n++
	if f.n > f.limit {
		return 0, errors.New("simulated downstream write failure")
	}
	return len(p), nil
}

// runCancelScenario runs the pipeline against a slow, failing consumer
// and reports whether every goroutine drained after cancellation.
func runCancelScenario(stream []byte, workers, queue, slowMS int) bool {
	fw := &failAfterWriter{limit: 50}
	w := repack.New(fw, repack.PolicyIsolate)
	w.DelayPerPacket = time.Duration(slowMS) * time.Millisecond
	done := make(chan *runResult, 1)
	go func() {
		done <- runStream(context.Background(), bytes.NewReader(stream), w,
			workers, queue, GroupPackets, 1000)
	}()
	select {
	case <-done:
		return w.Err() != nil // failure observed AND pool reclaimed
	case <-time.After(15 * time.Second):
		return false
	}
}
