package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"mediacore/internal/manifest"
	"mediacore/internal/media"
	"mediacore/internal/mux"
	"mediacore/internal/pipeline"
)

// verifyCompleted re-verifies a finished delivery without writing anything.
// This is what makes "run resume twice" idempotent: the second run proves the
// delivered bytes are unchanged instead of replaying the copy.
func verifyCompleted(st *runState, f *demoFlags, cp *pipeline.Checkpoint, stdout io.Writer) error {
	start := time.Now()
	if cp.InputFingerprint != st.container.Fingerprint {
		return media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint was written for a different input")
	}
	if cp.MappingSign != st.plan.Sign() {
		return media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint was written for a different track mapping")
	}
	out, err := os.Open(st.outputPath)
	if err != nil {
		return media.NewError(media.ClassCheckpointCorrupt,
			"checkpoint names output %s but the file is missing", st.outputPath).WithCause(err)
	}
	defer out.Close()

	plan := st.plan.Outputs()
	remuxTracks := make([]mux.TrackResult, 0, len(st.container.Tracks))
	buf := make([]byte, 64<<10)
	for _, t := range st.container.Tracks {
		key := t.ID.String()
		wantOut, ok := cp.FinalSHA[key]
		if !ok {
			return media.NewError(media.ClassCheckpointCorrupt,
				"checkpoint has no final hash for track %s", key)
		}
		gotOut, err := hashOutputTrack(out, cp, plan[key], buf)
		if err != nil {
			return err
		}
		wantIn := cp.FinalInputSHA[key]
		gotIn, err := hashSourceSamples(st.src, t, buf)
		if err != nil {
			return err
		}
		if gotOut != wantOut {
			return media.NewError(media.ClassCheckpointCorrupt,
				"delivered track %s no longer matches its checkpoint hash", key).
				WithContainer(t.ID.Container).WithTrack(t.ID.Value).WithOffset(0)
		}
		if wantIn != "" && gotIn != wantIn {
			return media.NewError(media.ClassCheckpointCorrupt,
				"input track %s no longer matches the checkpoint", key).
				WithContainer(t.ID.Container).WithTrack(t.ID.Value).WithOffset(0)
		}
		remuxTracks = append(remuxTracks, mux.TrackResult{
			Key: key, OutputID: plan[key], Samples: t.SampleCount(),
			InputSHA256: gotIn, OutputSHA256: gotOut,
		})
	}
	// Preserve the original output size; the file is not touched.
	info, err := out.Stat()
	if err != nil {
		return media.NewError(media.ClassIO, "cannot stat output").WithCause(err)
	}
	res := &mux.Result{OutputPath: st.outputPath, OutputSize: info.Size(), Samples: cp.TotalSamples, Tracks: remuxTracks, Resumed: true}
	types := map[string]media.MediaType{}
	codecs := map[string]string{}
	for _, t := range st.container.Tracks {
		types[t.ID.String()] = t.Type
		codecs[t.ID.String()] = t.Codec
	}
	m := manifest.Build(manifest.Input{
		Inputs:       []string{st.src.Name()},
		Adapter:      st.adapter.Format(),
		Version:      st.adapter.Version(),
		Fingerprint:  st.container.Fingerprint,
		Output:       st.outputPath,
		OutputSize:   res.OutputSize,
		TotalSamples: cp.TotalSamples,
		UnknownBoxes: st.container.UnknownBoxes,
		QueuePeak:    st.queuePeak,
		Remux:        res,
		RemuxTracks:  remuxTracks,
		Resolved:     st.resolved,
		TrackTypes:   types,
		TrackCodecs:  codecs,
	})
	if err := writeManifest(m, st.manifest); err != nil {
		return err
	}
	// A verification run reports only the resume contract; the delivery
	// summary belongs to the run that produced the output.
	fmt.Fprintf(stdout, "resume_ms=%d replay_samples=0 output_sha256=%s\n",
		time.Since(start).Milliseconds(), digestOf(remuxTracks))
	return nil
}

// hashOutputTrack hashes the delivered sample spans of one output track,
// identified by its assigned output ID rather than its input identity.
func hashOutputTrack(f *os.File, cp *pipeline.Checkpoint, outID uint32, buf []byte) (string, error) {
	all, err := readOutputSamples(f)
	if err != nil {
		return "", err
	}
	// The delivered track carries its output ID as the container identity.
	spans, ok := all[fmt.Sprintf("mp4:%d", outID)]
	if !ok {
		return "", media.NewError(media.ClassCheckpointCorrupt,
			"output has no track with output ID %d", outID)
	}
	h := sha256.New()
	for _, span := range spans {
		if err := hashSpan(f, buf, h, span[0], span[1]); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashSourceSamples(src media.Source, t *media.Track, buf []byte) (string, error) {
	h := sha256.New()
	for i := 0; i < t.SampleCount(); i++ {
		s, err := t.Index.At(i)
		if err != nil {
			return "", err
		}
		if err := hashSpan(src, buf, h, s.Offset, s.Size); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type spanReader interface {
	ReadAt(p []byte, off int64) (int, error)
}

func hashSpan(r spanReader, buf []byte, h io.Writer, off, size int64) error {
	remaining := size
	for remaining > 0 {
		chunk := int64(len(buf))
		if chunk > remaining {
			chunk = remaining
		}
		n, err := r.ReadAt(buf[:chunk], off)
		if n > 0 {
			h.Write(buf[:n])
			off += int64(n)
			remaining -= int64(n)
		}
		if remaining == 0 {
			return nil
		}
		if err != nil {
			return media.NewError(media.ClassTruncated,
				"sample range ends past the available bytes").WithOffset(off).WithCause(err)
		}
		if n == 0 {
			return media.NewError(media.ClassTruncated,
				"no bytes available for a sample range").WithOffset(off)
		}
	}
	return nil
}

// readOutputSamples maps each output track key to its sample byte spans by
// reparsing the delivered MP4. Re-parsing rather than trusting the
// checkpoint means the verification covers the actual container.
func readOutputSamples(f *os.File) (map[string][][2]int64, error) {
	c, err := parseMP4File(f)
	if err != nil {
		return nil, err
	}
	out := make(map[string][][2]int64, len(c.Tracks))
	for _, t := range c.Tracks {
		spans := make([][2]int64, 0, t.SampleCount())
		for i := 0; i < t.SampleCount(); i++ {
			s, err := t.Index.At(i)
			if err != nil {
				return nil, err
			}
			spans = append(spans, [2]int64{s.Offset, s.Size})
		}
		out[t.ID.String()] = spans
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
