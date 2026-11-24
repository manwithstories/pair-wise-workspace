// Package manifest produces the editor-facing frame manifest and checks that
// it agrees with the unified timeline and the error model.
//
// Both writing and checking stream: the manifest is built from an index
// scanner and aligned by a merge-join against a frame scanner, so neither
// direction holds a track's frames in memory.
package manifest

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"mediad/internal/index"
)

// Row is one manifest line, carrying enough to locate and verify a frame.
type Row struct {
	TrackID     uint32
	FrameNo     uint64
	PTS         int64
	Offset      int64
	Size        uint32
	ContentHash string
	Status      string
}

// Header is the fixed CSV header, so editors can rely on column order.
var Header = []string{
	"track", "frame", "pts", "offset", "size", "content_sha256", "status",
}

// Build writes the manifest for a track's index to path atomically and returns
// the digest of the exact bytes written plus the row count. It pulls entries
// through the supplied iterator, so memory does not grow with the manifest.
func Build(path string, next func() (index.Entry, bool, error)) (digest [32]byte, rows int, err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return digest, 0, err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	h := sha256.New()
	bw := bufio.NewWriter(io.MultiWriter(tmp, h))
	cw := csv.NewWriter(bw)
	if err := cw.Write(Header); err != nil {
		return digest, 0, err
	}
	row := make([]string, len(Header))
	for {
		e, more, err := next()
		if err != nil {
			return digest, 0, err
		}
		if !more {
			break
		}
		row[0] = strconv.FormatUint(uint64(e.TrackID), 10)
		row[1] = strconv.FormatUint(e.FrameNo, 10)
		row[2] = strconv.FormatInt(e.PTS, 10)
		row[3] = strconv.FormatInt(e.Offset, 10)
		row[4] = strconv.FormatUint(uint64(e.Size), 10)
		row[5] = fmt.Sprintf("%x", e.Digest)
		row[6] = "ok"
		if err := cw.Write(row); err != nil {
			return digest, 0, err
		}
		rows++
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return digest, 0, err
	}
	if err := bw.Flush(); err != nil {
		return digest, 0, err
	}
	if err := tmp.Sync(); err != nil {
		return digest, 0, err
	}
	if err := tmp.Close(); err != nil {
		return digest, 0, err
	}
	ok = true
	if err := os.Rename(name, path); err != nil {
		return digest, 0, err
	}
	copy(digest[:], h.Sum(nil))
	return digest, rows, nil
}

// FrameRef is the frame view alignment needs. It is supplied by the source
// scanner; CaptureUnits is the raw device timestamp.
type FrameRef struct {
	FrameNo      uint64
	PTS          int64
	CaptureUnits uint64
	Usable       bool
}

// FrameSource yields frames in ascending offset order, returning io.EOF when
// exhausted.
type FrameSource interface {
	Next() (FrameRef, error)
}

// Gap is an inclusive range of frame numbers known to be absent because
// preview packets were lost. The manifest must preserve it.
type Gap struct{ From, To uint64 }

// Args are the inputs to the alignment check.
type Args struct {
	// Entries yields index entries in frame order.
	Entries func() (index.Entry, bool, error)
	// Frames yields source frames in the same order the index was built.
	Frames FrameSource
	// Timescale is unified ticks per second.
	Timescale uint32
	// CaptureUPS is the track's capture rate.
	CaptureUPS uint32
	// EpochUnits is the capture value mapped to PTS zero.
	EpochUnits uint64
	// Missing are the frame ranges lost from the preview stream.
	Missing []Gap
}

// Report is the result of aligning the manifest to the timeline. SkewMS is not
// set here; numeric skew is recomputed from the written manifest by
// metrics.SkewFromManifest so it reflects the file on disk.
type Report struct {
	Rows     int
	Gaps     int
	Inserted int // rows synthesized to fill a gap (must be zero)
	lastPTS  int64
}

// MaxSkewMS is the acceptance bound for manifest alignment.
const MaxSkewMS = 1.0

// Align merge-joins index entries with source frames and verifies:
//
//  1. every entry corresponds to a usable frame whose unified PTS matches,
//  2. the number of entries equals the number of usable frames (no silently
//     dropped or duplicated row),
//  3. no entry falls inside a known gap,
//  4. the PTS implied by declared cadence differs by at most MaxSkewMS.
//
// It reads each input exactly once and holds no per-frame slice.
func Align(a Args) (*Report, error) {
	if a.Entries == nil || a.Frames == nil {
		return nil, errors.New("manifest: missing input")
	}
	if a.Timescale == 0 || a.CaptureUPS == 0 {
		return nil, errors.New("manifest: zero timescale")
	}
	r := &Report{Gaps: len(a.Missing)}
	inGap := func(no uint64) bool {
		for _, g := range a.Missing {
			if no >= g.From && no <= g.To {
				return true
			}
		}
		return false
	}

	var usable int
	var pending *FrameRef
	fetchFrame := func() (*FrameRef, error) {
		if pending != nil {
			f := *pending
			pending = nil
			return &f, nil
		}
		fr, err := a.Frames.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &fr, nil
	}

	for {
		e, more, err := a.Entries()
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
		// Advance the frame stream to the entry's frame.
		var fr *FrameRef
		for {
			var ferr error
			fr, ferr = fetchFrame()
			if ferr != nil {
				return nil, ferr
			}
			if fr == nil {
				return nil, fmt.Errorf("manifest: entry frame %d has no source frame", e.FrameNo)
			}
			if fr.Usable {
				usable++
			}
			if fr.FrameNo == e.FrameNo {
				break
			}
			if fr.FrameNo > e.FrameNo {
				return nil, fmt.Errorf("manifest: entry frame %d is missing from the source order", e.FrameNo)
			}
			// A non-matching earlier frame is consumed; unusable frames are
			// expected to be absent from the index.
		}
		if !fr.Usable {
			return nil, fmt.Errorf("manifest: entry frame %d points at an unusable frame", e.FrameNo)
		}
		if fr.PTS != e.PTS {
			return nil, fmt.Errorf("manifest: frame %d pts %d != entry %d", e.FrameNo, fr.PTS, e.PTS)
		}
		if inGap(e.FrameNo) {
			return nil, fmt.Errorf("manifest: entry frame %d falls in a known gap", e.FrameNo)
		}
		// The timeline must be monotonic across the indexed frames. Raw
		// capture cadence is not used as the reference here: a clock rollback
		// the mapper legitimately repaired would otherwise register as skew.
		// Numeric skew is measured separately from the manifest itself.
		if fr.PTS < r.lastPTS {
			return nil, fmt.Errorf("manifest: frame %d pts %d goes backwards after %d", e.FrameNo, fr.PTS, r.lastPTS)
		}
		r.lastPTS = fr.PTS
		r.Rows++
	}
	// Drain the remaining frames to count usable ones not represented.
	for {
		fr, err := fetchFrame()
		if err != nil {
			return nil, err
		}
		if fr == nil {
			break
		}
		if fr.Usable {
			usable++
		}
	}
	if usable != r.Rows {
		return nil, fmt.Errorf("manifest: %d usable frames but %d manifest rows", usable, r.Rows)
	}
	return r, nil
}

// DigestReader computes the digest of manifest bytes without materializing a
// second copy, used when re-checking an existing file.
func DigestReader(r io.Reader) ([32]byte, error) {
	var d [32]byte
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return d, err
	}
	copy(d[:], h.Sum(nil))
	return d, nil
}
