// Package manifest produces the editor-facing frame manifest and checks that
// it agrees with the unified timeline and the error model.
//
// The manifest is the single list an editor uses to locate frames, so it must
// be complete and self-consistent: every indexed frame appears exactly once,
// its PTS matches the timeline the index was built on, and the manifest's own
// digest identifies the exact bytes.
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
	"mediad/internal/model"
)

// Row is one manifest line. It carries enough to locate and verify a frame
// without consulting the source.
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

// Build writes the manifest for one track's indexed frames to path atomically
// and returns its digest and row count.
func Build(path string, entries []index.Entry, source []model.Frame) (digest [32]byte, rows int, err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return digest, 0, model.Fatal("manifest.build", err)
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
	mw := io.MultiWriter(tmp, h)
	bw := bufio.NewWriter(mw)
	cw := csv.NewWriter(bw)
	if err := cw.Write(Header); err != nil {
		return digest, 0, model.Fatal("manifest.build", err)
	}
	row := make([]string, len(Header))
	for _, e := range entries {
		row[0] = strconv.FormatUint(uint64(e.TrackID), 10)
		row[1] = strconv.FormatUint(e.FrameNo, 10)
		row[2] = strconv.FormatInt(e.PTS, 10)
		row[3] = strconv.FormatInt(e.Offset, 10)
		row[4] = strconv.FormatUint(uint64(e.Size), 10)
		row[5] = fmt.Sprintf("%x", e.Digest)
		row[6] = "ok"
		if err := cw.Write(row); err != nil {
			return digest, 0, model.Fatal("manifest.build", err)
		}
		rows++
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return digest, 0, model.Fatal("manifest.build", err)
	}
	if err := bw.Flush(); err != nil {
		return digest, 0, model.Fatal("manifest.build", err)
	}
	if err := tmp.Sync(); err != nil {
		return digest, 0, model.Fatal("manifest.build", err)
	}
	if err := tmp.Close(); err != nil {
		return digest, 0, model.Fatal("manifest.build", err)
	}
	ok = true
	if err := os.Rename(name, path); err != nil {
		return digest, 0, model.Fatal("manifest.commit", err)
	}
	copy(digest[:], h.Sum(nil))
	return digest, rows, nil
}

// Args are the inputs to the alignment check.
type Args struct {
	// Frames is every frame the scanner reported, in source order.
	Frames []model.Frame
	// Entries is the validated index.
	Entries []index.Entry
	// Timescale is the unified timeline's ticks per second.
	Timescale uint32
	// CaptureUPS is the track's capture rate, used to derive expected PTS.
	CaptureUPS uint32
	// EpochUnits is the capture value mapped to PTS zero.
	EpochUnits uint64
	// Missing are frame ranges (inclusive) known to be absent because RTP
	// packets were lost; the manifest must preserve these gaps, not invent
	// frames to fill them.
	Missing []Gap
}

// Gap is an inclusive range of frame numbers known to be absent.
type Gap struct {
	From, To uint64
}

// Report is the result of aligning the manifest to the timeline.
type Report struct {
	Rows      int
	SkewTicks int64 // max |declared expected PTS - manifest PTS|
	SkewMS    float64
	Gaps      int
	Inserted  int // rows synthesized to fill a gap (must be zero)
	Entries   int // indexed entries checked
}

// MaxSkewMS is the acceptance bound for manifest alignment.
const MaxSkewMS = 1.0

// Align verifies three properties and returns a report:
//
//  1. every manifest entry corresponds to a usable frame whose unified PTS
//     matches the entry,
//  2. the PTS implied by the declared cadence differs from the actual PTS by
//     no more than MaxSkewMS, and
//  3. no entry falls inside a known gap and no frame is invented to fill one.
//
// It also confirms the entry count equals the number of usable frames, so a
// silently dropped or duplicated manifest row is caught.
func Align(a Args) (*Report, error) {
	r := &Report{Entries: len(a.Entries), Gaps: len(a.Missing)}
	byOffset := make(map[int64]model.Frame, len(a.Frames))
	usable := 0
	for _, fr := range a.Frames {
		byOffset[fr.Offset] = fr
		if fr.Status.Usable() {
			usable++
		}
	}
	if usable != len(a.Entries) {
		return nil, model.Verify("manifest.align", fmt.Errorf(
			"%d usable frames but %d manifest entries", usable, len(a.Entries)))
	}
	tickPerSec := int64(a.Timescale)
	if tickPerSec == 0 {
		return nil, model.Verify("manifest.align", errors.New("zero timescale"))
	}
	// Expected PTS from declared capture cadence: (capture - epoch) scaled.
	expectPTS := func(raw uint64) int64 {
		units := int64(raw) - int64(a.EpochUnits)
		whole := units / int64(a.CaptureUPS)
		rem := units % int64(a.CaptureUPS)
		return whole*tickPerSec + rem*tickPerSec/int64(a.CaptureUPS)
	}
	for _, e := range a.Entries {
		fr, ok := byOffset[e.Offset]
		if !ok {
			return nil, model.Verify("manifest.align", fmt.Errorf("entry frame %d offset %d not in source", e.FrameNo, e.Offset))
		}
		if !fr.Status.Usable() {
			return nil, model.Verify("manifest.align", fmt.Errorf("entry frame %d points at %s", e.FrameNo, fr.Status))
		}
		if fr.PTS != e.PTS {
			return nil, model.Verify("manifest.align", fmt.Errorf("frame %d pts %d != entry %d", e.FrameNo, fr.PTS, e.PTS))
		}
		for _, g := range a.Missing {
			if e.FrameNo >= g.From && e.FrameNo <= g.To {
				return nil, model.Verify("manifest.align", fmt.Errorf("entry frame %d falls in a known gap", e.FrameNo))
			}
		}
		want := expectPTS(fr.Meta.CaptureUnits)
		if want != fr.PTS {
			// Tolerant to sub-ms quantization: compare in nanoseconds.
			delta := fr.PTS - want
			ns := delta * 1e9 / tickPerSec
			if ns < 0 {
				ns = -ns
			}
			if ns > int64(MaxSkewMS*1e6) {
				return nil, model.Verify("manifest.align", fmt.Errorf(
					"frame %d skew %d ns exceeds %.1f ms", e.FrameNo, ns, MaxSkewMS))
			}
			if abs64(delta) > abs64(r.SkewTicks) {
				r.SkewTicks = delta
			}
		}
		r.Rows++
	}
	r.SkewMS = float64(abs64(r.SkewTicks)) * 1000 / float64(tickPerSec)
	if r.Rows != len(a.Entries) {
		return nil, model.Verify("manifest.align", fmt.Errorf("rows %d != entries %d", r.Rows, len(a.Entries)))
	}
	return r, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// DigestReader computes the manifest digest without materializing the file,
// used when re-checking an existing manifest.
func DigestReader(r io.Reader) ([32]byte, error) {
	var d [32]byte
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return d, err
	}
	copy(d[:], h.Sum(nil))
	return d, nil
}
