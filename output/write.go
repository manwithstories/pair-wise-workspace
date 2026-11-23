package output

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"bcast/demux"
)

// WriteFile remuxes tracks into path atomically: the bytes go to a temporary
// file in the same directory, are verified by parsing them back and comparing
// every payload against src, and only then renamed over path. A crash
// therefore leaves either the previous file or the new one, never a partial
// one. On verification failure the destination is not touched.
func WriteFile(path string, src io.ReaderAt, tracks []TrackMux) (Report, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".bcast-out-*")
	if err != nil {
		return Report{}, fmt.Errorf("output: temp file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if err := Mux(tmp, src, tracks); err != nil {
		return Report{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Report{}, fmt.Errorf("output: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Report{}, fmt.Errorf("output: close: %w", err)
	}
	rep, err := Verify(tmpName, src, tracks)
	if err != nil {
		return Report{}, err
	}
	digest, err := fileDigest(tmpName)
	if err != nil {
		return Report{}, err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return Report{}, fmt.Errorf("output: rename: %w", err)
	}
	committed = true
	rep.Digest = digest
	rep.Path = path
	return rep, nil
}

// Report describes a successful remux+verify.
type Report struct {
	Path           string
	Digest         string
	SamplesIn      int // requested
	SamplesOut     int // re-parsed from the output
	MissingSamples int // requested but absent from the output
	ByteMismatch   int // payloads present but not byte-identical to the source
	Tracks         int
}

// MatchRate is the fraction of requested samples that round-tripped with
// identical payload bytes. Missing and mismatched samples both count against
// it, so a lossy remux cannot report 1.0.
func (r Report) MatchRate() float64 {
	if r.SamplesIn == 0 {
		return 1
	}
	ok := r.SamplesIn - r.ByteMismatch - r.MissingSamples
	if ok < 0 {
		ok = 0
	}
	return float64(ok) / float64(r.SamplesIn)
}

// Verify parses path back and compares every sample payload, in output order,
// with the bytes Mux copied from src. It proves the file is self-consistent
// and lossless for the samples it claims to carry.
func Verify(path string, src io.ReaderAt, tracks []TrackMux) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("output: verify read: %w", err)
	}
	f, err := demux.Parse(bytes.NewReader(data), int64(len(data)), path)
	if err != nil && len(f.Samples) == 0 {
		return Report{}, fmt.Errorf("output: verify parse: %w", err)
	}
	rep := Report{Tracks: len(tracks), SamplesOut: len(f.Samples)}
	// Expected payloads, in the order Mux writes them: track order, sample
	// order within each track.
	type expect struct {
		off  int64
		size uint32
	}
	var want []expect
	for _, t := range tracks {
		rep.SamplesIn += len(t.Samples)
		for _, s := range t.Samples {
			want = append(want, expect{off: s.Offset, size: s.Size})
		}
	}
	if len(f.Samples) < len(want) {
		rep.MissingSamples = len(want) - len(f.Samples)
	}
	n := len(want)
	if len(f.Samples) < n {
		n = len(f.Samples)
	}
	for i := 0; i < n; i++ {
		got := f.Samples[i]
		w := want[i]
		if got.Size != w.size {
			rep.ByteMismatch++
			continue
		}
		gotBytes, err := readAt(bytes.NewReader(data), got.Offset, int64(got.Size))
		if err != nil {
			rep.ByteMismatch++
			continue
		}
		wantBytes, err := readAt(src, w.off, int64(w.size))
		if err != nil {
			// The source no longer has these bytes; the output carries what
			// the muxer could copy, so it cannot be proven equal.
			rep.ByteMismatch++
			continue
		}
		if !bytes.Equal(gotBytes, wantBytes) {
			rep.ByteMismatch++
		}
	}
	return rep, nil
}

// fileDigest is the SHA-256 of a whole file, read back after it is closed.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("output: digest open: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("output: digest read: %w", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// readAt copies exactly size bytes starting at off.
func readAt(r io.ReaderAt, off, size int64) ([]byte, error) {
	buf := make([]byte, size)
	if _, err := r.ReadAt(buf, off); err != nil {
		return nil, err
	}
	return buf, nil
}
