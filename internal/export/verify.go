package export

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"medpipe/internal/fault"

	mp4 "github.com/abema/go-mp4"
)

// Verify re-checks every artifact in an output directory against the
// manifest: file hashes, frame well-formedness and count, provenance
// line count, segment concatenation and MP4 structure. It returns the
// recorded metrics so the caller can print them.
func Verify(_ context.Context, outDir string) (*Metrics, error) {
	if err := CheckSafePath(outDir); err != nil {
		return nil, err
	}
	mdata, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		return nil, fault.Wrap(fault.KindResource, fault.Unknown, fault.Unknown,
			0, false, err, "manifest missing")
	}
	var man Manifest
	if err := json.Unmarshal(mdata, &man); err != nil {
		return nil, fault.Wrap(fault.KindCorrupt, fault.Unknown, fault.Unknown,
			0, false, err, "manifest corrupt")
	}
	m := man.Metrics

	// 1. Every recorded output file must hash to its recorded value.
	for rel, want := range man.Outputs {
		sum, err := hashFile(filepath.Join(outDir, rel))
		if err != nil {
			return &m, fault.Wrap(fault.KindResource, fault.Unknown,
				fault.Unknown, 0, false, err, "output file missing: "+rel)
		}
		if sum != want {
			return &m, fault.New(fault.KindCorrupt, fault.Unknown,
				fault.Unknown, 0, false, "output hash mismatch: "+rel)
		}
	}
	// 2. The output digest must match the recorded one.
	if got := outputDigest(man.Outputs); got != m.OutputSHA256 {
		return &m, fault.New(fault.KindCorrupt, fault.Unknown, fault.Unknown,
			0, false, "output digest mismatch")
	}
	// 3. frames.bin: well-formed frames, sequential indices, exact count.
	if err := verifyFrames(filepath.Join(outDir, "frames.bin"), m.FramesWritten); err != nil {
		return &m, err
	}
	// 4. Segment files concatenated must equal frames.bin.
	if err := verifySegmentConcat(outDir, man.Segments); err != nil {
		return &m, err
	}
	// 5. provenance.jsonl: header + exactly one line per frame.
	if err := verifyProvenance(filepath.Join(outDir, "provenance.jsonl"), m); err != nil {
		return &m, err
	}
	// 6. media.mp4 must parse as a container with a moov box.
	if err := verifyMP4(filepath.Join(outDir, "media.mp4")); err != nil {
		return &m, err
	}
	return &m, nil
}

func verifyFrames(path string, want int) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) != want*FrameSize {
		return fault.New(fault.KindCorrupt, fault.Unknown, fault.Unknown, 0,
			false, fmt.Sprintf("frames.bin size %d, want %d frames", len(data), want))
	}
	for i := 0; i < want; i++ {
		h, ok := ParseFrameHeader(data[i*FrameSize : (i+1)*FrameSize])
		if !ok || h.Index != uint64(i) {
			return fault.New(fault.KindCorrupt, fault.Unknown, int64(i),
				int64(i*FrameSize), false, "frame header invalid")
		}
	}
	return nil
}

func verifySegmentConcat(outDir string, segments []Segment) error {
	var buf bytes.Buffer
	for _, seg := range segments {
		data, err := os.ReadFile(filepath.Join(outDir, segBinRel(seg.Index)))
		if err != nil {
			return err
		}
		buf.Write(data)
	}
	frames, err := os.ReadFile(filepath.Join(outDir, "frames.bin"))
	if err != nil {
		return err
	}
	if !bytes.Equal(buf.Bytes(), frames) {
		return fault.New(fault.KindCorrupt, fault.Unknown, fault.Unknown, 0,
			false, "segments do not concatenate to frames.bin")
	}
	return nil
}

func verifyProvenance(path string, m Metrics) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
	if len(lines) != m.FramesWritten+1 {
		return fault.New(fault.KindCorrupt, fault.Unknown, fault.Unknown, 0,
			false, fmt.Sprintf("provenance lines %d, want %d", len(lines), m.FramesWritten+1))
	}
	var head ManifestHeader
	if err := json.Unmarshal(lines[0], &head); err != nil {
		return fault.Wrap(fault.KindCorrupt, fault.Unknown, fault.Unknown,
			0, false, err, "provenance header corrupt")
	}
	if len(head.Anomalies) != m.ProvenanceErrors {
		return fault.New(fault.KindCorrupt, fault.Unknown, fault.Unknown, 0,
			false, "provenance anomaly count mismatch")
	}
	return nil
}

func verifyMP4(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := mp4.Probe(f)
	if err != nil {
		return fault.Wrap(fault.KindCorrupt, fault.Unknown, fault.Unknown,
			0, false, err, "mp4 probe failed")
	}
	if info == nil || len(info.Tracks) == 0 || info.Timescale == 0 {
		return fault.New(fault.KindCorrupt, fault.Unknown, fault.Unknown,
			0, false, "mp4 missing tracks or timing")
	}
	return nil
}
